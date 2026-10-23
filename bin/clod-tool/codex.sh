# clod tool driver: codex (OpenAI Codex CLI).
#
# Codex is installed from the GitHub release at image build time. The
# version defaults to `latest` (resolved via the GitHub API during the
# build); pin a specific release by writing it to .clod/codex-version
# (e.g. `0.160.1`), which also makes image rebuilds reproducible.
# Editing that file changes the .clod hash, so the image rebuilds
# automatically.
#
# Release tags are `rust-vX.Y.Z` (and `vX.Y.Z` for the npm/standalone
# line); asset names use the bare version X.Y.Z. We install the
# *packaged* release (codex-package-<target>.tar.gz), not the bare
# single-binary archive: the package layout (bin/codex next to a
# codex-package.json manifest, plus codex-path/ and
# codex-resources/bwrap) is what codex's app-server daemon needs to
# find its "complete local package" — running a bare renamed binary
# fails with "this CLI has no complete local package". The package
# lives at /opt/codex/releases/<ver>-<target> in the image;
# /usr/local/bin/codex is a symlink into it, so the daemon's layout
# detection follows the symlink and finds the manifest. Downloads are
# SHA-256 verified against the release metadata.
#
# The package dir deliberately lives outside the bind-mounted
# .clod/codex/ volume: the install RUN step re-runs on every rebuild
# (clod-build-time cache bust) and would otherwise download a fresh
# copy of the package into the mounted volume on every session.
#
# Sandboxing: the container IS the sandbox. Codex's own Linux sandbox
# (bubblewrap) double-sandboxes the clod container and needs user
# namespaces that Docker's default seccomp profile blocks, so by
# default the wrapper runs codex with `--sandbox danger-full-access`
# (no bwrap) while keeping codex's approval prompts. Override
# per-domain with .clod/codex-sandbox:
#   container        (default) inject --sandbox danger-full-access
#   workspace-write  inject that mode + run the container --privileged
#   read-only        inject that mode + run the container --privileged
#   off              inject nothing (you manage --sandbox yourself)
# Passing --sandbox yourself (CLI or .clod/codex-default-flags)
# always wins over the injected default.
#
# ALL of codex's state (config.toml, auth.json, history.jsonl, and
# the sessions/ rollout transcripts that power `codex resume`) lives
# under CODEX_HOME, which the wrapper pins to $HOME/.codex. The
# host-side .clod/codex/ dir is bind-mounted over $HOME/.codex, so a
# single volume mount covers config, credentials, and session
# persistence — `codex resume --last` works across container
# rebuilds.
#
# Authentication: run `clod-codex login` (browser OAuth) or
# `clod-codex login --device-auth` (no local browser; paste the code
# from any machine). Credentials land in .clod/codex/auth.json on the
# host and persist. Auth is shared with the host in both directions
# (see sync_codex_config): the domain inherits a host login if one
# exists, and a login done in a container initializes the host's
# ~/.codex when it has no codex config, so neither side has to log in
# again on the next start.

TOOL_NAME="codex"
TOOL_STATE_DIR=".clod/codex"

# tool_init runs during initialize() before the Dockerfile is
# generated.
tool_init() {
    mkdir -p "$TOOL_STATE_DIR"
}

# tool_sync runs on every clod invocation.
tool_sync() {
    mkdir -p "$TOOL_STATE_DIR"
    sync_codex_config
}

# Internal: is this auth.json file actually usable by codex? ChatGPT
# mode requires tokens (access/refresh); API-key mode requires
# openai_api_key. A file with neither (e.g. a placeholder) still
# parses as ChatGPT auth, and the TUI's account/read bootstrap dies
# on it with "plan type is required for chatgpt authentication", so
# such a file must never be seeded into a domain.
auth_json_usable() {
    local f=$1
    if command -v python3 >/dev/null 2>&1; then
        python3 -c '
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    sys.exit(1)
t = d.get("tokens") or {}
sys.exit(0 if (t.get("access_token") or t.get("refresh_token") or d.get("openai_api_key")) else 1)
' "$f" 2>/dev/null
        return
    fi
    grep -qE '"(access_token|refresh_token|openai_api_key)"[[:space:]]*:[[:space:]]*"[^"]+' "$f" 2>/dev/null
}

# Internal: share codex auth/config between host and domain state dir.
#
# Host -> domain (seed, or repair a broken domain auth): if the host
# has a usable ~/.codex/auth.json and the domain has none or an
# unusable one, copy it in so a domain inherits a real host login.
# A domain auth.json that is already usable is never overwritten — it
# is the per-domain source of truth once present.
#
# Domain -> host (init, not a sync): if the host has NO codex config
# at all (never ran codex), a login performed in the container
# (.clod/codex/auth.json) is installed as the host's ~/.codex so the
# host itself doesn't have to log in again. This is a one-shot init:
# the moment the host's ~/.codex contains anything, host-side state
# is the host's own and clod no longer touches it.
#
# Note: codex on the host may store credentials in the OS keyring
# instead of auth.json (its "auto" storage mode); only a file-based
# host login is shareable. A login done in a container is always
# file-based, so that direction always works.
sync_codex_config() {
    local host_codex="$HOME/.codex"
    if [[ -d "$host_codex" && -f "$host_codex/auth.json" ]] \
       && auth_json_usable "$host_codex/auth.json" \
       && { [[ ! -f "$TOOL_STATE_DIR/auth.json" ]] || ! auth_json_usable "$TOOL_STATE_DIR/auth.json"; }; then
        cp "$host_codex/auth.json" "$TOOL_STATE_DIR/auth.json"
        printf '[clod] codex: seeded %s/auth.json from host %s (replacing missing or unusable copy)\n' "$TOOL_STATE_DIR" "$host_codex" >&2
    fi
    if [[ -d "$host_codex" && -f "$host_codex/config.toml" && ! -f "$TOOL_STATE_DIR/config.toml" ]]; then
        cp "$host_codex/config.toml" "$TOOL_STATE_DIR/config.toml"
        printf '[clod] codex: seeded %s/config.toml from host %s\n' "$TOOL_STATE_DIR" "$host_codex" >&2
    fi
    if [[ -f "$TOOL_STATE_DIR/auth.json" ]] && auth_json_usable "$TOOL_STATE_DIR/auth.json" \
       && { [[ ! -d "$host_codex" ]] || [[ -z "$(ls -A "$host_codex")" ]]; }; then
        mkdir -p "$host_codex"
        cp "$TOOL_STATE_DIR/auth.json" "$host_codex/auth.json"
        [[ -f "$TOOL_STATE_DIR/config.toml" ]] && cp "$TOOL_STATE_DIR/config.toml" "$host_codex/config.toml"
        printf '[clod] codex: initialized host %s from %s/auth.json (login done in container)\n' "$host_codex" "$TOOL_STATE_DIR" >&2
    fi
}

# tool_dockerfile_root_section emits Dockerfile_wrapper directives
# that run as root, *before* the USER switch. Installs the codex
# binary from the GitHub release and writes the codex-wrapper
# entrypoint script.
tool_dockerfile_root_section() {
    # Bake the version pin into an ARG (read host-side). The .clod/ dir
    # is a hidden directory and is EXCLUDED from the docker build
    # context, so the build-time RUN cannot `cat .clod/codex-version`
    # directly — it must come from the Dockerfile itself. Editing the
    # pin file still changes the .clod hash (top-level file), which
    # re-runs initialize() and re-emits this ARG with the new value.
    local pin=""
    if [[ -f .clod/codex-version ]]; then
        pin="$(tr -d '[:space:]' < .clod/codex-version)"
    fi
    printf 'ARG CODEX_PIN="%s"\n' "$pin"
    cat <<'EOF'
# Install Codex from the packaged GitHub release (see driver header
# for why the package, not the bare binary). The version is pinned via
# .clod/codex-version if present (baked into CODEX_PIN by the driver,
# because the .clod/ dir is excluded from the build context),
# otherwise `latest` is resolved through the GitHub API at build time.
#
# The clod-build-time secret (set by .clod/system/build to the
# wall-clock time of the clod invocation that triggered the build)
# is part of this RUN step's cache key, so every rebuild re-runs
# the step; if the target package is already present in the image
# (same ver + arch) it skips the ~160MB download and just re-links.
RUN --mount=type=secret,id=clod-build-time,target=/clod-build-time \
    set -eux; \
    arch="$(uname -m)"; \
    case "$arch" in \
        x86_64)  vendor_target='x86_64-unknown-linux-musl' ;; \
        aarch64) vendor_target='aarch64-unknown-linux-musl' ;; \
        *) echo "unsupported arch: $arch" >&2; exit 1 ;; \
    esac; \
    pkg_asset="codex-package-${vendor_target}.tar.gz"; \
    if [ -n "$CODEX_PIN" ]; then \
        ver="$CODEX_PIN"; \
        meta_url="https://api.github.com/repos/openai/codex/releases/tags/rust-v${ver}"; \
    else \
        meta_url="https://api.github.com/repos/openai/codex/releases/latest"; \
        ver="$(curl -fsSL "$meta_url" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)"; \
        ver="${ver#rust-}"; ver="${ver#v}"; \
    fi; \
    release_dir="/opt/codex/releases/${ver}-${vendor_target}"; \
    if [ -x "$release_dir/bin/codex" ] && [ "$("$release_dir/bin/codex" --version | awk '{print $NF}')" = "$ver" ]; then \
        echo "[clod] codex ${ver} package already in image at ${release_dir}; skipping download" >&2; \
    else \
        # (set +x: keep the ~40KB release JSON out of the xtrace log. \
        # tr -d '\n' normalizes the API's pretty-printed JSON onto one \
        # line — JSON strings never contain literal newlines — so the \
        # asset name+digest match below works regardless of formatting. \
        # Each asset object carries "name" and a "digest" (sha256:...), \
        # with the digest a few fields after the name.) \
        set +x; \
        meta="$(curl -fsSL "$meta_url" | tr -d '\n')"; \
        digest="$(printf '%s\n' "$meta" | awk -v a="$pkg_asset" ' \
            { \
                i = index($0, "\"name\":\"" a "\""); \
                if (i == 0) i = index($0, "\"name\": \"" a "\""); \
                if (i == 0) next; \
                rest = substr($0, i); \
                if (match(rest, /"digest":"sha256:[0-9a-fA-F]{64}"/) || \
                    match(rest, /"digest": "sha256:[0-9a-fA-F]{64}"/)) { \
                    s = substr(rest, RSTART, RLENGTH); \
                    sub(/^.*sha256:/, "", s); \
                    sub(/".*$/, "", s); \
                    print tolower(s); \
                    exit; \
                } \
            }')"; \
        set -x; \
        [ -n "$digest" ] || { echo "[clod] could not find sha256 digest for ${pkg_asset}" >&2; exit 1; }; \
        url="https://github.com/openai/codex/releases/download/rust-v${ver}/${pkg_asset}"; \
        echo "[clod] installing codex ${ver} from ${url}" >&2; \
        curl -fsSL "${url}" -o /tmp/codex-pkg.tar.gz; \
        echo "${digest}  /tmp/codex-pkg.tar.gz" | sha256sum -c -; \
        mkdir -p "$release_dir" /tmp/codex-stage; \
        tar -xzf /tmp/codex-pkg.tar.gz -C /tmp/codex-stage; \
        chmod 0755 /tmp/codex-stage/bin/codex /tmp/codex-stage/bin/codex-code-mode-host /tmp/codex-stage/codex-path/rg; \
        [ -f /tmp/codex-stage/codex-resources/bwrap ] && chmod 0755 /tmp/codex-stage/codex-resources/bwrap; \
        ln -sf bin/codex /tmp/codex-stage/codex; \
        rm -rf "$release_dir"; \
        mv /tmp/codex-stage "$release_dir"; \
        rm -f /tmp/codex-pkg.tar.gz; \
    fi; \
    ln -sfn "releases/${ver}-${vendor_target}" /opt/codex/current; \
    ln -sfn /opt/codex/current/bin/codex /usr/local/bin/codex; \
    /usr/local/bin/codex --version

# codex-wrapper: pins CODEX_HOME to the bind-mounted per-domain state
# dir (auth.json + config.toml + sessions all live there), decides the
# sandbox (see driver header: container-is-the-sandbox by default),
# applies per-domain default flags if present, then execs codex.
COPY <<DEOF /usr/local/bin/codex-wrapper
#!/bin/bash
set -euo pipefail

export CODEX_HOME="\${HOME}/.codex"
mkdir -p "\$CODEX_HOME"

# clod's container is the sandbox. Codex's own bubblewrap sandbox
# needs user namespaces that Docker's default seccomp profile blocks,
# so by default we run codex with --sandbox danger-full-access (no
# bwrap) and keep its approval prompts. .clod/codex-sandbox overrides
# per domain; a --sandbox you supply yourself (CLI or
# .clod/codex-default-flags) always wins and suppresses the default.
cargs=()
have_sandbox=0
for a in "\$@"; do
  if [[ "\$a" == "--sandbox" || "\$a" == --sandbox=* ]]; then
    have_sandbox=1
  fi
done
# Single line (no backslash continuation): BuildKit's unquoted heredoc
# drops a backslash before a newline but keeps the newline, which would
# split an if-condition across lines and leave a dangling &&.
if [[ -f .clod/codex-default-flags ]] && grep -qE '(^|[[:space:]])--sandbox([[:space:]]|=[^[:space:]]+|$)' .clod/codex-default-flags; then
  have_sandbox=1
fi
if [[ "\$have_sandbox" == 0 ]]; then
  mode=""
  if [[ -f .clod/codex-sandbox ]]; then
    mode="\$(tr -d '[:space:]' < .clod/codex-sandbox)"
  fi
  case "\$mode" in
    ''|container|danger-full-access)
      cargs=(--sandbox danger-full-access) ;;
    read-only|workspace-write)
      cargs=(--sandbox "\$mode") ;;
    off)
      cargs=() ;;
    *)
      printf '[clod] codex: unknown .clod/codex-sandbox %q; defaulting to container\n' "\$mode" >&2
      cargs=(--sandbox danger-full-access) ;;
  esac
fi

# The common path (no default-flags file) execs directly so a bare
# `clod-codex` passes exactly its own args to codex. The default-flags
# file is a shell line, so that path uses eval (same as clod-claude /
# clod-crush).
if [[ -f .clod/codex-default-flags ]]; then
  eval "codex \${cargs[*]:-} \$(<.clod/codex-default-flags) \"\$@\""
elif (( \${#cargs[@]} )); then
  codex "\${cargs[@]}" "\$@"
else
  codex "\$@"
fi
DEOF
RUN chmod +x /usr/local/bin/codex-wrapper
EOF
}

# tool_dockerfile_user_install: nothing extra runs as the
# unprivileged user — codex is system-installed at /usr/local/bin/.
tool_dockerfile_user_install() {
    : # no-op
}

tool_dockerfile_entrypoint() {
    printf 'ENTRYPOINT ["codex-wrapper"]\n'
}

# tool_run_volume_args emits the per-tool `-v` lines that go into
# .clod/system/run's `docker run` invocation. The function runs at
# init time and its stdout is interpolated into the run script — so
# `$cwd`, `$user_home` etc. should be left literal here (single-
# quoted heredoc) and will be expanded later when the run script
# itself executes. One mount covers everything: codex keeps all of
# its state under CODEX_HOME (the wrapper sets it to $HOME/.codex).
tool_run_volume_args() {
    cat <<'EOF'
  -v "$cwd/.clod/codex:$user_home/.codex" \
EOF
}

# tool_run_env_args: nothing extra; the wrapper + CODEX_HOME handle
# the rest.
tool_run_env_args() {
    : # no extra env vars for codex
}

# tool_run_extra_flags: emit docker run flags when the domain opts in
# to codex's own bubblewrap sandbox (.clod/codex-sandbox = read-only or
# workspace-write). Those modes need user namespaces, which only work
# in the container with --privileged (Docker's default seccomp blocks
# unshare). The container default needs no extra flag.
tool_run_extra_flags() {
    local mode=""
    if [[ -f .clod/codex-sandbox ]]; then
        mode="$(tr -d '[:space:]' < .clod/codex-sandbox)"
    fi
    case "$mode" in
        read-only|workspace-write)
            printf '  --privileged \\\n'
            ;;
    esac
}

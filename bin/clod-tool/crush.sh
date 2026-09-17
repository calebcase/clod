# clod tool driver: crush (Charm Crush).
#
# Crush is installed from the GitHub release tarball at image build
# time. The version defaults to `latest` (resolved via the GitHub API
# during the build); pin a specific release by writing it to
# .clod/crush-version (e.g. `v0.94.2`), which also makes image
# rebuilds reproducible. Editing that file changes the .clod hash, so
# the image rebuilds automatically.
#
# Crush is pointed at the host's model provider through its normal
# config discovery: the global crush config (~/.config/crush/crushrc
# or crush.json) is seeded into .clod/crush/config/ once and bind-
# mounted over the container user's ~/.config/crush/. A project-local
# .crushrc / .crush.json dropped in the domain directory always wins,
# per crush's own merge rules (project config > global config).
# Provider base_urls that point at the host's loopback are handled
# automatically — see the "Reaching host provider endpoints" note
# below and the README.
#
# Sessions are persisted in .clod/crush/data/ (crush's SQLite session
# DB), so `crush --resume` works across container rebuilds.
#
# Sourced from bin/clod when CLOD_TOOL=crush (or .clod/tool contains
# "crush"; set it once by running `clod-crush` in the directory).

TOOL_NAME="crush"
TOOL_STATE_DIR=".clod/crush"

# Where crush keeps its global config in the container. The host-side
# .clod/crush/config/ dir is bind-mounted here.
CRUSH_CONFIG_DIR="$HOME/.config/crush"

# tool_init runs during initialize() before the Dockerfile is
# generated. Creates the host-side state dirs and seeds the global
# crush config if the container-side copy doesn't exist yet.
tool_init() {
    mkdir -p "$TOOL_STATE_DIR/config" "$TOOL_STATE_DIR/data" "$TOOL_STATE_DIR/cache"
    sync_crush_config
}

# tool_sync runs on every clod invocation. Seeds the global crush
# config if it appeared on the host after init; never overwrites an
# existing copy — that file is the per-domain source of truth once
# present (edit it to change providers/models for this domain).
tool_sync() {
    mkdir -p "$TOOL_STATE_DIR/config" "$TOOL_STATE_DIR/data" "$TOOL_STATE_DIR/cache"
    sync_crush_config
}

# Reaching host provider endpoints.
#
# The container's `localhost` is the container itself, so
# provider base_urls of `://localhost:PORT` in the domain config
# copy are rewritten to `://host.docker.internal:PORT` (see
# fix_host_base_urls), and the run script defines the name via
# --add-host=host.docker.internal:host-gateway (see
# tool_run_extra_flags) — that resolves to the docker bridge
# gateway IP.
#
# Host services bound to the bridge or 0.0.0.0 are then reachable
# directly. Services bound strictly to the host's 127.0.0.1 (e.g.
# an `ssh -L` port forward — common, and keeps the host from
# advertising listening sockets) are NOT reachable over the bridge
# (no loopback hairpin by default), so the run script starts a
# host-side relay per provider port: ncat bound to the bridge
# gateway IP only, forwarding to 127.0.0.1, torn down on exit
# (see tool_run_extra_setup). Host networking is deliberately not
# used: it would defeat much of the network isolation clod exists
# to provide.
fix_host_base_urls() {
    local f
    for f in "$TOOL_STATE_DIR/config/crush.json" "$TOOL_STATE_DIR/config/crushrc"; do
        [[ -f "$f" ]] || continue
        if grep -qE '"base_url"[[:space:]]*:[[:space:]]*"?[^"]*(https?)://localhost:' "$f" 2>/dev/null \
           || grep -qE -- '--base-url[= ]*[^ ]*(https?)://localhost:' "$f" 2>/dev/null; then
            sed -i -E \
                -e 's|("base_url"[[:space:]]*:[[:space:]]*"?)(https?)://localhost:|\1\2://host.docker.internal:|' \
                -e 's|(\-\-base-url[= ]*[^ ]*(https?))://localhost:|\1://host.docker.internal:|' \
                "$f"
            printf '[clod] crush: rewrote localhost provider base_url(s) in %s to host.docker.internal\n' "$f" >&2
        fi
    done
}

# Internal: copy the host's global crush config into the domain state
# dir, once. Mirrors the claude driver's sync_claude_config semantics.
sync_crush_config() {
    local src
    if [[ -f "$CRUSH_CONFIG_DIR/crushrc" ]]; then
        src="$CRUSH_CONFIG_DIR/crushrc"
    elif [[ -f "$CRUSH_CONFIG_DIR/crush.json" ]]; then
        src="$CRUSH_CONFIG_DIR/crush.json"
    else
        printf '[clod] crush: no global config at %s (looking for crushrc or crush.json); using crush defaults. Seed .clod/crush/config/ manually to override.\n' \
            "$CRUSH_CONFIG_DIR" >&2
        return
    fi
    local dst="$TOOL_STATE_DIR/config/$(basename "$src")"
    if [[ ! -f "$dst" ]]; then
        cp "$src" "$dst"
        printf '[clod] crush: seeded %s from %s\n' "$dst" "$src" >&2
    fi
    fix_host_base_urls
}

# tool_dockerfile_root_section emits Dockerfile_wrapper directives
# that run as root, *before* the USER switch. Installs the crush
# binary from the GitHub release and writes the crush-wrapper
# entrypoint script.
tool_dockerfile_root_section() {
    cat <<'EOF'
# Install Crush from the GitHub release. The version is pinned via
# .clod/crush-version if present, otherwise `latest` is resolved
# through the GitHub API at build time. .clod/crush-version is a
# top-level file so it's part of the change-detection hash and a
# version bump rebuilds the image automatically. The release
# tarball nests its files under `crush_<ver>_<arch>/`.
#
# The clod-build-time secret (set by .clod/system/build to the
# wall-clock time of the clod invocation that triggered the build)
# is part of this RUN step's cache key, so every rebuild re-runs
# the install and re-resolves `latest` instead of serving a stale
# cached layer. A non-empty .clod/crush-version always wins, so
# pinned builds stay reproducible.
RUN --mount=type=secret,id=clod-build-time,target=/clod-build-time \
    set -eux; \
    arch="$(uname -m)"; \
    case "$arch" in \
        x86_64)  asset='Linux_x86_64' ;; \
        aarch64) asset='Linux_arm64' ;; \
        *) echo "unsupported arch: $arch" >&2; exit 1 ;; \
    esac; \
    if [ -s .clod/crush-version ]; then \
        ver="$(cat .clod/crush-version)"; \
    else \
        ver="$(curl -fsSL https://api.github.com/repos/charmbracelet/crush/releases/latest | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)"; \
    fi; \
    case "$ver" in v*) tag="$ver";; *) tag="v$ver";; esac; \
    tagnum="${tag#v}"; \
    url="https://github.com/charmbracelet/crush/releases/download/$tag/crush_${tagnum}_$asset.tar.gz"; \
    echo "[clod] installing crush $tag from $url" >&2; \
    mkdir -p /tmp/crush-extract; \
    curl -fsSL "$url" | tar -xz -C /tmp/crush-extract; \
    cp "/tmp/crush-extract/crush_${tagnum}_${asset}/crush" /usr/local/bin/crush; \
    chmod +x /usr/local/bin/crush; \
    rm -rf /tmp/crush-extract; \
    /usr/local/bin/crush --version

# crush-wrapper: applies per-domain default flags (optional) then
# execs crush. Crush's own config discovery handles providers, MCP,
# and permissions; the entrypoint stays deliberately thin.
COPY <<DEOF /usr/local/bin/crush-wrapper
#!/bin/bash
set -euo pipefail

if [[ -f .clod/crush-default-flags ]]; then
  eval "crush \$(<.clod/crush-default-flags) \"\$@\""
else
  crush "\$@"
fi
DEOF
RUN chmod +x /usr/local/bin/crush-wrapper
EOF
}

# tool_dockerfile_user_install: nothing extra runs as the
# unprivileged user — crush is system-installed at /usr/local/bin/.
tool_dockerfile_user_install() {
    : # no-op
}

tool_dockerfile_entrypoint() {
    printf 'ENTRYPOINT ["crush-wrapper"]\n'
}

# tool_run_volume_args emits the per-tool `-v` lines that go into
# .clod/system/run's `docker run` invocation. The function runs at
# init time and its stdout is interpolated into the run script — so
# `$cwd`, `$user_home` etc. should be left literal here (single-
# quoted heredoc) and will be expanded later when the run script
# itself executes.
tool_run_volume_args() {
    cat <<'EOF'
  -v "$cwd/.clod/crush/config:$user_home/.config/crush" \
  -v "$cwd/.clod/crush/data:$user_home/.local/share/crush" \
  -v "$cwd/.clod/crush/cache:$user_home/.cache/crush" \
EOF
}

# tool_run_env_args: nothing extra; the wrapper script + crush's
# config discovery handle the rest.
tool_run_env_args() {
    : # no extra env vars for crush
}

# tool_run_extra_flags: extra `docker run` flags emitted before the
# container name. host.docker.internal is defined on Docker Desktop
# for free but not on Linux docker, so we add it explicitly; it
# resolves to the bridge gateway IP, which is where the host-side
# relays (tool_run_extra_setup) listen for loopback-bound providers.
tool_run_extra_flags() {
    cat <<'EOF'
  --add-host=host.docker.internal:host-gateway \
EOF
}

# tool_run_extra_setup: shell lines baked into the run script just
# before the docker invocation (runs on the host). For each provider
# port referenced by the domain crush config, start a host-side relay
# that forwards container traffic to the host's 127.0.0.1, so
# providers bound strictly to loopback (ssh port forwards, local
# vLLM/prodia instances) are reachable without host networking.
tool_run_extra_setup() {
    cat <<'EOF'
# Host-side loopback relays for the crush provider endpoints.
clod_relay_pids=()
start_crush_relays() {
  local cfg=".clod/crush/config/crush.json"
  [[ -f "$cfg" ]] || return
  local ports
  ports=$(grep -oE '"base_url"[[:space:]]*:[[:space:]]*"[^"]*"' "$cfg" \
      | grep -oE 'https?://[^/"]+' \
      | grep -oE '[0-9]+$' | sort -u)
  [[ -n "$ports" ]] || return
  command -v ncat >/dev/null 2>&1 || {
    printf '[clod] crush: ncat not found on host; providers bound to host 127.0.0.1 (ports: %s) will not be reachable from the container\n' "$(echo $ports | tr '\n' ' ')" >&2
    return
  }
  local gw
  gw=$(docker network inspect bridge --format '{{range .IPAM.Config}}{{.Gateway}}{{end}}' 2>/dev/null || true)
  [[ -n "$gw" ]] || {
    printf '[clod] crush: could not determine the docker bridge gateway; skipping relays\n' >&2
    return
  }
  local port
  for port in $ports; do
    # The relay binds to the bridge gateway IP only — the host keeps
    # its loopback-only binding, and the gateway address is not
    # routable from the LAN.
    ncat -lk "$gw" "$port" --sh-exec "ncat 127.0.0.1 $port" >/dev/null 2>&1 &
    clod_relay_pids+=("$!")
    printf '[clod] crush: relaying container port %s -> host 127.0.0.1:%s\n' "$port" "$port" >&2
  done
}
stop_crush_relays() {
  local pid
  for pid in "${clod_relay_pids[@]:-}"; do
    [[ -n "$pid" ]] || continue
    # Kill the listener and its per-connection children. (Not
    # kill -- -$pid: without job control the child shares this
    # script's process group, so a group kill would kill the run
    # script too.)
    pkill -P "$pid" 2>/dev/null || true
    kill "$pid" 2>/dev/null || true
  done
}
clod_tool_cleanup() { stop_crush_relays; }
start_crush_relays
EOF
}

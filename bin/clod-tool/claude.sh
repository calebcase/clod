# clod tool driver: claude (Anthropic Claude Code).
#
# Drivers expose a small set of bash functions that bin/clod calls at
# init time and again at run-script generation time. See
# bin/clod-tool/README.md (and the top of bin/clod) for the contract.
#
# Sourced from bin/clod when CLOD_TOOL=claude (or .clod/tool contains
# "claude"; that's the project default if neither is set).

TOOL_NAME="claude"
TOOL_STATE_DIR=".clod/claude"

# Seed .clod/claude/claude.json from the host config if it isn't already
# populated. Runs on every invocation, not just first-time init, so that a
# user who authenticates on the host *after* .clod was created still gets
# their credentials picked up on the next run. Never overwrites an existing
# file — the bot persists permission grants there and we must not clobber
# them.
sync_claude_config() {
    mkdir -p "$TOOL_STATE_DIR"
    if [[ ! -f "$TOOL_STATE_DIR/claude.json" && -f "$config" ]]; then
        cp "$config" "$TOOL_STATE_DIR/claude.json"
    fi
}

# tool_init runs during initialize() before the Dockerfile is
# generated. Sets up host-side state for the tool — for claude that's
# the .clod/claude/ state dir.
tool_init() {
    mkdir -p "$TOOL_STATE_DIR"
    sync_claude_config
}

# tool_sync runs on every clod invocation (not just init) so that a
# user who authenticates on the host *after* .clod was created still
# gets their credentials picked up on the next run.
tool_sync() {
    sync_claude_config
}

# tool_dockerfile_root_section emits Dockerfile_wrapper directives
# that run as root, *before* the USER switch. Used to drop the
# wrapper script into /usr/bin/.
tool_dockerfile_root_section() {
    cat <<'EOF'
COPY <<DEOF /usr/bin/claude-wrapper
#!/bin/bash
set -euo pipefail

if [[ -f ~/.claude/claude.json ]]; then
  cp ~/.claude/claude.json ~/.claude.json
fi

# Note: We intentionally do NOT copy ~/.claude.json back on exit.
# The bot saves permissions directly to ~/.claude/claude.json (via volume mount),
# and copying back would overwrite those saves with the stale copy made at startup.

# Start clodproxy if the bot dropped it in the runtime dir. It's a
# tiny reverse-proxy that terminates idle keep-alive connections to
# api.anthropic.com aggressively — workaround for upstream claude-
# code #54434 (Node undici pool holds silently half-closed sockets
# between turns and wedges on next request). If the binary isn't
# present we just skip it and go direct; nothing else about the
# session should change.
if [[ -n "\${CLOD_RUNTIME_DIR:-}" && -x "\${CLOD_RUNTIME_DIR}/clodproxy" && -z "\${CLOD_DISABLE_PROXY:-}" ]]; then
  "\${CLOD_RUNTIME_DIR}/clodproxy" >&2 &
  # Wait up to 3s for the proxy to bind. Uses bash's /dev/tcp
  # builtin so we don't require curl/nc in the base image.
  proxy_up=0
  for i in \$(seq 1 30); do
    if (exec 3<>/dev/tcp/127.0.0.1/8788) 2>/dev/null; then
      exec 3>&-; exec 3<&-
      proxy_up=1
      break
    fi
    sleep 0.1
  done
  if (( proxy_up )); then
    # Two-pronged routing:
    #  - ANTHROPIC_BASE_URL: the SDK sends plain HTTP to loopback,
    #    hitting our reverse-proxy path — gives us per-request
    #    tracing for the main /v1/messages calls.
    #  - HTTPS_PROXY: every OTHER outbound HTTPS (statsig, sentry,
    #    docs.anthropic.com, etc.) is intercepted by claude-code
    #    per its documented corporate-proxy support and CONNECT-
    #    tunneled through us — gives us idle-timeout enforcement
    #    so the between-turn CLOSE_WAIT class of wedges can't
    #    accumulate on any endpoint. Discovered from eagle
    #    2026-07-12 13:19Z wedge on a Datadog / Statsig-adjacent IP
    #    that was NOT api.anthropic.com and thus bypassed the
    #    reverse-proxy path.
    #  - NO_PROXY: don't proxy loopback (matters for permbridge /
    #    schedbridge / in-container health checks).
    export ANTHROPIC_BASE_URL="http://127.0.0.1:8788/"
    export HTTPS_PROXY="http://127.0.0.1:8788/"
    export HTTP_PROXY="http://127.0.0.1:8788/"
    export NO_PROXY="localhost,127.0.0.1,::1"
    echo "[wrapper] clodproxy healthy — routing api.anthropic.com direct + all other HTTPS via CONNECT tunnel" >&2
  else
    echo "[wrapper] clodproxy failed to come up; falling back to direct Anthropic" >&2
  fi
fi

# Read default flags from .clod/claude-default-flags if it exists.
if [[ -f .clod/claude-default-flags ]]; then
  eval "claude \$(<.clod/claude-default-flags) \"\$@\""
else
  claude "\$@"
fi
DEOF
RUN chmod u+x /usr/bin/claude-wrapper
EOF
}

# tool_dockerfile_user_install emits Dockerfile_wrapper directives
# that run as the unprivileged user, *after* the USER switch. Used to
# install the tool's user-local binary (claude code's native installer
# drops it into ~/.local/bin/claude). Version pinned so container
# rebuilds are reproducible and so we can roll forward deliberately
# when we've read the changelog. Upstream install.sh takes 'stable',
# 'latest', or a MAJOR.MINOR.PATCH string as the first positional arg.
# Bumped 2026-09-02: 2.1.217 → 2.1.258 (latest) to roll forward to
# the current release.
tool_dockerfile_user_install() {
    cat <<'EOF'
# Install Claude Code using native installer. Version pinned so
# container rebuilds are reproducible and so we can roll forward
# deliberately when we've read the changelog. Upstream install.sh
# takes 'stable', 'latest', or a MAJOR.MINOR.PATCH string as the
# first positional arg. Bumped 2026-09-02: 2.1.217 → 2.1.258
# (latest) to roll forward to the current release.
RUN curl -fsSL https://claude.ai/install.sh | bash -s 2.1.258
EOF
}

# tool_dockerfile_entrypoint emits the ENTRYPOINT line for
# Dockerfile_wrapper. The wrapper script (written in the root
# section) is the canonical entrypoint so per-task default flags +
# claude.json bootstrap happen in one place.
tool_dockerfile_entrypoint() {
    printf 'ENTRYPOINT ["claude-wrapper"]\n'
}

# tool_run_volume_args emits the per-tool `-v` lines that go into
# .clod/system/run's `docker run` invocation. The function runs at
# init time and its stdout is interpolated into the run script — so
# `$cwd`, `$user_home` etc. should be left literal here (single-
# quoted heredoc) and will be expanded later when the run script
# itself executes.
tool_run_volume_args() {
    cat <<'EOF'
  -v "$cwd/.clod/claude:$user_home/.claude" \
EOF
}

# tool_run_env_args emits per-tool `-e` env-var lines for the run
# script's docker invocation. claude doesn't need any beyond the
# common ones bin/clod already sets.
tool_run_env_args() {
    : # no extra env vars for claude
}

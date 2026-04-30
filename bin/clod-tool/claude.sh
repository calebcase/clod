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

# tool_init runs during initialize() *before* the Dockerfile is
# generated. Sets up host-side state for the tool — for claude that's
# seeding ~/.claude.json into .clod/claude/ so the in-container
# wrapper can copy it onto the user's ~/.claude/ at startup.
tool_init() {
    mkdir -p "$TOOL_STATE_DIR"
    sync_claude_config
}

# tool_sync runs on every clod invocation (not just init) so that a
# user who authenticates on the host *after* .clod was created still
# gets their credentials picked up on the next run. Never overwrites
# an existing file — the bot persists permission grants there and we
# must not clobber them.
tool_sync() {
    sync_claude_config
}

# Internal: copy host's ~/.claude.json to .clod/claude/claude.json
# only when the project copy is missing.
sync_claude_config() {
    mkdir -p "$TOOL_STATE_DIR"
    if [[ ! -f "$TOOL_STATE_DIR/claude.json" && -f "$config" ]]; then
        cp "$config" "$TOOL_STATE_DIR/claude.json"
    fi
}

# tool_dockerfile_root_section emits Dockerfile_wrapper directives
# that run as root, *before* the USER switch. Used to drop the
# wrapper script into /usr/bin/ and to install any system packages
# the tool needs.
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
# drops it into ~/.local/bin/claude).
tool_dockerfile_user_install() {
    cat <<'EOF'
# Install Claude Code using the official native installer.
RUN curl -fsSL https://claude.ai/install.sh | bash
EOF
}

# tool_dockerfile_entrypoint emits the ENTRYPOINT line for
# Dockerfile_wrapper. The wrapper script (written above) is the
# canonical entrypoint so per-task default flags + claude.json
# bootstrap happen in one place.
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

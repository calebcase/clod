# clod tool driver: crush (Charm's open-source coding agent).
#
# Crush mode is local-first: an Ollama daemon runs inside the
# container, models are cached on the host (~/.cache/clod/ollama/) so
# they survive container rebuilds and are shared across every clod
# domain on this machine, and a Qwen2.5-Coder model size is auto-
# selected from the largest GPU's VRAM at init time.
#
# Sourced from bin/clod when CLOD_TOOL=crush or .clod/tool contains
# "crush".

TOOL_NAME="crush"
TOOL_STATE_DIR=".clod/crush"

# Where the host caches Ollama model blobs. Shared across all clod
# domains so a 20 GB model only ever downloads once on this machine.
# Bind-mounted into the container at $USER_HOME/.ollama/.
OLLAMA_HOST_CACHE="${HOME}/.cache/clod/ollama"

# Qwen2.5-Coder via Ollama is the default coding-tuned local model
# family. Sizes (Q4_K_M unless noted) and approximate disk/VRAM:
#   3b  ~ 2.0 GB
#   7b  ~ 4.7 GB
#   14b ~ 9.0 GB
#   32b ~ 20  GB (default Q4_K_M)
#
# Higher-precision quants (q8_0, fp16) are pulled when the GPU has
# the headroom — they noticeably improve coding quality. Edit
# .clod/crush/model after init to override.
pick_default_model_for_vram() {
    local vram_mib="$1"
    local vram_gib=$(( vram_mib / 1024 ))
    if   (( vram_gib >= 80 )); then printf 'qwen2.5-coder:32b-instruct-fp16\n'
    elif (( vram_gib >= 40 )); then printf 'qwen2.5-coder:32b-instruct-q8_0\n'
    elif (( vram_gib >= 24 )); then printf 'qwen2.5-coder:32b\n'
    elif (( vram_gib >= 12 )); then printf 'qwen2.5-coder:14b\n'
    elif (( vram_gib >=  6 )); then printf 'qwen2.5-coder:7b\n'
    elif (( vram_gib >=  3 )); then printf 'qwen2.5-coder:3b\n'
    else                            printf 'qwen2.5-coder:3b\n'  # CPU fallback (slow)
    fi
}

# Probe the host's largest single GPU's VRAM. Returns 0 when no
# GPU / nvidia-smi is available; the model picker treats that as
# "tiny CPU-only".
detect_largest_gpu_vram_mib() {
    if ! command -v nvidia-smi >/dev/null 2>&1; then
        printf '0\n'
        return
    fi
    local mib
    mib=$(nvidia-smi --query-gpu=memory.total --format=csv,noheader,nounits 2>/dev/null \
        | sort -n | tail -1)
    printf '%s\n' "${mib:-0}"
}

# tool_init runs during initialize() before the Dockerfile is
# generated. Picks a default model based on host VRAM (one-time;
# overridable by editing .clod/crush/model afterwards), seeds an
# initial crush.json pointing crush at the in-container Ollama, and
# ensures the host-side Ollama model cache exists so the bind mount
# in .clod/system/run doesn't fail with "no such file".
tool_init() {
    mkdir -p "$TOOL_STATE_DIR/config" "$TOOL_STATE_DIR/data" "$TOOL_STATE_DIR/cache"
    mkdir -p "$OLLAMA_HOST_CACHE"

    # Default model: pick once based on host hardware. Don't clobber
    # an existing pick — the user may have overridden it.
    if [[ ! -f "$TOOL_STATE_DIR/model" ]]; then
        local vram
        vram=$(detect_largest_gpu_vram_mib)
        local model
        model=$(pick_default_model_for_vram "$vram")
        printf '%s\n' "$model" > "$TOOL_STATE_DIR/model"
        printf '[clod] crush: detected %s MiB VRAM, defaulting to model %s\n' "$vram" "$model" >&2
        printf '[clod] crush: edit .clod/crush/model to override\n' >&2
    fi

    # Seed crush.json with an Ollama provider pointed at the in-
    # container daemon. Crush picks the provider listed first as the
    # default; only one provider here so it's unambiguous. Don't
    # clobber an existing config — the user may have customised it.
    if [[ ! -f "$TOOL_STATE_DIR/config/crush.json" ]]; then
        local model
        model=$(<"$TOOL_STATE_DIR/model")
        cat > "$TOOL_STATE_DIR/config/crush.json" <<JSON
{
  "\$schema": "https://charm.land/crush.json",
  "providers": {
    "ollama": {
      "id": "ollama",
      "name": "Ollama (local)",
      "type": "openai",
      "base_url": "http://127.0.0.1:11434/v1",
      "api_key": "ollama",
      "models": [
        {
          "id": "$model",
          "name": "$model",
          "context_window": 32768,
          "default_max_tokens": 4096,
          "supports_images": false
        }
      ]
    }
  }
}
JSON
        printf '[clod] crush: wrote initial config %s\n' \
            "$TOOL_STATE_DIR/config/crush.json" >&2
    fi
}

# tool_sync runs on every clod invocation. For crush there's no host
# credential to mirror (everything runs locally), but we make sure
# the Ollama cache dir still exists so a manual `rm` doesn't break
# the next run.
tool_sync() {
    mkdir -p "$OLLAMA_HOST_CACHE"
    mkdir -p "$TOOL_STATE_DIR/config" "$TOOL_STATE_DIR/data" "$TOOL_STATE_DIR/cache"
}

# tool_dockerfile_root_section: install Ollama + Crush as root, write
# the crush-wrapper entrypoint script. Both binaries are dropped into
# /usr/local/bin/ so the user doesn't need write access to install
# them, and the entrypoint script picks them up via PATH.
tool_dockerfile_root_section() {
    cat <<'EOF'
# Install Ollama (local LLM runtime). The official installer ships
# the binary directly to /usr/local/bin/ollama and tries to set up
# systemd; the systemd part is best-effort and harmlessly fails in a
# container. We start `ollama serve` ourselves from the entrypoint
# script.
RUN curl -fsSL https://ollama.com/install.sh | sh \
 || (echo "[clod] ollama installer reported a non-fatal error; continuing" >&2; \
     test -x /usr/local/bin/ollama)

# Install Crush from the latest GitHub release. Uses uname -m to
# pick the right tarball (amd64 / arm64). The single static Go
# binary lands at /usr/local/bin/crush.
RUN set -eux; \
    arch="$(uname -m)"; \
    case "$arch" in \
        x86_64)  asset=Linux_x86_64 ;; \
        aarch64) asset=Linux_arm64 ;; \
        *) echo "unsupported arch: $arch" >&2; exit 1 ;; \
    esac; \
    url="$(curl -fsSL https://api.github.com/repos/charmbracelet/crush/releases/latest \
        | grep -oE "https://[^\"]*${asset}\\.tar\\.gz" | head -1)"; \
    test -n "$url"; \
    curl -fsSL "$url" | tar -xz -C /usr/local/bin/ crush; \
    chmod +x /usr/local/bin/crush

# crush-wrapper: boots Ollama in the background, waits for it to be
# ready, pulls the configured model on first run, then runs crush in
# the foreground. The model name is read from the config dir bind-
# mounted from .clod/crush/config/model, so a model swap on the host
# takes effect on the next clod invocation without rebuilding the
# image.
COPY <<DEOF /usr/local/bin/crush-wrapper
#!/bin/bash
set -euo pipefail

ollama_host="\${OLLAMA_HOST:-127.0.0.1:11434}"
ollama_url="http://\$ollama_host"
log=/tmp/ollama.log

# Start ollama if nothing's already listening (host-Ollama mode would
# set OLLAMA_HOST to point at the host daemon, in which case we just
# use it).
if ! curl -sSf "\$ollama_url/api/tags" >/dev/null 2>&1; then
  printf '[clod] starting ollama (logs: %s)\n' "\$log" >&2
  ollama serve >"\$log" 2>&1 &
  for i in \$(seq 1 60); do
    if curl -sSf "\$ollama_url/api/tags" >/dev/null 2>&1; then
      printf '[clod] ollama ready after %ss\n' "\$i" >&2
      break
    fi
    sleep 1
  done
  if ! curl -sSf "\$ollama_url/api/tags" >/dev/null 2>&1; then
    printf '[clod] ERROR: ollama did not start within 60s; last 40 log lines:\n' >&2
    tail -40 "\$log" >&2 || true
    exit 1
  fi
fi

# Resolve the configured model and pull it on first run. The crush
# config volume mount lands at \$HOME/.config/crush/, so the model
# pointer the host wrote to .clod/crush/model lives at
# \$HOME/.config/crush/model.
model_file="\$HOME/.config/crush/model"
if [[ -f "\$model_file" ]]; then
  model=\$(<"\$model_file")
else
  model="qwen2.5-coder:7b"
fi

if ! curl -sSf "\$ollama_url/api/tags" \
     | grep -qE "\"name\":\"\${model}\"|\"model\":\"\${model}\""; then
  printf '[clod] pulling %s (first run on this host; this can take a while)\n' "\$model" >&2
  ollama pull "\$model"
fi

exec crush "\$@"
DEOF
RUN chmod +x /usr/local/bin/crush-wrapper
EOF
}

# tool_dockerfile_user_install: nothing extra runs as the
# unprivileged user — both crush and ollama are system-installed at
# /usr/local/bin/.
tool_dockerfile_user_install() {
    : # no-op
}

tool_dockerfile_entrypoint() {
    printf 'ENTRYPOINT ["crush-wrapper"]\n'
}

# tool_run_volume_args: bind mounts that go into .clod/system/run's
# docker invocation. Layout (host → container):
#   .clod/crush/config → ~/.config/crush      (crush config + model pointer)
#   .clod/crush/data   → ~/.local/share/crush (sessions DB, history)
#   .clod/crush/cache  → ~/.cache/crush       (small caches)
#   ~/.cache/clod/ollama → ~/.ollama          (model blobs, host-shared)
#
# `$cwd`, `$user_home`, `$ollama_host_cache` are left literal here
# because the function output is baked into the run script and
# expanded when that script runs. `$ollama_host_cache` is exported
# from bin/clod's run-script generator so it points at the same
# OLLAMA_HOST_CACHE the host computed at init time.
tool_run_volume_args() {
    cat <<'EOF'
  -v "$cwd/.clod/crush/config:$user_home/.config/crush" \
  -v "$cwd/.clod/crush/data:$user_home/.local/share/crush" \
  -v "$cwd/.clod/crush/cache:$user_home/.cache/crush" \
  -v "$ollama_host_cache:$user_home/.ollama" \
EOF
}

# tool_run_env_args: nothing extra; the wrapper script reads its
# config from the bind-mounted ~/.config/crush/ tree.
tool_run_env_args() {
    : # no extra env vars for crush
}

# tool_run_extra_setup: emitted into .clod/system/run before the
# docker invocation. crush needs the OLLAMA_HOST_CACHE path computed
# the same way it was at init time, so the run script and the bind
# mount line up.
tool_run_extra_setup() {
    cat <<'EOF'
ollama_host_cache="${HOME}/.cache/clod/ollama"
mkdir -p "$ollama_host_cache"
EOF
}

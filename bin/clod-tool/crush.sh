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

# Default-model picker.
#
# Qwen3-Coder is the default family because Crush is an *agentic*
# tool — it expects the model to issue structured tool calls (Bash,
# file edits, etc.) rather than narrate commands in prose. Qwen2.5-
# Coder generates code well but isn't reliably tuned for the
# function-call protocol Crush drives tools through, which surfaces
# as the model echoing a `bash some-cmd` line in plain text instead
# of actually running it. Qwen3-Coder was built specifically for
# this agentic shape and is the closest open-weight analogue to
# Claude Code today.
#
# Tag sizes (Ollama default Q4_K_M unless noted):
#   qwen3-coder:480b   ~ 270 GB  (480B MoE, 35B active — only fits
#                                 on hardware with hundreds of GB
#                                 single-GPU VRAM; closest to a
#                                 Claude-shaped experience)
#   qwen3-coder:30b    ~  17 GB  (30B MoE, 3B active — sweet spot:
#                                 fast, capable, fits on a single
#                                 24 GB GPU)
#   qwen3:8b           ~   5 GB  (general-purpose Qwen3, decent
#                                 tool-calling, falls back when no
#                                 GPU has room for qwen3-coder:30b)
#   qwen3:4b           ~   2.5 GB
#   qwen3:1.7b         ~   1.4 GB (CPU fallback — agentic UX will
#                                  feel slow / unreliable, but at
#                                  least the stack starts)
#
# Edit .clod/crush/config/model after init to override.
pick_default_model_for_vram() {
    local vram_mib="$1"
    local vram_gib=$(( vram_mib / 1024 ))
    if   (( vram_gib >= 256 )); then printf 'qwen3-coder:480b\n'
    elif (( vram_gib >=  16 )); then printf 'qwen3-coder:30b\n'
    elif (( vram_gib >=   6 )); then printf 'qwen3:8b\n'
    elif (( vram_gib >=   3 )); then printf 'qwen3:4b\n'
    else                             printf 'qwen3:1.7b\n'  # CPU fallback
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

# Render crush.json with the configured model and the in-container
# Ollama provider. The model id appears twice (id + name) because
# crush keys lookups by id but displays name in the picker.
#
# `crush.json` is treated as a derived artifact of `.clod/crush/
# config/model`: the wrapper relies on the embedded model id matching
# what `ollama pull` was told to fetch, so edits to the id field on
# the host don't survive a regen. Other fields (context_window,
# default_max_tokens, supports_images) are preserved across runs by
# `_ensure_crush_json_matches_model`, which only regenerates when
# the embedded model id has drifted from the `model` file.
_render_crush_json() {
    local model="$1"
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
}

# Idempotent crush.json regen. Reads the current model from
# .clod/crush/config/model and rewrites crush.json only if its
# embedded model id doesn't already match. Called on every invocation
# from tool_sync so a host-side `echo qwen3-coder:30b > model` takes
# effect on the next clod run without requiring CLOD_REINIT.
_ensure_crush_json_matches_model() {
    local cfg="$TOOL_STATE_DIR/config/crush.json"
    local model
    model=$(<"$TOOL_STATE_DIR/config/model")
    if [[ -f "$cfg" ]] && grep -q "\"id\": *\"$model\"" "$cfg"; then
        return
    fi
    _render_crush_json "$model"
    printf '[clod] crush: regenerated %s for model=%s\n' "$cfg" "$model" >&2
}

# tool_init runs during initialize() before the Dockerfile is
# generated. Picks a default model based on host VRAM (one-time;
# overridable by editing .clod/crush/config/model afterwards), seeds
# crush.json pointing crush at the in-container Ollama, and ensures
# the host-side Ollama model cache exists so the bind mount in
# .clod/system/run doesn't fail with "no such file".
tool_init() {
    mkdir -p "$TOOL_STATE_DIR/config" "$TOOL_STATE_DIR/data" "$TOOL_STATE_DIR/cache"
    mkdir -p "$OLLAMA_HOST_CACHE"

    # Default model: pick once based on host hardware. Don't clobber
    # an existing pick — the user may have overridden it. Lives under
    # config/ so the bind-mount that maps `.clod/crush/config` →
    # container `$HOME/.config/crush/` lands the file where the
    # crush-wrapper expects it (`$HOME/.config/crush/model`).
    if [[ ! -f "$TOOL_STATE_DIR/config/model" ]]; then
        local vram
        vram=$(detect_largest_gpu_vram_mib)
        local model
        model=$(pick_default_model_for_vram "$vram")
        printf '%s\n' "$model" > "$TOOL_STATE_DIR/config/model"
        printf '[clod] crush: detected %s MiB VRAM, defaulting to model %s\n' "$vram" "$model" >&2
        printf '[clod] crush: edit .clod/crush/config/model to override\n' >&2
    fi

    # Always regenerate crush.json from the current model. Under
    # CLOD_REINIT this whole function reruns; we want the config to
    # reflect any model edits the user made between init runs. Use
    # the plain renderer (not _ensure_) so any out-of-band hand-edits
    # to crush.json get reset — reinit means "rebuild from scratch".
    local model
    model=$(<"$TOOL_STATE_DIR/config/model")
    _render_crush_json "$model"
    printf '[clod] crush: wrote %s for model=%s\n' \
        "$TOOL_STATE_DIR/config/crush.json" "$model" >&2
}

# tool_sync runs on every clod invocation. For crush we ensure
# crush.json is in sync with the model file (catches model edits
# that didn't go through CLOD_REINIT), and reassert the host-side
# state dirs in case a manual `rm` removed any of them.
tool_sync() {
    mkdir -p "$OLLAMA_HOST_CACHE"
    mkdir -p "$TOOL_STATE_DIR/config" "$TOOL_STATE_DIR/data" "$TOOL_STATE_DIR/cache"
    _ensure_crush_json_matches_model
}

# tool_dockerfile_root_section: install Ollama + Crush as root, write
# the crush-wrapper entrypoint script. Both binaries are dropped into
# /usr/local/bin/ so the user doesn't need write access to install
# them, and the entrypoint script picks them up via PATH.
tool_dockerfile_root_section() {
    cat <<'EOF'
# zstd is required to unpack the Ollama release tarball (the upstream
# archive uses zstd-compressed layers). ca-certificates is already in
# Dockerfile_base; we add zstd here in the crush-only path so claude
# images don't pay for it.
RUN --mount=type=cache,sharing=locked,target=/var/cache/apt \
    --mount=type=cache,sharing=locked,target=/var/lib/apt \
    apt-get update \
 && apt-get install -qq -y zstd

# Install Ollama (local LLM runtime).
#
# We deliberately do NOT use the upstream `curl ... install.sh | sh`
# convenience installer here: after dropping the binary it goes on to
# add an `ollama` system user, set up a `ollama.service` systemd
# unit, and try to start it. In a Docker build there's no systemd,
# but the installer doesn't fail fast on that — it hangs at "Install
# complete. Run \"ollama\" from the command line." and never returns,
# so the entire docker build stalls.
#
# Direct binary fetch is simpler and faster: pull the official
# release tarball, extract `bin/ollama` (and `lib/*` for GPU runner
# shared objects) into /usr/local/, done. The releases are
# `<arch>.tar.zst` (renamed from .tgz somewhere around v0.6 — old
# install.sh callers see the zstd-extraction error then hang on the
# systemd setup). We hand tar `--zstd` explicitly.
RUN set -eux; \
    arch="$(uname -m)"; \
    case "$arch" in \
        x86_64)  asset=ollama-linux-amd64.tar.zst ;; \
        aarch64) asset=ollama-linux-arm64.tar.zst ;; \
        *) echo "unsupported arch: $arch" >&2; exit 1 ;; \
    esac; \
    url="https://github.com/ollama/ollama/releases/latest/download/$asset"; \
    curl -fsSL "$url" -o /tmp/ollama.tar.zst; \
    mkdir -p /tmp/ollama-extract; \
    tar --zstd -xf /tmp/ollama.tar.zst -C /tmp/ollama-extract; \
    cp /tmp/ollama-extract/bin/ollama /usr/local/bin/ollama; \
    chmod +x /usr/local/bin/ollama; \
    if [ -d /tmp/ollama-extract/lib ]; then \
        mkdir -p /usr/local/lib; \
        cp -r /tmp/ollama-extract/lib/* /usr/local/lib/; \
    fi; \
    rm -rf /tmp/ollama.tar.zst /tmp/ollama-extract; \
    /usr/local/bin/ollama --version

# Install Crush from the latest GitHub release. The release tarball
# nests its files under `crush_<ver>_<arch>/`, so we extract to a
# temp dir and pull the binary out rather than expecting it at the
# tarball root. The .sbom.json siblings in the release would also
# match a naive Linux_x86_64*.tar.gz pattern; the regex below stops
# at the first .tar.gz boundary which gives us the binary tarball
# and not its checksum/sbom companions.
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
    mkdir -p /tmp/crush-extract; \
    curl -fsSL "$url" | tar -xz -C /tmp/crush-extract; \
    cp /tmp/crush-extract/*/crush /usr/local/bin/crush; \
    chmod +x /usr/local/bin/crush; \
    rm -rf /tmp/crush-extract; \
    /usr/local/bin/crush --version

# crush-wrapper: boots Ollama in the background, waits for it to be
# ready, pulls the configured model on first run, then runs crush in
# the foreground. The model name is read from the config dir bind-
# mounted from .clod/crush/config/model, so a model swap on the host
# takes effect on the next clod invocation without rebuilding the
# image.
#
# `<<"DEOF"` (quoted) tells Docker BuildKit to treat the heredoc body
# verbatim: no variable expansion, no backslash-escape processing.
# That keeps `\n` in printf format strings intact, keeps the grep
# alternation `"name":"..."|"model":"..."` quote-matching, and lets
# us write `$var` for runtime expansion without doubling backslashes.
COPY <<"DEOF" /usr/local/bin/crush-wrapper
#!/bin/bash
set -euo pipefail

ollama_host="${OLLAMA_HOST:-127.0.0.1:11434}"
ollama_url="http://$ollama_host"
log=/tmp/ollama.log

# Start ollama if nothing's already listening (host-Ollama mode would
# set OLLAMA_HOST to point at the host daemon, in which case we just
# use it).
if ! curl -sSf "$ollama_url/api/tags" >/dev/null 2>&1; then
  printf '[clod] starting ollama (logs: %s)\n' "$log" >&2
  ollama serve >"$log" 2>&1 &
  for i in $(seq 1 60); do
    if curl -sSf "$ollama_url/api/tags" >/dev/null 2>&1; then
      printf '[clod] ollama ready after %ss\n' "$i" >&2
      break
    fi
    sleep 1
  done
  if ! curl -sSf "$ollama_url/api/tags" >/dev/null 2>&1; then
    printf '[clod] ERROR: ollama did not start within 60s; last 40 log lines:\n' >&2
    tail -40 "$log" >&2 || true
    exit 1
  fi
fi

# Resolve the configured model and pull it on first run. The crush
# config volume mount lands at $HOME/.config/crush/, so the model
# pointer the host wrote to .clod/crush/model lives at
# $HOME/.config/crush/model.
model_file="$HOME/.config/crush/model"
if [[ -f "$model_file" ]]; then
  model=$(<"$model_file")
else
  model="qwen3-coder:30b"
fi

if ! curl -sSf "$ollama_url/api/tags" \
     | grep -qE "\"name\":\"${model}\"|\"model\":\"${model}\""; then
  printf '[clod] pulling %s (first run on this host; this can take a while)\n' "$model" >&2
  ollama pull "$model"
fi

exec crush "$@"
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

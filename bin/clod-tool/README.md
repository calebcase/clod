# clod tool drivers

`bin/clod` is tool-agnostic: the agent that runs inside the container is
supplied by a *driver* file in this directory. Each driver is a bash script
named `<tool>.sh` that `bin/clod` sources once at startup after resolving the
active tool.

## Choosing a tool

Resolution order (first match wins):

1. `CLOD_TOOL` environment variable.
2. `.clod/tool` file in the working directory — persistent per-directory
   choice, written by `clod-claude` / `clod-crush` (thin wrappers that set
   `CLOD_TOOL` and exec `clod`).
3. `claude` — the default, for backwards compatibility with
   pre-multi-tool clod.

The resolved tool is persisted to `.clod/tool` on every init, so running
`clod-crush` in a claude domain switches it (and vice versa); `CLOD_TOOL`
wins over the file for that one run but the switch sticks.

Switching tools in a directory (e.g. running `clod-crush` in a claude
domain) reinitializes the `.clod` system files and rebuilds the image; the
tool choice is also part of the `.clod` change-detection hash.

## Driver contract

A driver sets two variables and defines these functions. `bin/clod` calls
them at the points shown; a driver may omit `tool_run_extra_setup` and
`tool_run_extra_flags` (others are required).

- `TOOL_NAME` — display name for log messages.
- `TOOL_STATE_DIR` — path under `.clod/` where the tool's host-side state
  lives (e.g. `.clod/claude`, `.clod/crush`).

| Function | Called from | Purpose |
|---|---|---|
| `tool_init` | `initialize()` before Dockerfile generation | Create host-side state dirs, seed configs (never overwrite existing). |
| `tool_sync` | every invocation, after init checks | Idempotent per-run sync (e.g. pick up host credentials that appeared later). |
| `tool_dockerfile_root_section` | `initialize()` | Emit Dockerfile directives running as root before the USER switch: install the tool, write its entrypoint script. |
| `tool_dockerfile_user_install` | `initialize()` | Emit directives running as the unprivileged user (may be a no-op). |
| `tool_dockerfile_entrypoint` | `initialize()` | Emit the `ENTRYPOINT [...]` line. |
| `tool_run_volume_args` | `initialize()` when generating `.clod/system/run` | Emit `-v` lines for the `docker run` invocation. Leave `$cwd`/`$user_home` literal — they expand when the run script executes. |
| `tool_run_env_args` | `initialize()` | Emit `-e` lines for the run script (may be a no-op). |
| `tool_run_extra_flags` | `initialize()` | Optional; emit `docker run` flags placed right after `--rm` (e.g. `--add-host=...`, `--network=host`). Each flag line must end with a line continuation `\`. |
| `tool_run_extra_setup` | `initialize()` | Optional; emit shell lines placed before the `docker run` call in the run script (e.g. host-path setup, or clearing `hostname_flag` when using `--network=host`). |

The common sections shared by all drivers (user/group setup, PATH, SSH
known_hosts pinning, the SSH-agent/GPU/concurrency/run plumbing) live in
`bin/clod` itself, not in drivers.

## Notes for drivers

- Dockerfile content emitted by the `tool_dockerfile_*` functions is
  build-time Dockerfile syntax, not shell script: wrap `$` in `\${...}` when
  the value must survive to container runtime, and use `COPY <<DEOF ...
  DEOF` (an unquoted heredoc marker) for in-image scripts so BuildKit
  expands `$` at build time.
- Keep per-tool state under `TOOL_STATE_DIR` and gitignore it; the
  `.clod` hash only covers top-level files, so per-tool dirs must not
  change contents between runs unless you want a rebuild.
- The container's stdout is a strict channel for the agent's output;
  diagnostic noise belongs on stderr with a `[clod]` prefix.

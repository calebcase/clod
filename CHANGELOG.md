# Changelog

All notable changes to clod will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.6.0] - 2025-12-17

### Added
- **Three-level verbosity system** - Fine-grained control over tool output verbosity
  - Level -1 (🙈 see_no_evil): Silent mode - no verbose tool output at all
  - Level 0 (default): Summary mode - show only tool summaries
  - Level 1 (💬 speech_balloon): Full mode - show complete tool output with snippets
  - Configure default level via `CLOD_BOT_VERBOSITY_LEVEL` environment variable
  - React with 🙈 emoji to enable silent mode on a thread
  - React with 💬 emoji to enable full verbose mode on a thread
  - If multiple verbosity reactions exist, bot uses the least verbose setting
  - Verbosity settings are tracked even before bot is invoked (ready when needed)
  - Bot only posts confirmation messages in threads with active sessions
  - Verbose tools (Read, Glob, Grep, etc.) respect verbosity level
  - Non-verbose tools always show full output regardless of verbosity setting
- **Configurable CLOD_CONCURRENT mode** - Made concurrent clod execution optional
  - Now disabled by default (was always enabled in bot)
  - Configure via `CLOD_CONCURRENT` environment variable
  - Creates unique runtime directories when enabled for parallel execution
- **Thread context gathering** - Bot automatically gathers previous conversation when joining existing threads
  - When bot receives a proper command (`@bot task_name: instructions`) in a thread not started by a bot mention, all prior messages are gathered as context
  - Context includes user names and message content formatted for Claude
  - Only applies to new sessions (threads without existing bot sessions)
  - Bot ignores casual mentions without proper command format in new threads to avoid interrupting unrelated conversations
  - Allows bot to participate naturally in ongoing discussions when explicitly invoked with proper format

### Changed
- **Reduced notification noise** - Bot now consolidates consecutive verbose tool messages
  - When posting verbose tool output, bot edits the previous verbose message instead of creating a new one
  - Significantly reduces Slack notifications during long-running tasks with many tool calls
  - Verbose message tracking is cleared when non-verbose content is posted
  - Only applies to verbose tools in quiet mode (when verbosity toggle is off)
- Runner now accepts `concurrent` parameter and conditionally enables CLOD_CONCURRENT mode
- CLI passes `ClodConcurrent` flag to Runner from configuration
- SessionStore tracks `VerbosityLevel` (int) per thread instead of `Verbose` (bool)

### Fixed
- **Version display in upgrade messages** - Upgrade messages now show actual previous version instead of "vunknown"
  - Read `installed_version` from `.clod/system/version` before checking for changes
  - Previously only read version in some code paths, causing "vunknown" to display
  - Now consistently shows "Upgrading clod from v0.5.0 to v0.6.0" format
- **Permission system not available error** - Fixed missing `CLOD_RUNTIME_DIR` environment variable
  - Runner now sets `CLOD_RUNTIME_DIR` environment variable pointing to `.clod/runtime-{suffix}`
  - Permission MCP script can now find FIFOs for permission requests/responses
  - Previously only set `CLOD_RUNTIME_SUFFIX`, causing "Permission system not available" errors
- **Bot no longer interrupts unrelated threads** - HandleMessage now stays silent for threads without sessions
  - Previously posted error message for any thread reply without a bot session
  - Now only responds to threads with active sessions or proper `@bot task_name: instructions` commands
  - Prevents bot from interrupting team conversations where it wasn't explicitly invoked
- Fixed redundant condition in handlers.go message filtering

## [0.5.0] - 2025-12-16

### Added
- **Four-layer Dockerfile architecture** - New `Dockerfile_user` layer for user-specific customizations
  - User-editable layer that runs in user context (after USER switch)
  - Perfect for installing user-local tools like uvx to `~/.local/bin`
  - Preserved across clod upgrades alongside `Dockerfile_project`
  - Can override entrypoint if needed (set in `Dockerfile_wrapper` by default)
  - Layers: base → project → wrapper → user

### Changed
- Updated documentation to reflect four-layer architecture
- Build process now concatenates four files: base, project, wrapper, user
- Default entrypoint set in wrapper but overridable in user layer

## [0.4.0] - 2025-12-15

### Added
- **Agent prompt support** - Automatically copies a prompt file (default: `README.md`) to the runtime directory and instructs Claude to read it as part of its system prompt
  - Configure with `AGENTS_PROMPT_PATH` environment variable
  - Enables agents to have project-specific instructions
- **Improved tool summaries** - Bot now shows contextual summaries for `Write`, `Edit`, `TodoWrite`, and `EnterPlanMode` tools
  - Displays file paths and task details
  - Better visibility into agent actions
- **Better Bash command display** - Multi-line commands (like heredocs) now show only the first line in summaries
  - Cleaner output in Slack threads
  - Reduces noise from long command blocks
- **Duplicate file handling** - When downloading multiple Slack attachments with the same filename, auto-incrementing numbers are added
  - Example: `image.png`, `image-1.png`, `image-2.png`
  - Prevents file overwrites
- **Verbosity controls** - Added logging level controls for bot
  - Configure via `LOG_LEVEL` environment variable
  - Supports trace, debug, info, warn, error levels

### Fixed
- **Docker environment fixes** - Added `HOME` and `USER` environment variables to container
  - Better compatibility with tools expecting these variables
  - Improved container environment consistency
- Architecture documentation fixes for improved clarity

## [0.3.0] - 2025-12-14

### Added
- **System directory migration** - Moved auto-generated files to `.clod/system/` subdirectory
  - Cleaner separation between user-editable and system-managed files
  - Automatic migration from old structure
  - System files: `Dockerfile_base`, `Dockerfile_wrapper`, `Dockerfile`, `build`, `run`, `version`, `hash`
  - User files remain at `.clod/` root: `Dockerfile_project`, `id`, `name`, `image`, etc.
- **Claude default flags support** - Added `.clod/claude-default-flags` configuration file
  - Set per-directory default flags for `claude` command
  - Supports `--system-prompt` and other flags
  - Flags are automatically passed to every `claude` invocation
- **Root user support** - Fixed home directory handling for root user
  - Root uses `/root` instead of `/home/$USER_NAME`
  - Proper UID/GID handling for root (UID 0)

### Changed
- Directory structure reorganized with system files isolated
- Version tracking and hash computation updated for new structure

## [0.2.9] - 2025-12-14

### Added
- **SSH credential forwarding** - Forward SSH credentials into containers
  - Three modes: existing agent, specific key file, or disabled
  - Configure via `.clod/ssh` file or `CLOD_SSH` environment variable
  - Automatic cleanup of dedicated SSH agents
  - Platform detection (macOS Docker Desktop and Linux)
  - Support for passphrase-protected keys
- **GPU support** - Auto-detect and forward NVIDIA GPUs for AI/ML workloads
  - Configure via `.clod/gpus` file or `CLOD_GPUS` environment variable
  - Auto-detection tests if `--gpus all` works
  - Support for specific GPU selection
  - Compatible with NVIDIA Container Toolkit
- **Install one-liner** - Added convenient one-line installation command
  - Clones repo, sets up paths, creates symlinks
  - Automatically configures shell RC files

### Improved
- Documentation updates for SSH and GPU features
- README improvements with better examples

## [0.2.0] - 2025-10-30

### Added
- **Three-layer Dockerfile architecture** - Separates concerns for better maintainability
  - `Dockerfile_base` - Auto-generated base OS and npm (system-managed)
  - `Dockerfile_project` - Project-specific dependencies (user-editable, preserved)
  - `Dockerfile_wrapper` - User/group setup and Claude Code installation (system-managed)
  - Layers are concatenated at build time
- **Automatic version tracking** - Detects and upgrades when clod version changes
  - Version stored in `.clod/version` file
  - Semantic version comparison for upgrade detection
  - Automatic reinitialization on version mismatch
- **Change detection** - SHA256 hashing triggers rebuild when `.clod/` files are modified
  - Hashes all files in `.clod/` (maxdepth 1)
  - Stored in `.clod/.hash` file
  - Rebuilds only when changes detected
- **Project dependencies preserved** - `Dockerfile_project` customizations survive upgrades
  - User-editable file for project-specific packages
  - Not overwritten during version upgrades
  - Can be committed to git for team sharing
- **Slack bot integration** - Run agents remotely via Socket Mode bot
  - Session persistence across threads
  - Permission prompts with interactive buttons
  - File upload/download support
  - Task discovery from agent directories
  - Multi-user support with allowlist
- **Bot improvements**
  - Handle `text_result` arrays in tool outputs
  - Better instruction handling and UX
  - Version updater utility

### Changed
- Dockerfile structure completely redesigned from single file to layered approach
- Configuration management improved with better separation of concerns

## [0.1.0] - 2025-09-06

### Added
- Initial release of clod
- Basic Docker containerization for Claude Code
- Directory-specific `.clod/` configuration
- Unique container IDs per directory
- Config file management (`.clod/claude/claude.json`)
- Basic Dockerfile generation
- Working directory mounting
- User/group ID mapping for file permissions
- "Kill shell" safety demonstration
- Basic documentation

### Security
- Isolated Docker execution environment
- Limited filesystem access to working directory only
- User ID preservation for file ownership

[0.6.0]: https://github.com/calebcase/clod/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/calebcase/clod/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/calebcase/clod/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/calebcase/clod/compare/v0.2.9...v0.3.0
[0.2.9]: https://github.com/calebcase/clod/compare/v0.2.0...v0.2.9
[0.2.0]: https://github.com/calebcase/clod/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/calebcase/clod/releases/tag/v0.1.0

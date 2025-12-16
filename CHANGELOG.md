# Changelog

All notable changes to clod will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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

[0.5.0]: https://github.com/calebcase/clod/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/calebcase/clod/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/calebcase/clod/compare/v0.2.9...v0.3.0
[0.2.9]: https://github.com/calebcase/clod/compare/v0.2.0...v0.2.9
[0.2.0]: https://github.com/calebcase/clod/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/calebcase/clod/releases/tag/v0.1.0

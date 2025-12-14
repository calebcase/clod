# clod <sub>*ˈkläd*</sub>

Run [claude code][claude-code] in a modestly more secure way.

**Version 0.2.8**

## Features

The main feature of `clod` is that you don't have to worry about what shell
`1a7d6a` is or what would happen if it was killed:

![Kill shell: 1a7d6a](kill-shell-1a7d6a.png "Kill shell: 1a7d6a")

These bots are imperfect. They do unexpected things. If they are running as you
on your main system they can do *a lot* of damage.

Use `clod` and save a ~~kitten~~ home directory today.

### What's New in v0.2

- **Three-layer Dockerfile architecture** - Separates base image, project dependencies, and wrapper
- **Automatic version tracking** - Detects and upgrades when clod version changes
- **Change detection** - SHA256 hashing triggers rebuild when `.clod/` files are modified
- **Project dependencies preserved** - Your `Dockerfile_project` customizations survive upgrades
- **Slack bot integration** - Run agents remotely via Socket Mode bot (see [bot/](./bot/))

## Install

Via user's home bin directory:

```bash
ln -s $(pwd)/bin/clod ~/bin/clod
```

If you don't have a home bin directory:

```bash
mkdir -p ~/bin
export PATH=$PATH:$HOME/bin
```

Add that export to your shell configuration (e.g. `.bashrc`).

## Usage

### Basic Usage

```bash
cd <directory>
clod
```

On first run, clod initializes the `.clod/` directory. If you have a
`~/.claude.json`, it will be copied as the starting configuration. Otherwise, a
fresh config is created.

After initialization, clod builds a Docker image and runs Claude Code inside an
isolated container with access to the current working directory. Clod recreates
the directory structure for the current work directory inside the container.
Clod also preserves ownership and inside the container Claude Code will be
running as your user with your user ID and group ID. Files created by Claude
Code inside the container will be owned by you on the outside.

### Passing Arguments

All arguments are passed directly to `claude-code`:

```bash
clod "Your prompt here"
clod --session-id abc123 "Continue previous session"
clod --permission-mode acceptEdits
clod --dangerously-skip-permissions
```

View all available options:

```bash
clod --help
```

### Saving Configuration

After a session, you can save your Claude config for reuse across projects if
you want to do that:

```bash
cp .clod/claude/claude.json ~/.claude.json
```

New directories initialized with clod will use this config as the base.

## Architecture

### Three-Layer Dockerfile Design

Clod v0.2 uses a layered approach to separate concerns and enable upgrades
without losing customizations:

#### 1. Dockerfile_base (auto-generated)

- Base operating system
- npm installation for Claude Code
- Regenerated when clod version changes

#### 2. Dockerfile_project (user-editable)

- **Your project-specific dependencies**
- Add packages, tools, or custom configuration
- **Preserved across clod upgrades**

Example customization:

```dockerfile
# .clod/Dockerfile_project
FROM base AS project

ARG DEBIAN_FRONTEND=noninteractive
RUN --mount=type=cache,sharing=locked,target=/var/cache/apt \
    --mount=type=cache,sharing=locked,target=/var/lib/apt \
    apt-get update \
 && apt-get install -qq -y \
      bc \
      ca-certificates \
      curl \
      ffmpeg \
      file \
      imagemagick \
      jq \
      python3-venv \
      unzip

RUN ln -s /usr/bin/python3 /usr/bin/python
```

#### 3. Dockerfile_wrapper (auto-generated)

- User/group mapping for file permissions
- Claude Code installation via npm
- Entrypoint configuration

The final `Dockerfile` is created by concatenating these during build.

### Directory Structure

After initialization, the `.clod/` directory contains:

```
.clod/
├── Dockerfile_base           # Auto-generated base
├── Dockerfile_project        # Your customizations (edit this!)
├── Dockerfile_wrapper        # Auto-generated wrapper
├── Dockerfile                # Combined (generated)
├── build                     # Script: builds Docker image
├── run                       # Script: runs container
├── id                        # Unique 8-char container ID
├── name                      # Directory name
├── image                     # Base image (ubuntu:24.04 or golang:latest)
├── version                   # Clod version (0.2.3)
├── .hash                     # SHA256 hash for change detection
├── concurrent                # Optional: "true" enables concurrent instances
├── runtime-{suffix}/         # Runtime files (FIFOs, MCP config) - per instance
└── claude/                   # Claude configuration (gitignored)
    ├── claude.json           # API key, settings, sessions
    └── ...
```

### Automatic Version Management

Clod tracks its version and automatically handles upgrades:

1. **Version file** (`.clod/version`) stores the clod version used for initialization
2. **Semantic version comparison** detects when clod CLI is updated
3. **Automatic reinitialization** when version mismatch detected
4. **Change detection** via SHA256 hash triggers rebuild if `.clod/` files modified manually
5. **Preserves `Dockerfile_project`** - your customizations survive upgrades

### Customizing Docker Configuration

#### Adding Dependencies

Edit `.clod/Dockerfile_project` to add system packages:

```dockerfile
RUN apt-get update && apt-get install -y \
    your-package \
    another-tool
```

#### Adjusting Docker Run Options

Edit `.clod/run` to modify container execution (add ports, mounts, etc.):

```bash
# Example: Add port forward
docker run \
  -p 8080:8080 \
  ...existing flags... \
  "$image" "$@"
```

#### Changing Base Image

Edit `.clod/image` to use a different base:

```bash
echo "golang:latest" > .clod/image
```

## Permission Modes

If the Docker containment is sufficient for your threat model, you can reduce
Claude's permission prompts:

### Accept Edits Automatically

```bash
clod --permission-mode acceptEdits
```

### Bypass All Permissions

```bash
clod --dangerously-skip-permissions
```

See [Claude permission modes documentation][claude-permission-modes] for details.

## Slack Bot Integration

Clod includes a Go-based Slack bot for running agents remotely with full
session persistence and permission management.

### Features

- **Socket Mode** - No public endpoint needed
- **Session persistence** - Continue conversations across Slack threads
- **Permission prompts** - Interactive buttons for tool approvals
- **File uploads** - Attach files as context for agents
- **Task discovery** - Automatically finds agents with `.clod/` directories

### Quick Start

1. **Create Slack App**

   [Click here to create Clod Bot from manifest.](https://api.slack.com/apps?new_app=1&manifest_yaml=_metadata%3A%0A%20%20major_version%3A%202%0A%20%20minor_version%3A%201%0A%0Adisplay_information%3A%0A%20%20name%3A%20Clod%20Bot%0A%20%20description%3A%20Run%20Claude%20Code%20agents%20via%20Slack%20with%20isolated%20Docker%20execution%0A%20%20background_color%3A%20%22%234A154B%22%0A%20%20long_description%3A%20%22Clod%20Bot%20enables%20you%20to%20run%20Claude%20Code%20agents%20directly%20from%20Slack.%20Each%20agent%20runs%20in%20an%20isolated%20Docker%20container%20with%20its%20own%20dependencies%20and%20configuration.%20The%20bot%20supports%20file%20uploads%2C%20session%20persistence%20across%20threads%2C%20and%20interactive%20permission%20prompts.%22%0A%0Afeatures%3A%0A%20%20bot_user%3A%0A%20%20%20%20display_name%3A%20Clod%20Bot%0A%20%20%20%20always_online%3A%20true%0A%0Aoauth_config%3A%0A%20%20scopes%3A%0A%20%20%20%20bot%3A%0A%20%20%20%20%20%20-%20app_mentions%3Aread%0A%20%20%20%20%20%20-%20channels%3Ahistory%0A%20%20%20%20%20%20-%20channels%3Ajoin%0A%20%20%20%20%20%20-%20chat%3Awrite%0A%20%20%20%20%20%20-%20chat%3Awrite.customize%0A%20%20%20%20%20%20-%20chat%3Awrite.public%0A%20%20%20%20%20%20-%20groups%3Ahistory%0A%20%20%20%20%20%20-%20groups%3Aread%0A%20%20%20%20%20%20-%20files%3Aread%0A%20%20%20%20%20%20-%20files%3Awrite%0A%20%20%20%20%20%20-%20remote_files%3Aread%0A%20%20%20%20%20%20-%20remote_files%3Awrite%0A%0Asettings%3A%0A%20%20event_subscriptions%3A%0A%20%20%20%20bot_events%3A%0A%20%20%20%20%20%20-%20app_mention%0A%20%20%20%20%20%20-%20message.channels%0A%20%20%20%20%20%20-%20message.groups%0A%20%20interactivity%3A%0A%20%20%20%20is_enabled%3A%20true%0A%20%20org_deploy_enabled%3A%20false%0A%20%20socket_mode_enabled%3A%20true%0A%20%20token_rotation_enabled%3A%20false%0A)

   Select a workspace and click **Create**.

2. **Generate App-Level Token**

   After creating the app:

   - Navigate to **Settings > Basic Information**
   - Scroll down to **App-Level Tokens**
   - Click **Generate Token and Scopes**
   - Name it (e.g., "socket-token")
   - Add these scopes:
     - `connections:write`
     - `authorizations:read`
     - `app_configurations:write`
   - Click **Generate**
   - **Copy the token** (starts with `xapp-`) - you'll need this for `SLACK_APP_TOKEN`

3. **Install App to Workspace**

   - Go to **Settings > Install App**
   - Click **Install to Workspace**
   - Review permissions and click **Allow**

4. **Get Bot Token**

   - After installation, you'll see **OAuth & Permissions** page
   - **Copy the Bot User OAuth Token** (starts with `xoxb-`) - you'll need this for `SLACK_BOT_TOKEN`
   - Alternatively, navigate to **OAuth & Permissions** in the sidebar to find this token

5. **Find Your User ID**

   - In Slack, click on your profile picture
   - Select **Profile**
   - Click the **⋯ More** menu
   - Select **Copy member ID**
   - This is your User ID (starts with `U`) - you'll need this for `ALLOWED_USERS`

6. **Configure Bot**

   Set these environment variables with the values you copied:

   ```bash
   export SLACK_BOT_TOKEN="xoxb-your-bot-token-here"
   export SLACK_APP_TOKEN="xapp-your-app-token-here"
   export ALLOWED_USERS="U12345678,U87654321"  # Comma-separated User IDs
   export AGENTS_PATH="/path/to/your/agents"   # Directory containing agent folders
   ```

7. **Run Bot**

   ```bash
   go install github.com/calebcase/clod/bot@latest
   bot
   ```

### Bot Usage

Mention the bot with agent name and instructions:

```
@clod-bot services: Follow the instructions in TASK-deprecations.md
```

Assuming an agent directory like:

```
services/
└── TASK-deprecations.md
```

The bot will start a session with `clod` in the `services` directory and give
it the initial prompt `Follow the instructions in TASK-deprecations.md`.

Reply in threads to continue sessions:

```
Find the users that will be impacted by the deprecations.
```

## Sharing Configurations

Clod configuration is "relocatable" (with care) - you can check in parts of
`.clod/` for team collaboration.

### Recommended `.gitignore`

**Critical - Never commit:**

```gitignore
/.clod/claude        # Contains API keys and credentials
/.clod/id            # Machine-specific container ID
/.clod/runtime*      # Runtime files (FIFOs, MCP config) - includes all suffixed dirs
```

**Safe to commit:**

- `.clod/Dockerfile_base` - Generated but stable
- `.clod/Dockerfile_project` - **Your customizations** (commit this!)
- `.clod/Dockerfile_wrapper` - Generated but stable
- `.clod/build` - Build script
- `.clod/run` - Run script (if customized)
- `.clod/version` - Version tracking
- `.clod/concurrent` - Concurrency setting (optional)

### Example Team Workflow

1. Developer customizes `Dockerfile_project` with project dependencies
2. Commits `.clod/Dockerfile_project` and `.clod/version`
3. Teammates clone repo and run `clod`
4. Clod detects existing configuration and uses it
5. Each developer has isolated container with shared dependencies

**Warning:** Do NOT commit `.clod/claude/claude.json` unless you're certain it
contains no credentials or hard-coded paths.

## Environment Variables

### Clod Configuration

- `CLOD_REINIT` - Force reinitialization (default: false)
- `CLOD_CONFIG` - Path to global claude.json (default: ~/.claude.json)
- `CLOD_IMAGE` - Base Docker image (default: ubuntu:24.04)
- `CLOD_ENTRYPOINT` - Override entrypoint (optional)
- `CLOD_CONCURRENT` - Enable concurrent instances (overrides `.clod/concurrent` file)
- `CLOD_RUNTIME_SUFFIX` - Specify runtime directory suffix (auto-generated if not set)
- `MCP_TOOL_TIMEOUT` - Permission prompt timeout (optional)

### Example: Force Reinit

```bash
CLOD_REINIT=true clod
```

### Example: Custom Base Image

```bash
CLOD_IMAGE="node:20" clod
```

This is not a permanent change. Convenient if you want to temporarily switch to
another base image without changing the normal image used. To change the image
permanently edit the `.clod/image` file.

## Troubleshooting

### Docker Build Failures

**Check Dockerfile_project syntax:**
```bash
cat .clod/Dockerfile_project
```

**Rebuild manually:**
```bash
.clod/build
```

**View full Dockerfile:**
```bash
cat .clod/Dockerfile
```

### Permission Issues

**Files created with wrong ownership:**

- Clod maps host UID/GID into container
- Verify: `echo $UID` matches your user ID

**Container can't access files:**

- Check working directory mount: `.clod/run` includes `-v "$cwd:$cwd"`

### Version Detection Issues

**Clod always reinitializes:**

```bash
# Check version file
cat .clod/version

# Check hash
cat .clod/.hash

# Manually set version
echo "0.2.0" > .clod/version
```

**Force upgrade:**

```bash
rm .clod/version
clod
```

### Change Detection False Positives

If clod keeps rebuilding when nothing changed:

```bash
# Recompute hash
cd <agent_directory>
find .clod -maxdepth 1 -type f ! -name claude ! -name .hash | sort | xargs sha256sum | sha256sum | awk '{print $1}' > .clod/.hash
```

## Advanced Usage

### Multiple Agents

Each directory gets its own isolated container:

```bash
cd agent1/ && clod "Task 1" &
cd agent2/ && clod "Task 2" &
```

Container IDs (`.clod/id`) prevent naming conflicts.

### Concurrency

By default clod instances using the same directory are prevented. Instead the
directory is considered to be "locked" by some other clod agent. This is
enforced by naming the docker containers the same.

You can enable concurrent instances in multiple ways:

**Per-directory default (persistent):**
```bash
echo "true" > .clod/concurrent
```

**One-time override (environment variable):**
```bash
CLOD_CONCURRENT=true clod "Your prompt"
```

When concurrent mode is enabled:

- Docker containers are named with a unique suffix (e.g., `clod-agents-12345678-a1b2c3`)
- Each instance gets its own runtime directory (`.clod/runtime-a1b2c3`)
- The runtime directory is mounted read-write inside the container
- Multiple simultaneous clod instances can run in the same directory

The environment variable `CLOD_CONCURRENT` always overrides the
`.clod/concurrent` file setting. The bot automatically enables concurrent mode
and uses a unique runtime directories for each agent session.

### Debugging

Enable Docker build debug:

```bash
# Edit .clod/build and add --progress=plain
docker build --progress=plain ...
```

View container logs:

```bash
docker ps -a  # Find container name
docker logs <container_name>
```

## Similar Projects

If clod's design choices aren't to your liking, these alternatives may be:

- [Claude Code Dev Containers](https://docs.anthropic.com/en/docs/claude-code/devcontainer) - Official devcontainer support
- [claudebox](https://github.com/RchGrav/claudebox) - Alternative Docker wrapper
- [claude-docker](https://github.com/VishalJ99/claude-docker) - Another containerization approach

## License

See [LICENSE](../LICENSE) file for details.

---

[claude-code]: https://www.anthropic.com/claude-code
[claude-permission-modes]: https://docs.anthropic.com/en/docs/claude-code/iam#permission-modes
[claude-dangerously-skip-permissions]: https://docs.anthropic.com/en/docs/claude-code/devcontainer

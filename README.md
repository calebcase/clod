# clod <sub>*ˈkläd*</sub>

Run [claude code][claude-code] in a modestly more secure way.

**Version 0.32.0**

## Features

The main feature of `clod` is that you don't have to worry about what shell
`1a7d6a` is or what would happen if it was killed:

![Kill shell: 1a7d6a](kill-shell-1a7d6a.png "Kill shell: 1a7d6a")

These bots are imperfect. They do unexpected things. If they are running as you
on your main system they can do *a lot* of damage.

Use `clod` and save a ~~kitten~~ home directory today.

## What's New

See [CHANGELOG.md](CHANGELOG.md) for detailed release notes and version history.

## Install

### Quick Install (One-Liner)

Copy and paste this command to install clod:

```bash
mkdir -p ~/src/github.com/calebcase && cd ~/src/github.com/calebcase && git clone https://github.com/calebcase/clod.git && mkdir -p ~/bin && ln -sf ~/src/github.com/calebcase/clod/bin/clod ~/bin/clod && RCFILE="${HOME}/.$(basename $SHELL)rc" && grep -q 'PATH.*HOME/bin' "$RCFILE" 2>/dev/null || echo 'export PATH=$PATH:$HOME/bin' >> "$RCFILE" && export PATH=$PATH:$HOME/bin && echo "✓ Installation complete! Run 'clod' to start, or restart your shell."
```

This will:
1. Clone the repo to `~/src/github.com/calebcase/clod`
2. Create `~/bin` directory if needed
3. Add `~/bin` to your PATH in shell config (`.bashrc`, `.zshrc`, etc.)
4. Add `~/bin` to current shell's PATH
5. Create symlink to the clod script

### Manual Install

If you already have the repo cloned, link it to your home bin directory:

```bash
ln -sf ~/src/github.com/calebcase/clod/bin/clod ~/bin/clod
```

If you don't have a home bin directory:

```bash
mkdir -p ~/bin
export PATH=$PATH:$HOME/bin
```

Add that export to your shell configuration (e.g. `.bashrc` or `.zshrc`).

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

### Four-Layer Dockerfile Design

Clod uses a layered approach to separate concerns and enable upgrades
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

#### 3. Dockerfile_user (user-editable)

- **Your user-specific customizations**
- Runs in user context (after USER switch)
- Add user-local tools, dotfiles, or scripts
- **Preserved across clod upgrades**

Example customization:

```dockerfile
# .clod/Dockerfile_user
FROM wrapper AS user

# Install uv and uvx to user's ~/.local/bin
RUN curl -LsSf https://astral.sh/uv/install.sh | sh
ENV PATH="$PATH:$USER_HOME/.local/bin"
```

#### 4. Dockerfile_wrapper (auto-generated)

- User/group mapping for file permissions
- Claude Code installation via npm
- Sets default entrypoint (can be overridden in Dockerfile_user)

The final `Dockerfile` is created by concatenating these four layers during build.

### Directory Structure

After initialization, the `.clod/` directory contains:

```
.clod/
├── system/                   # System-managed files (auto-generated)
│   ├── Dockerfile_base       # Auto-generated base
│   ├── Dockerfile_wrapper    # Auto-generated wrapper
│   ├── Dockerfile            # Combined (generated)
│   ├── build                 # Script: builds Docker image
│   ├── run                   # Script: runs container
│   ├── version               # Clod version (0.3.0)
│   └── hash                  # SHA256 hash for change detection
├── Dockerfile_project        # Your customizations (edit this!)
├── Dockerfile_user           # Your user customizations (edit this!)
├── id                        # Unique 8-char container ID
├── name                      # Directory name
├── image                     # Base image (ubuntu:24.04 or golang:latest)
├── concurrent                # Optional: "true" enables concurrent instances
├── ssh                       # Optional: SSH forwarding ("true", "false", or key path)
├── gpus                      # Optional: GPU support ("all", device IDs, or empty)
├── claude-default-flags      # Optional: Default flags
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

## SSH Credential Forwarding

Clod supports forwarding SSH credentials into the container, enabling Claude Code to access private Git repositories, remote servers, and other SSH-authenticated resources.

### Configuration Methods

SSH forwarding can be configured via:

1. **Configuration file** (per-directory default):
   ```bash
   echo "auto" > .clod/ssh
   ```

2. **Environment variable** (overrides file):
   ```bash
   export CLOD_SSH="auto"
   ```

### SSH Modes

#### Mode 1: Auto-detect or Start Agent (Recommended)

```bash
echo "auto" > .clod/ssh
```

Uses an existing SSH agent if one is running, otherwise starts a dedicated agent
and loads your default keys. Clod auto-detects the SSH socket path:
- **macOS with Docker Desktop**: Uses `/run/host-services/ssh-auth.sock`
- **Linux or macOS with SSH_AUTH_SOCK**: Uses your `$SSH_AUTH_SOCK`
- **No agent running**: Starts a new agent and runs `ssh-add` to load default keys

This mode is ideal for most setups — it just works whether or not you have an
agent running.

#### Mode 2: Require Existing SSH Agent

```bash
echo "true" > .clod/ssh
```

Requires a running SSH agent. Clod will error if no agent is found. This is
useful when you want to ensure an agent is already configured and don't want
clod to start one automatically.

#### Mode 3: Use Specific SSH Key

```bash
echo "~/.ssh/id_ed25519" > .clod/ssh
# or
export CLOD_SSH="~/.ssh/id_rsa"
```

Starts a dedicated SSH agent with only the specified key. Clod will:
1. Verify the key file exists (errors if not found)
2. Create an isolated SSH agent for this session
3. Add the specified key (prompting for passphrase if needed)
4. Forward the agent into the container
5. Automatically clean up the agent on exit

This mode is useful for:
- Specific per-project keys
- Keys with different passphrases
- Temporary key access without affecting your main agent

#### Mode 4: Disable SSH Forwarding

```bash
echo "false" > .clod/ssh
# or
export CLOD_SSH="false"
```

Explicitly disables SSH forwarding (default behavior).

### Usage Examples

**Auto-detect or start an agent:**

```bash
echo "auto" > .clod/ssh
clod "Clone the backend repo from git@github.com:company/backend.git"
```

**Require an existing agent:**

```bash
eval "$(ssh-agent -s)"
ssh-add ~/.ssh/id_ed25519
echo "true" > .clod/ssh
clod "Clone the backend repo from git@github.com:company/backend.git"
```

**Deploy with a specific key:**

```bash
export CLOD_SSH="~/.ssh/deploy_key"
clod "Deploy the application to production"
```

**Use project-specific key:**

```bash
echo "~/.ssh/project_key" > .clod/ssh
clod "Run the deployment script"
```

### Security Notes

- SSH agent sockets are mounted read-only into containers
- Dedicated agents (auto and key file modes) are isolated and cleaned up automatically
- Keys are never copied into the container - only the agent socket is forwarded
- The `.clod/ssh` file should be added to `.gitignore` if it contains key paths

## GPU Support

Clod can forward GPU access into containers for AI/ML workloads, CUDA
development, or any GPU-accelerated tasks.

### Configuration Methods

GPU support can be configured via:

1. **Configuration file** (per-directory default):
   ```bash
   echo "all" > .clod/gpus
   ```

2. **Environment variable** (overrides file):
   ```bash
   export CLOD_GPUS="all"
   ```

3. **Auto-detection** (default):
   If neither file nor environment variable is set, clod tests if `docker run
   --gpus all` works. If successful, GPU support is automatically enabled.

### GPU Values

The value can be any valid `--gpus` flag value:

- `all` - Forward all GPUs (most common)
- `0` - Forward only GPU 0
- `0,1` - Forward GPUs 0 and 1
- `"device=0,2"` - Forward specific devices
- (empty) - Disable GPU forwarding

### Requirements

To use GPU support, you need:

1. **NVIDIA GPU** and drivers installed on host
2. **NVIDIA Container Toolkit** installed:
   ```bash
   # Ubuntu/Debian
   distribution=$(. /etc/os-release;echo $ID$VERSION_ID)
   curl -s -L https://nvidia.github.io/nvidia-docker/gpgkey | sudo apt-key add -
   curl -s -L https://nvidia.github.io/nvidia-docker/$distribution/nvidia-docker.list | \
     sudo tee /etc/apt/sources.list.d/nvidia-docker.list
   sudo apt-get update && sudo apt-get install -y nvidia-container-toolkit
   sudo systemctl restart docker
   ```

3. **Docker 19.03+** with GPU support

### Usage Examples

**Enable GPUs for ML development:**

```bash
echo "all" > .clod/gpus
clod "Set up PyTorch and train the model using GPU"
```

**Use specific GPU:**

```bash
export CLOD_GPUS="0"
clod "Run the inference script on GPU 0"
```

**Temporarily disable GPU:**

```bash
CLOD_GPUS="" clod "Run CPU-only tests"
```

### Customizing GPU Image

For GPU workloads, you may want a CUDA-enabled base image:

```bash
echo "nvidia/cuda:12.3.0-devel-ubuntu22.04" > .clod/image
```

Edit `.clod/Dockerfile_project` to add ML frameworks:

```dockerfile
FROM base AS project

RUN apt-get update && apt-get install -y \
    python3-pip \
    python3-venv

RUN pip3 install torch torchvision torchaudio --index-url https://download.pytorch.org/whl/cu121
```

### Verifying GPU Access

Inside a clod session:

```bash
clod
# Then in the Claude Code session, ask:
# "Run nvidia-smi to verify GPU access"
```

## Configuration File for Default Flags

You can configure per-directory default flags that are automatically passed to
every `claude` invocation by creating a `.clod/claude-default-flags` file.
Write flags exactly as you would pass them on the command line.

### Example: Set Permission Mode

Create `.clod/claude-default-flags`:

```
--permission-mode acceptEdits
```

Now all invocations in this directory automatically use `--permission-mode acceptEdits`:

```bash
clod "Your prompt here"
# Equivalent to: clod --permission-mode acceptEdits "Your prompt here"
```

### Example: Set System Prompt

Create `.clod/claude-default-flags`:

```
--system-prompt "You are a helpful assistant that specializes in Python development. Always follow PEP 8 style guidelines."
```

This applies your custom system prompt to every session in this directory.

### Combining Multiple Flags

You can combine multiple flags in `.clod/claude-default-flags`:

```
--permission-mode acceptEdits --system-prompt "You are a backend developer. Focus on performance and security."
```

Or split across multiple lines for readability:

```
--permission-mode acceptEdits
--system-prompt "You are a backend developer. Focus on performance and security."
```

Flags from the file are applied first, then command-line arguments:

```bash
clod --session-id abc123 "Continue work"
# Equivalent to: clod --permission-mode acceptEdits --system-prompt "..." --session-id abc123 "Continue work"
```

### Notes

- Write flags exactly as you would on the command line (space-separated, quotes as needed)
- Arguments passed on the command line can override defaults where supported
- The file is read on every `clod` invocation
- This file is safe to commit if it contains project-specific settings
- Individual developers can create their own `.clod/claude-default-flags` for personal preferences (add to `.gitignore` if needed)

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

Clod includes a Go-based Slack bot for running claude code in a workspace of
domain directories. Each domain is its own area of work — its own
`.clod/` setup, its own `README.md` of onboarding context, its own
session history. The bot drives claude inside each domain; the same
README that orients an LLM should be the same README that orients a
new teammate joining the team.

### Concepts

- **Workspace** — the top-level directory the bot watches (`CLOD_BOT_WORKSPACE_PATH`). Holds domain subdirectories plus a workspace-root `README.md` of cross-domain conventions.
- **Domain** — a subdirectory inside the workspace with its own `.clod/` and a `README.md` that explains what the domain is for, its conventions, and how to work in it.
- **Session** — a Slack thread driving an agent inside one domain. Sessions are persistent across thread replies and bot restarts; you start them, configure them, and close them. Each session runs the agent through a series of turns ("tasks") as the user replies.

### Features

- **Socket Mode** — no public endpoint needed; the bot dials out to Slack
- **Multiple ways to start a session** — pick an existing domain, auto-name a new domain (with optional template), run in the workspace root itself, or use "host-direct" mode that bypasses the docker sandbox
- **Session persistence** — continue conversations across Slack threads; on bot restart every active session is auto-resumed (mid-turn work AND idle-between-turns), with a prompt that tells the agent to re-read whatever state it saved during graceful shutdown. Sessions that haven't been touched within `CLOD_BOT_RESUME_STALE_AFTER` (default 30m) are skipped to keep an overnight outage from waking every thread at once
- **Permission prompts** — interactive Slack buttons for tool approvals, with persistent allow/deny patterns
- **Per-session settings** — model (`opus` / `sonnet` / `haiku` / point releases / 1M-context variants), effort level, plan mode, verbosity, file-sync toggle, per-session allowlist
- **File handling** — Slack attachments come into the domain dir; new/changed files in the domain dir flow back to Slack as snippets (toggleable). `@bot upload <path>` pushes host files into the thread, zipping anything over the threshold
- **Slack reference expansion** — paste a permalink to a thread/channel and the bot pulls the conversation into the prompt (with confirmation for large or private references)
- **Onboarding READMEs as system prompt** — the workspace-root `README.md` plus the domain's `README.md` are inlined into claude's system prompt on every run via `--append-system-prompt-file`. Same docs work for a person reading the directory and an LLM running there.
- **Home tab** — per-user recent sessions with clickable links and workspace-wide usage rollups (cost + turns over 24h/7d/30d/90d/365d)
- **Domain discovery** — automatically finds domains (subdirectories with `.clod/`); the workspace root itself is also runnable

### Quick Start

1. **Create Slack App**

   [Click here to create Clod Bot from manifest.](https://api.slack.com/apps?new_app=1&manifest_yaml=_metadata%3A%0A%20%20major_version%3A%202%0A%20%20minor_version%3A%201%0A%0Adisplay_information%3A%0A%20%20name%3A%20Clod%20Bot%0A%20%20description%3A%20Run%20Claude%20Code%20agents%20via%20Slack%20with%20isolated%20Docker%20execution%0A%20%20background_color%3A%20%22%234A154B%22%0A%20%20long_description%3A%20%22Clod%20Bot%20enables%20you%20to%20run%20Claude%20Code%20agents%20directly%20from%20Slack.%20Each%20agent%20runs%20in%20an%20isolated%20Docker%20container%20with%20its%20own%20dependencies%20and%20configuration.%20The%20bot%20supports%20file%20uploads%2C%20session%20persistence%20across%20threads%2C%20and%20interactive%20permission%20prompts.%22%0A%0Afeatures%3A%0A%20%20bot_user%3A%0A%20%20%20%20display_name%3A%20Clod%20Bot%0A%20%20%20%20always_online%3A%20true%0A%20%20app_home%3A%0A%20%20%20%20home_tab_enabled%3A%20true%0A%20%20%20%20messages_tab_enabled%3A%20true%0A%20%20%20%20messages_tab_read_only_enabled%3A%20false%0A%0Aoauth_config%3A%0A%20%20scopes%3A%0A%20%20%20%20bot%3A%0A%20%20%20%20%20%20-%20app_mentions%3Aread%0A%20%20%20%20%20%20-%20channels%3Ahistory%0A%20%20%20%20%20%20-%20channels%3Ajoin%0A%20%20%20%20%20%20-%20channels%3Aread%0A%20%20%20%20%20%20-%20chat%3Awrite%0A%20%20%20%20%20%20-%20chat%3Awrite.customize%0A%20%20%20%20%20%20-%20chat%3Awrite.public%0A%20%20%20%20%20%20-%20groups%3Ahistory%0A%20%20%20%20%20%20-%20groups%3Aread%0A%20%20%20%20%20%20-%20im%3Ahistory%0A%20%20%20%20%20%20-%20im%3Aread%0A%20%20%20%20%20%20-%20im%3Awrite%0A%20%20%20%20%20%20-%20im%3Awrite.topic%0A%20%20%20%20%20%20-%20mpim%3Ahistory%0A%20%20%20%20%20%20-%20mpim%3Aread%0A%20%20%20%20%20%20-%20files%3Aread%0A%20%20%20%20%20%20-%20files%3Awrite%0A%20%20%20%20%20%20-%20reactions%3Aread%0A%20%20%20%20%20%20-%20reactions%3Awrite%0A%20%20%20%20%20%20-%20remote_files%3Aread%0A%20%20%20%20%20%20-%20remote_files%3Awrite%0A%20%20%20%20%20%20-%20search%3Aread.im%0A%20%20%20%20%20%20-%20search%3Aread.mpim%0A%20%20%20%20%20%20-%20users%3Aread%0A%0Asettings%3A%0A%20%20event_subscriptions%3A%0A%20%20%20%20bot_events%3A%0A%20%20%20%20%20%20-%20app_home_opened%0A%20%20%20%20%20%20-%20app_mention%0A%20%20%20%20%20%20-%20message.channels%0A%20%20%20%20%20%20-%20message.groups%0A%20%20%20%20%20%20-%20message.im%0A%20%20%20%20%20%20-%20message.mpim%0A%20%20%20%20%20%20-%20reaction_added%0A%20%20%20%20%20%20-%20reaction_removed%0A%20%20interactivity%3A%0A%20%20%20%20is_enabled%3A%20true%0A%20%20org_deploy_enabled%3A%20false%0A%20%20socket_mode_enabled%3A%20true%0A%20%20token_rotation_enabled%3A%20false%0A)

   The canonical manifest lives at `bot/manifest.yaml` — re-encode that file if you ever need to refresh this link.

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
   - This is your User ID (starts with `U`) - you'll need this for `CLOD_BOT_ALLOWED_USERS`

6. **Configure Bot**

   Set these environment variables with the values you copied:

   ```bash
   export SLACK_BOT_TOKEN="xoxb-your-bot-token-here"
   export SLACK_APP_TOKEN="xapp-your-app-token-here"
   export CLOD_BOT_ALLOWED_USERS="U12345678,U87654321"   # Comma-separated User IDs
   export CLOD_BOT_WORKSPACE_PATH="/path/to/your/workspace" # Workspace dir with domain subdirs
   ```

7. **Run Bot**

   ```bash
   go install github.com/calebcase/clod/bot@latest
   bot
   ```

   See the **Bot Configuration** section below for the full set of `CLOD_BOT_*`
   environment variables (timeouts, default model, README paths, etc.).
   Most users only need the three required tokens above plus
   `CLOD_BOT_WORKSPACE_PATH`.

### Bot Usage

The bot recognizes a small command grammar in mentions. Everything to the left
of the colon is structural; everything to the right is free-form instructions
sent to claude.

#### Starting a session

| Form | Meaning |
| --- | --- |
| `@bot <domain>: <instructions>` | Start a session in an existing domain at `<WorkspacePath>/<domain>/`. If the directory has no `.clod/` yet, the bot opens an init dialog before starting. |
| `@bot <template>:: <instructions>` | Start a session in a new auto-named domain seeded from `<template>`. Skips the init dialog. |
| `@bot :: <instructions>` | Start a session in a new auto-named domain. The bot opens a two-step init dialog (template picker → custom detail) before starting. |
| `@bot *: <instructions>` | Start a session in the workspace root itself rather than a domain subdirectory. Filesync and plan mode default off. |
| `@bot !: <instructions>` | Host-direct session — runs `claude` directly on the host without the docker sandbox. The bot prompts for confirmation first. Sticky for the life of the session. |

Any of the start forms accept an optional **model name** between `@bot` and
the start token, picking a specific model at session start instead of using
the bot-wide default:

```
@bot opus services: do thing
@bot sonnet[1m] :: spike a quick reproduction
@bot haiku <template>:: scaffold
@bot opus *: workspace-wide audit
@bot claude-opus-4-7 services: …
```

Recognised model tokens are the same set `@bot set model=` accepts:
`opus`, `sonnet`, `haiku`, `best`, `default`, `opusplan`, plus specific
point releases (`claude-(opus|sonnet|haiku)-X.Y…`) and a `[1m]` suffix
for 1M-context variants. The first whitespace-delimited word after the
mention is checked against this set; if it doesn't match, parsing
proceeds without consuming a model.

Domain names are restricted to `[a-zA-Z0-9_-]` (max 64 chars). The `::`
form disambiguates template-based auto-naming from `<domain>:` because
domain names can't contain whitespace, which also makes the model
prefix unambiguous against domain names.

Example:

```
@clod-bot services: Follow the instructions in TASK-deprecations.md
```

with a workspace laid out like:

```
workspace/
├── README.md            # Workspace-wide onboarding (shared across domains)
└── services/            # The "services" domain
    ├── .clod/
    ├── README.md        # Domain-specific onboarding
    └── TASK-deprecations.md
```

starts a `clod` session in `services/` with the initial prompt `Follow the
instructions in TASK-deprecations.md`. Reply in the thread to continue:

```
Find the users that will be impacted by the deprecations.
```

#### Per-session commands

These commands run against the active session for the thread you send them in.
They must be sent as `@bot <command>` so they reach the command router rather
than the running agent:

| Command | Effect |
| --- | --- |
| `@bot close` | Stop the agent and close the session. Auto-resume on bot restart is disabled until you @-mention again. |
| `@bot upload <path>` | Upload a host-side file (or directory, with a recursive-vs-top-level prompt) into the thread. Many files get zipped to /tmp first, with a confirmation prompt for archives over 100 MB. |
| `@bot allow @user` / `@bot disallow @user` | Manage the per-session allowlist (in addition to the bot-wide allowlist). |
| `@bot set model=<value>` | Switch model. Accepts families (`opus`, `sonnet`, `haiku`, `best`, `default`, `opusplan`), specific point releases (`claude-opus-4-7`, `claude-opus-4-6`, `claude-sonnet-4-6`, …), and 1M-context variants (`opus[1m]`, `sonnet[1m]`). `+` / `-` cycle, or react with 🎼 / 📜 / 🌸. While the agent is running, the bot cancels and resumes with the new model. |
| `@bot set effort=<value>` | Set how long claude thinks per turn (`low`, `medium`, `high`, `xhigh`, `max`). `+` / `-` step; `clear` removes the override. While the agent is running, the bot cancels and resumes with the new effort. |
| `@bot set verbosity=<value>` | `0` = summary (default), `1` = full, `-1` = silent. Or react with 🙈 / 💬. |
| `@bot set plan=<on/off>` | Toggle plan mode. `+` / `-` / 💭 also work. |
| `@bot set filesync=<on/off>` | Toggle the domain-dir → Slack file watcher for this session. |

#### Joining existing conversations

When you use a start command (`@bot <domain>: instructions`) inside an existing
thread that wasn't started by the bot, the bot collects the prior messages in
the thread and includes them as context in the initial prompt — so you can
bring it into ongoing discussions without recapping.

#### Slack references

Paste a Slack permalink (channel link or thread link) into a message and the
bot will expand the referenced thread and include it in the prompt:

- Public channels the bot isn't in: it auto-joins and posts a notice
- Private channels: invite the bot first with `/invite @<bot>`
- Large or private references trigger a confirmation dialog (Include inline,
  Save as asset, Skip, Cancel)

#### DMs with the bot

Top-level DMs need an explicit prefix (`*:`, `!:`, `::`, `<template>::`, or
`<domain>:`) — the @-mention is implicit. Inside an active session's thread,
just type to send input to the running agent. Bot commands (`close`, `set …`,
`allow @user`) inside a thread still need an explicit `<@bot> <command>`.

#### Onboarding context (READMEs)

Write the workspace and each domain like onboarding documentation. The same
docs that bring a new teammate up to speed are what claude reads as system
prompt context on every run.

- **Workspace-root `README.md`** (at `CLOD_BOT_WORKSPACE_PATH/README.md`)
  — cross-domain conventions, shared tooling, points of contact, links
  to internal references. Applies to every domain.
- **Domain `README.md`** (at `<workspace>/<domain>/README.md`) — what the
  domain owns, how it's structured, common operations, gotchas. Applies
  only when running in that domain. Domain-specific guidance overrides
  workspace-wide on conflict.

Both files are inlined into claude's system prompt via
`--append-system-prompt-file` on every run, with section headers
(`# Workspace context` / `# Domain: <name>`) so the LLM knows which is
which. Either may be missing — the bot just skips that section.

Both filenames are configurable (`CLOD_BOT_WORKSPACE_README` /
`CLOD_BOT_DOMAIN_README`) but the defaults match the natural location a
human would look first.

## Sharing Configurations

Clod configuration is "relocatable" (with care) - you can check in parts of
`.clod/` for team collaboration.

### Recommended `.gitignore`

**Critical - Never commit:**

```gitignore
/.clod/claude        # Contains API keys and credentials
/.clod/id            # Machine-specific container ID
/.clod/runtime*      # Runtime files (FIFOs, MCP config)
/.clod/ssh           # May contain SSH key paths (user-specific)
/.clod/system        # System-managed auto-generated files
```

**Safe to commit:**

- `.clod/Dockerfile_project` - **Your customizations** (commit this!)
- `.clod/name` - Directory name (optional)
- `.clod/image` - Base image selection (optional)
- `.clod/concurrent` - Concurrency setting (optional)
- `.clod/gpus` - GPU configuration (optional)
- `.clod/claude-default-flags` - Default flags (optional)

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
- `CLOD_SSH` - SSH credential forwarding: `true`, `false`, or path to key file (overrides `.clod/ssh` file)
- `CLOD_GPUS` - GPU support: `all`, specific GPU IDs, or empty to disable (overrides `.clod/gpus` file)
- `MCP_TOOL_TIMEOUT` - Permission prompt timeout (optional)

### Bot Configuration

All bot-specific configuration uses the `CLOD_BOT_` prefix.

**Slack credentials (required):**

- `SLACK_BOT_TOKEN` — bot user OAuth token (`xoxb-…`)
- `SLACK_APP_TOKEN` — app-level token for Socket Mode (`xapp-…`)
- `CLOD_BOT_ALLOWED_USERS` — comma-separated list of Slack user IDs allowed to drive the bot

**Workspace + domains:**

- `CLOD_BOT_WORKSPACE_PATH` — workspace directory the bot scans for domains (default: `.`). Subdirectories with a `.clod/` are domains; the workspace itself is also runnable via `@bot *:` / `@bot !:`.
- `CLOD_BOT_DOMAIN_README` — per-domain onboarding README (default: `README.md`, relative to each domain dir or an absolute path). Inlined into claude's system prompt under `# Domain: <name>`. Empty disables.
- `CLOD_BOT_WORKSPACE_README` — workspace-root onboarding README (default: `README.md`, relative to `CLOD_BOT_WORKSPACE_PATH` or absolute). Inlined into claude's system prompt under `# Workspace context`, applied to every domain. Empty disables.

**Behavior + defaults:**

- `CLOD_BOT_DEFAULT_MODEL` — default `claude --model` to use when a thread hasn't picked one (e.g. `opus`, `sonnet`, `claude-haiku-4-5`). Empty defers to claude's own default.
- `CLOD_BOT_PERMISSION_MODE` — claude permission mode: `default`, `acceptEdits`, `plan`, or `bypassPermissions` (default: `bypassPermissions`, matching the recommendation for confined containerized environments).
- `CLOD_BOT_VERBOSITY_LEVEL` — default verbosity: `-1` (silent), `0` (summary), `1` (full) (default: `0`).
- `CLOD_BOT_VERBOSE_TOOLS` — tools affected by the verbosity toggle (default: `Read,Glob,Grep,WebFetch,WebSearch,TodoWrite,Write,Edit,EnterPlanMode`).
- `CLOD_BOT_TIMEOUT` — per-invocation timeout for `clod` execution (default: `24h`).
- `CLOD_BOT_RESUME_STALE_AFTER` — active sessions whose last update is older than this on startup are treated as stale and not auto-resumed (default: `30m`). Set to `0` to disable auto-resume entirely. Sessions within the window are resumed regardless of whether they were mid-turn or idle when the bot stopped — a graceful shutdown gives the agent a chance to save state, and the resume nudge tells it to re-read those notes.

**Storage + lifecycle:**

- `CLOD_BOT_SESSION_STORE_PATH` — path to the session JSON file (default: `sessions.json`). Per-user usage samples are stored in a sidecar `usage.json` next to it.
- `CLOD_BOT_GRACEFUL_SHUTDOWN_TTL` — graceful shutdown window before forcing exit (default: `30s`). On `SIGINT` / `SIGTERM` the bot sends every running session a "save your state and stop" message via stream-json, waits up to this many seconds for each to exit naturally, then force-kills (process-group SIGKILL + `docker stop` of the container). The same TTL bounds per-thread `@bot close` and mid-task model/effort restarts. Send a second signal to force exit immediately.
- `CLOD_BOT_LOG_LEVEL` — `trace` / `debug` / `info` / `warn` / `error` / `fatal` / `panic` (default: `info`).
- `CLOD_BOT_LOG_FORMAT` — `json` or `console` (default: `json`).

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

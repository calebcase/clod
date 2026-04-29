# Clod Architecture

## Overview

Clod is a security-focused wrapper for Claude Code that provides safe execution
through Docker containerization, with optional Slack bot integration for remote
task execution.

---

## System Architecture Diagram

```
┌──────────────────────────────────────────────────────────────────────────────┐
│                               USER INTERFACES                                │
├──────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  ┌───────────────────────────┐              ┌────────────────────────────┐   │
│  │   Terminal (CLI)          │              │   Slack Client             │   │
│  │                           │              │                            │   │
│  │   $ clod [args]           │              │   @bot task_name: ...      │   │
│  └─────────────┬─────────────┘              └──────────────┬─────────────┘   │
│                │                                           │                 │
└────────────────┼───────────────────────────────────────────┼─────────────────┘
                 │                                           │
                 │                                           │
┌────────────────▼───────────────────────────────────────────▼─────────────────┐
│                            CLOD COMPONENTS                                   │
├──────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  ┌─────────────────────────────────┐     ┌──────────────────────────────────┐│
│  │  clod CLI (bash)                │     │  Bot Server (Go)                 ││
│  │  bin/clod                       │     │  bot/main.go                     ││
│  │                                 │     │                                  ││
│  │  ┌───────────────────────────┐  │     │  ┌─────────────────────────────┐ ││
│  │  │  Initialization           │  │     │  │  CLI Layer (cli.go)         │ ││
│  │  │  - Check .clod/ dir       │  │     │  │  - Parse flags              │ ││
│  │  │  - Generate Dockerfiles   │  │     │  │  - Setup components         │ ││
│  │  │  - Create build/run       │  │     │  └─────────────┬───────────────┘ ││
│  │  └─────────────┬─────────────┘  │     │                │                 ││
│  │                │                │     │  ┌─────────────▼───────────────┐ ││
│  │  ┌─────────────▼─────────────┐  │     │  │  Bot Core (bot.go)          │ ││
│  │  │  Build Image              │  │     │  │  - Slack client             │ ││
│  │  │  .clod/system/build       │  │     │  │  - Socket mode handler      │ ││
│  │  │  - Dockerfile_base        │  │     │  │  - Event routing            │ ││
│  │  │  - Dockerfile_project     │  │     │  └─────────────┬───────────────┘ ││
│  │  │  - Dockerfile_wrapper     │  │     │                │                 ││
│  │  │  - Dockerfile_user        │  │     │                │                 ││
│  │  └─────────────┬─────────────┘  │     │  ┌─────────────▼───────────────┐ ││
│  │                │                │     │  │  Handler (handlers.go)      │ ││
│  │  ┌─────────────▼─────────────┐  │     │  │  - app_mention              │ ││
│  │  │  Run Container            │  │     │  │  - message (threads)        │ ││
│  │  │  .clod/system/run         │  │     │  │  - interactive (buttons)    │ ││
│  │  │  - Mount working dir      │  │     │  └─────────────┬───────────────┘ ││
│  │  │  - Mount .clod/claude     │  │     │                │                 ││
│  │  │  - Execute claude-wrapper │  │     │  ┌─────────────▼───────────────┐ ││
│  │  └─────────────┬─────────────┘  │     │  │  Runner (runner.go)         │ ││
│  └────────────────┼────────────────┘     │  │  - Execute clod in PTY      │ ││
│                   │                      │  │  - Parse stream-json        │ ││
│                   │                      │  │  - Permission FIFO mgmt     │ ││
│                   │                      │  └─────────────┬───────────────┘ ││
│                   │                      │                │                 ││
│                   │                      │  ┌─────────────▼───────────────┐ ││
│                   │                      │  │  Supporting Components      │ ││
│                   │                      │  │  ┌─────────────────────┐    │ ││
│                   │                      │  │  │ DomainRegistry      │    │ ││
│                   │                      │  │  │ (domains.go)        │    │ ││
│                   │                      │  │  │ - Discover domains  │    │ ││
│                   │                      │  │  └─────────────────────┘    │ ││
│                   │                      │  │  ┌─────────────────────┐    │ ││
│                   │                      │  │  │ SessionStore        │    │ ││
│                   │                      │  │  │ (session.go)        │    │ ││
│                   │                      │  │  │ - Thread→Session    │    │ ││
│                   │                      │  │  └─────────────────────┘    │ ││
│                   │                      │  │  ┌─────────────────────┐    │ ││
│                   │                      │  │  │ FileHandler         │    │ ││
│                   │                      │  │  │ (files.go)          │    │ ││
│                   │                      │  │  │ - Upload/download   │    │ ││
│                   │                      │  │  └─────────────────────┘    │ ││
│                   │                      │  │  ┌─────────────────────┐    │ ││
│                   │                      │  │  │ Authorizer          │    │ ││
│                   │                      │  │  │ (auth.go)           │    │ ││
│                   │                      │  │  │ - User allowlist    │    │ ││
│                   │                      │  │  └─────────────────────┘    │ ││
│                   │                      │  └─────────────────────────────┘ ││
│                   │                      └──────────────┬───────────────────┘│
│                   │                                     │                    │
└───────────────────┼─────────────────────────────────────┼────────────────────┘
                    │                                     │  
                    │                                     │  
┌───────────────────▼─────────────────────────────────────▼────────────────────┐
│                           DOCKER CONTAINER                                   │
├──────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  ┌─────────────────────────────────────────────────────────────────────────┐ │
│  │  claude-wrapper (bash script)                                           │ │
│  │  - Copy claude.json to ~/.claude/                                       │ │
│  │  - Execute claude-code                                                  │ │
│  │  - Copy claude.json back on exit                                        │ │
│  └──────────────────────────────┬──────────────────────────────────────────┘ │
│                                 │                                            │
│  ┌──────────────────────────────▼────────────────────────────────────────┐   │
│  │  Claude Code (@anthropic-ai/claude-code)                              │   │
│  │                                                                       │   │
│  │  ┌─────────────────────────────────────────────────────────────────┐  │   │
│  │  │  Input/Output: stream-json format                               │  │   │
│  │  │  - Text messages                                                │  │   │
│  │  │  - Image data (base64)                                          │  │   │
│  │  │  - Tool results                                                 │  │   │
│  │  │  - Statistics                                                   │  │   │
│  │  └─────────────────────────────────────────────────────────────────┘  │   │
│  │                                                                       │   │
│  │  ┌─────────────────────────────────────────────────────────────────┐  │   │
│  │  │  MCP Integration                                                │  │   │
│  │  │  - Standard MCP protocol                                        │  │   │
│  │  │  - Permission server via FIFOs                                  │  │   │
│  │  └─────────────────────────────────────────────────────────────────┘  │   │
│  │                                                                       │   │
│  │  ┌─────────────────────────────────────────────────────────────────┐  │   │
│  │  │  Built-in Tools                                                 │  │   │
│  │  │  - Bash, Read, Write, Edit, Grep, Glob                          │  │   │
│  │  │  - Task (sub-agents), WebSearch, WebFetch                       │  │   │
│  │  └─────────────────────────────────────────────────────────────────┘  │   │
│  └───────────────────────────────────────────────────────────────────────┘   │
│                                                                              │
│  ┌─────────────────────────────────────────────────────────────────────────┐ │
│  │  Mounted Volumes                                                        │ │
│  │  ┌────────────────────────────────────────────────────────────────────┐ │ │
│  │  │  /workdir (rw) - Working directory                                 │ │ │
│  │  └────────────────────────────────────────────────────────────────────┘ │ │
│  │  ┌────────────────────────────────────────────────────────────────────┐ │ │
│  │  │  /home/clod/.clod (ro) - Build artifacts                           │ │ │
│  │  └────────────────────────────────────────────────────────────────────┘ │ │
│  │  ┌────────────────────────────────────────────────────────────────────┐ │ │
│  │  │  /home/clod/.clod/claude (rw) - Claude config & sessions           │ │ │
│  │  └────────────────────────────────────────────────────────────────────┘ │ │
│  └─────────────────────────────────────────────────────────────────────────┘ │
│                                                                              │
└──────────────────────────────────────────────────────────────────────────────┘
```

---

## Agent Directory Structure

```
agent_directory/
├── .clod/                              # Docker build configuration
│   ├── system/                         # System-managed files (auto-generated)
│   │   ├── Dockerfile_base             # Generated: Base image with npm
│   │   ├── Dockerfile_wrapper          # Generated: User setup, Claude install, entrypoint
│   │   ├── Dockerfile                  # Combined (auto-generated)
│   │   ├── build                       # Script: Build Docker image
│   │   ├── run                         # Script: Run Docker container
│   │   ├── version                     # clod version
│   │   └── hash                        # Change detection hash
│   ├── Dockerfile_project              # User-editable: Custom dependencies
│   ├── Dockerfile_user                 # User-editable: User-context resources (can override entrypoint)
│   ├── id                              # Unique container ID
│   ├── name                            # Container name
│   ├── image                           # Base image selection
│   ├── concurrent                      # Optional: concurrency setting
│   ├── ssh                             # Optional: SSH forwarding config (auto/true/false/key path)
│   ├── gpus                            # Optional: GPU config
│   ├── claude-default-flags            # Optional: default flags
│   ├── claude/                         # Claude configuration (gitignored)
│   │   └── claude.json                 # Settings, sessions, permissions
│   └── runtime-{suffix}/               # Runtime files (per instance)
│       ├── permission_request.fifo     # Permission requests → bot
│       ├── permission_response.fifo    # Permission responses → Claude
│       ├── permbridge                  # Static linux/amd64 MCP bridge (embedded in bot)
│       ├── mcp_config.json             # MCP server configuration
│       ├── CONTEXT.md                  # Combined onboarding (workspace + domain READMEs)
│       └── direct-home/                # HOME redirect for `@bot !:` host-direct sessions
│
├── [project files]                     # Your working directory
│   ├── src/
│   ├── README.md
│   └── ...
│
└── .gitignore                          # Excludes sensitive dirs
```

---

## Data Flow Diagrams

### 1. CLI Execution Flow

```
User
 │
 │  $ clod [args]
 │
 ▼
┌────────────────────────────────────┐
│  clod CLI Initialization           │
│  1. Check if .clod/ exists         │
│  2. If not, run init:              │
│     - Create .clod/                │
│     - Generate Dockerfiles         │
│     - Create build/run scripts     │
│     - Copy ~/.claude.json          │
└────────────┬───────────────────────┘
             │
             ▼
┌────────────────────────────────────┐
│  Build Docker Image                │
│  Execute: .clod/system/build       │
│  - Builds image from Dockerfiles   │
│  - Tags as clod-<name>-<id>        │
│  - Installs Claude Code            │
└────────────┬───────────────────────┘
             │
             ▼
┌────────────────────────────────────┐
│  Run Container                     │
│  Execute: .clod/system/run [args]  │
│  - Mount working directory         │
│  - Mount .clod/claude              │
│  - Run claude-wrapper              │
└────────────┬───────────────────────┘
             │
             ▼
┌────────────────────────────────────┐
│  Inside Container                  │
│  claude-wrapper:                   │
│  1. Copy .clod/claude/claude.json  │
│     → ~/.claude/claude.json        │
│  2. Run claude-code [args]         │
│  3. On exit: copy back             │
└────────────┬───────────────────────┘
             │
             ▼
           User
        (Interactive
         session)
```

### 2. Bot Task Execution Flow

```
Slack User
    │
    │  @bot task_name: do something [attach files]
    │
    ▼
┌─────────────────────────────────────────┐
│  Bot: HandleAppMention                  │
│  1. Authorize user                      │
│  2. Parse mention → task_name + prompt  │
│  3. Lookup task in registry             │
│  4. Download attached files             │
└─────────────┬───────────────────────────┘
              │
              ▼
┌─────────────────────────────────────────┐
│  Runner: Start Task                     │
│  1. Create .clod/runtime-{suffix}/      │
│  2. Create permission FIFOs             │
│  3. Generate mcp_config.json            │
│  4. Write embedded permbridge binary    │
│  5. Build CONTEXT.md (workspace +       │
│     domain READMEs concatenated)        │
│  6. Execute: cd task_path &&            │
│     clod --output-format stream-json    │
│     --input-format stream-json          │
│     --append-system-prompt ...          │
│     --permission-prompt-tool ...        │
│     (or claude directly when            │
│      UseClaudeDirect is set)            │
└─────────────┬───────────────────────────┘
              │
              ▼
┌─────────────────────────────────────────┐
│  Container: Claude Execution            │
│  - Processes prompt                     │
│  - Executes tools                       │
│  - Requests permissions via MCP         │
│  - Outputs stream-json                  │
└─────────────┬───────────────────────────┘
              │
              │ stream-json output
              ▼
┌─────────────────────────────────────────┐
│  Runner: Parse Output                   │
│  - Text chunks → batch buffer           │
│  - Tool results → snippet format        │
│  - Stats → final summary                │
│  - Send to output channel               │
└─────────────┬───────────────────────────┘
              │
              ▼
┌─────────────────────────────────────────┐
│  Bot: Stream to Slack                   │
│  - Batch updates every 2s               │
│  - Convert markdown → mrkdwn            │
│  - Upload tool results as files         │
│  - Post stats blocks                    │
└─────────────┬───────────────────────────┘
              │
              ▼
┌─────────────────────────────────────────┐
│  Session Store                          │
│  - Save thread → session mapping        │
│  - Persist to sessions.json             │
└─────────────┬───────────────────────────┘
              │
              ▼
          Slack User
       (Sees results
        in thread)
```

### 3. Permission Flow

```
Claude (in container)
    │
    │  Needs permission for tool
    │  (e.g., Bash command)
    ▼
┌──────────────────────────────────┐
│  MCP Bridge (permbridge, Go)     │
│  Static linux/amd64 binary,      │
│  embedded into the bot via       │
│  go:embed and written into the   │
│  per-task runtime dir at start.  │
│  Speaks JSON-RPC MCP on stdio,   │
│  forwards request_permission to  │
│  the bot over the FIFO pair.     │
└────────┬─────────────────────────┘
         │
         │  Write JSON to
         │  permission_request.fifo
         ▼
┌──────────────────────────────────┐
│  Bot: Permission Goroutine       │
│  - Read from FIFO                │
│  - Parse request                 │
└────────┬─────────────────────────┘
         │
         │  Post interactive message
         ▼
┌──────────────────────────────────┐
│  Slack: Interactive Buttons      │
│  ┌────────────────────────────┐  │
│  │  Allow Once                │  │
│  ├────────────────────────────┤  │
│  │  Deny                      │  │
│  ├────────────────────────────┤  │
│  │  Allow All [ToolName]      │  │
│  ├────────────────────────────┤  │
│  │  Allow Similar (pattern:*) │  │
│  └────────────────────────────┘  │
└────────┬─────────────────────────┘
         │
         │  User clicks button
         ▼
┌──────────────────────────────────┐
│  Bot: HandleBlockAction          │
│  1. Parse button value           │
│  2. Update message               │
│  3. Build response JSON          │
│  4. Save pattern if "remember"   │
└────────┬─────────────────────────┘
         │
         │  Write response to
         │  permission_response.fifo
         ▼
┌──────────────────────────────────┐
│  permbridge                      │
│  - Read response from FIFO       │
│  - Return to Claude as MCP       │
│    tool result                   │
└────────┬─────────────────────────┘
         │
         │  {allowed: true/false}
         ▼
      Claude (Continues or tries different approach)
```

### 4. Session Continuation Flow

```
Slack User
    │
    │  (Replies in existing thread)
    │  "Now update the tests"
    ▼
┌─────────────────────────────────┐
│  Bot: HandleMessage             │
│  1. Check if in known thread    │
│  2. Lookup session mapping      │
│  3. Download any new files      │
└─────────────┬───────────────────┘
              │
              │  Found session ID
              ▼
┌─────────────────────────────────────────┐
│  Runner: Resume Task                    │
│  Execute: cd task_path &&               │
│  .clod/system/run --output-format       │
│  stream-json --prompt "..."             │
│  --session-id <existing-id>             │
└─────────────────┬───────────────────────┘
                  │
                  ▼
┌─────────────────────────────────────────┐
│  Container: Claude Resumes              │
│  - Loads session from                   │
│    .clod/claude/claude.json             │
│  - Has full conversation history        │
│  - Continues from context               │
└─────────────────┬───────────────────────┘
                  │
                  ▼
      (Output stream as normal)
```

---

## Component Details

### 1. clod CLI (bin/clod)

**Language**: Bash

**Responsibilities**:

- Initialize .clod directory structure
- Generate layered Dockerfiles
- Build Docker images with Claude Code
- Execute containers with proper mounts
- Manage configuration and versioning

**Key Functions**:

- `clod_init()`: First-time setup
- `clod_build()`: Docker image creation
- `clod_run()`: Container execution

### 2. Bot Server (bot/main.go)

**Language**: Go

**Entry Point**: `main.go`

**Component Breakdown**:

#### CLI Layer (cli.go)
- Parse command-line flags
- Initialize all components
- Start bot server

**Configuration** (full struct lives in `bot/cli.go`):

```go
type Flags struct {
    Log struct {
        Level  zerolog.Level // trace/debug/info/warn/error/fatal/panic
        Format string        // json or console
    }

    SlackBotToken string // xoxb-…
    SlackAppToken string // xapp-…
    AllowedUsers  []string

    SessionStorePath string // sessions.json + sidecar usage.json

    WorkspacePath     string // workspace dir scanned for domains
    DomainReadme    string // per-domain README (default README.md inside the domain dir)
    WorkspaceReadme string // workspace-root README (default README.md at the workspace root)

    ClodTimeout         time.Duration // per-invocation; default 24h
    PermissionMode      string        // default: bypassPermissions
    VerboseTools        []string      // tools the verbosity toggle gates
    VerbosityLevel      int           // -1 silent / 0 summary / 1 full
    DefaultModel        string        // claude --model fallback when thread has none
    GracefulShutdownTTL time.Duration // default 30s
    ResumeStaleAfter    time.Duration // default 30m; 0 disables auto-resume
}
```

#### Bot Core (bot.go)

- Slack client initialization
- Socket mode event handling
- Event routing to handlers

**Event Handlers**:

- `app_mention`: parses commands and starts tasks
- `message` (channels / groups / im / mpim): thread continuations, DM dispatch, slack-permalink expansion, file-attachment ingestion
- `app_home_opened`: renders the Home tab (per-user recent sessions + workspace usage rollup + help reference)
- `block_actions` / `view_submission`: interactive buttons and modal callbacks (permission prompts, init dialogs, upload confirmations, model picker, slackref expansion choice, large-zip / dangerous-mode confirmation, home-tab refresh)
- `reaction_added` / `reaction_removed`: emoji-driven controls on the anchor message (model picker, plan toggle, verbosity)
- Connection lifecycle events

#### Handler Layer (handlers.go)

- Message parsing (parser.go)
- Task execution coordination
- Output streaming
- File management

**Key Methods**:

- `HandleAppMention()`: New task requests
- `HandleMessage()`: Thread replies
- `HandleBlockAction()`: Button clicks
- `runClod()`: Orchestrate task execution

#### Runner (runner.go)

- Task lifecycle management
- PTY for bidirectional I/O
- Stream-json parsing
- Permission FIFO management

**RunningTask Interface**:

```go
type RunningTask interface {
    Output() <-chan OutputMessage
    Permissions() <-chan PermissionRequest
    SendPermission(PermissionResponse)
    Wait() error
}
```

#### Domain Registry (domains.go)

- Discover domain directories under the workspace
- Validate `.clod/` presence
- Map domain names to absolute paths

**Discovery Logic**:

```go
// Scans CLOD_BOT_WORKSPACE_PATH for:
// - Subdirectories with a .clod/ subdirectory
// - Containing an executable .clod/system/run script
// Maps: lowercase(dirname) → absolute path
```

#### Session Store (session.go)

- Thread-to-session mapping
- JSON persistence
- Atomic file writes

**Session Mapping** (full struct lives in `bot/session.go`):

```go
type SessionMapping struct {
    ChannelID string
    ThreadTS  string
    TaskName  string
    TaskPath  string
    SessionID string
    UserID    string

    // Per-thread settings (also see `@bot set …` commands)
    VerbosityLevel    int    // -1 silent / 0 summary / 1 full
    Model             string // "" = bot default; family or specific release
    PermissionMode    string // "" = bot default; "plan" enables plan mode
    FileSyncDisabled  bool   // toggle for the task-dir → Slack file watcher
    UseClaudeDirect   bool   // run claude on the host instead of via clod
    ExtraAllowedUsers []string

    // Liveness + resume
    Active           bool   // true while runClod is executing; cleared on clean exit
    Idle             bool   // true between turns (agent emitted result, waiting for input)
    ReactionAnchorTS string // first @-mention TS — anchor for status reactions; pinned for life

    // Reactions the bot has placed on the anchor (so we can remove them later;
    // Slack reactions are per-user)
    ModelReactionEmoji string
    MonitorCountEmoji  string

    // Background watchers spawned by the agent's Monitor tool
    ActiveMonitors []string

    // Lifetime totals across resumes (claude's per-result stats only cover
    // the current process, so we accumulate here)
    CumulativeCostUSD float64
    CumulativeTurns   int

    CreatedAt time.Time
    UpdatedAt time.Time // bumped on every heartbeat / mutation
}
```

`SessionStore` keeps a map keyed by `channelID:threadTS`. The store also
tracks per-result usage samples (user, cost USD, turns, timestamp) in a
sidecar `usage.json` file — used by the Home tab to render workspace-wide
rollups over rolling 24h / 7d / 30d / 90d / 365d windows. Samples older
than 365 days are pruned at append time.

#### File Handler (files.go)

- Download Slack attachments
- Upload task outputs
- Watch for new files

**Capabilities**:

- Download message attachments → task directory
- Upload task outputs → Slack thread
- Auto-detect new files in task directory
- Format tool results as collapsible snippets

#### Authorizer (auth.go)

- User ID validation
- Allowlist checking

### 3. Permission System

**Components**:

1. **MCP Bridge** (`bot/permbridge`): a small Go program that speaks JSON-RPC
   MCP on stdio and advertises a single `request_permission` tool. Built
   statically as a `linux/amd64` binary and embedded into the bot via
   `go:embed`; the bot writes the bytes into the per-task runtime dir at
   startup and points claude's `--permission-prompt-tool` at that path.
   Replaces an earlier Python implementation so user task images don't have
   to carry a python3 interpreter just to make the permission system work.
2. **FIFO Pipes** (`permission_request.fifo` / `permission_response.fifo`):
   the runtime dir is bind-mounted into the container, so both sides see
   the same paths. permbridge writes requests and reads responses; the bot
   does the inverse.
3. **Pattern Matching** (`permission.go`): rule evaluation for "Allow
   similar" decisions (`Tool(arg:*)`, `Tool(path/**)`, etc.).
4. **Persistence**: approved patterns are saved to both `allowedTools` and
   `permissions.allow` in `.clod/claude/claude.json` so claude reapplies
   them on resume.

### 4. Command Grammar (parser.go)

The bot recognizes a small grammar in mentions:

| Form | Result |
| --- | --- |
| `<@bot> <domain>: <text>` | Start a task in an existing domain at `<WorkspacePath>/<domain>/`; opens an init dialog if `.clod/` is missing. |
| `<@bot> <template>:: <text>` | Auto-name a new task and copy `<template>` as the seed. Skips the dialog. |
| `<@bot> :: <text>` | Auto-name a new task; pick template / Custom in a two-step modal. |
| `<@bot> *: <text>` | Run inside the workspace root itself (no per-domain subdir). |
| `<@bot> !: <text>` | Host-direct mode — run `claude` on the host without docker. Confirmation required. |
| `<@bot> close` / `set …` / `allow @u` / `disallow @u` / `upload <path>` | Per-thread commands that route to the command handler instead of the running agent. |

Task names are restricted to `[a-zA-Z0-9_-]` (max 64 chars). Because they
can't contain whitespace, `<template>::` and `<task>:` are unambiguous.

DMs follow the same grammar with one exception: top-level DMs require the
explicit prefix (the `@bot` mention is implicit). Inside an active session's
thread, plain text is forwarded as input to the running task; bot commands
inside a thread still need an explicit `<@bot>` mention to reach the
command router.

### 5. Home Tab (hometab.go)

On `app_home_opened` the bot renders:

- **Personal section** — sessions owned by the viewer that have been touched
  in the last 7 days. Each row shows the task name as a link to the anchor
  message (the user's @-mention), an active/idle status pip, lifetime turns
  + cost, and "updated N ago". A `[latest →]` link jumps to the most recent
  bot post in the thread when that's distinct from the anchor.
- **Workspace section** (allowlisted users only) — per-user usage rollups
  over rolling 24h / 7d / 30d / 90d / 365d windows, sorted by 30-day cost.
- **Help reference** — the same `buildHomeHelpBlocks` content the help
  modal uses; serves as a discoverability surface for the command grammar.
- **Refresh button** — re-renders the view in place.

### 6. Slack-reference expansion (slackref.go)

When the user pastes a Slack permalink (channel link or thread link) into a
message, the bot detects it, fetches the referenced thread, and includes it
in the prompt. For public channels the bot isn't already a member of, it
auto-joins via `channels:join` and posts a notice in the active thread.
Private channels require an explicit `/invite @bot`. Large or private
references trigger a confirmation modal (Include inline, Save as asset,
Skip, Cancel).

### 7. Upload command (upload.go)

`@bot upload <path>` pushes host-side files into the active thread:

- Single file → uploaded directly.
- Directory → recursive-vs-top-level prompt.
- Many files / large totals → zipped to `/tmp` first, with progress posted
  back to Slack as the zip is built.
- Archives over 100 MB → confirmation modal before transfer.
- During upload → a progressReader streams byte counts back so users see
  movement on long transfers.

### 8. Onboarding context (runner.go + permission.go)

On every clod invocation the runner builds a single combined `CONTEXT.md`
in the runtime dir containing the available workspace + domain READMEs,
then passes it to claude via `--append-system-prompt-file`:

- `# Workspace context` — from `<WorkspacePath>/<CLOD_BOT_WORKSPACE_README>`
  (default `README.md` at the workspace root). Shared across every domain.
- `# Domain: <name>` — from `<domain>/<CLOD_BOT_DOMAIN_README>` (default
  `README.md` inside the domain). Domain-specific guidance.

Either or both source files may be missing on disk; missing sections are
silently skipped. The workspace section is emitted first so the
domain-specific section appears last and can override on conflict.

The file form (`--append-system-prompt-file`) sidesteps the OS argv
length cap that long inline `--append-system-prompt` text would hit. The
runtime dir is bind-mounted into the container, so the path the bot
writes is the path claude opens. When neither README is present, no file
is written and the flag is omitted.

The intent is that a workspace's README files double as onboarding docs
— the same content that orients a new teammate in a domain is the
content the LLM gets as system prompt.

### 9. Host-direct mode

`@bot !: …` (and reaffirm-confirmation flow) flips `UseClaudeDirect` on the
session. In that mode the runner spawns `claude` on the host instead of via
`clod`'s docker wrapper. To keep the host config isolated, HOME is
redirected to the per-task `.clod/direct-home/` so claude's own state files
land under the task dir rather than in the operator's `~/.claude*`. The
flag is sticky for the life of the session so resumes keep the same
execution mode.

### 10. Auto-restart on `set model` / `set effort`

Mid-session changes to `model` or `effortLevel` aren't applied by claude
once the stream-json process has started. When a `set model=…` or
`set effort=…` lands while a task is running, the bot writes the new value
to both `sessions.json` and the per-task `.clod/claude/settings.json`,
records the cancel as expected (so the synthesized "task cancelled"
message is suppressed), cancels the running task, and immediately resumes
with the new `--model` / effort applied.

### 11. Auto-resume on bot restart (busy vs idle)

`SessionMapping.Active` flips to `true` when `runClod` starts and only
back to `false` on a clean exit. An unclean exit (crash, shutdown,
timeout) leaves it set so the next bot startup can pick up where the
previous run left off.

`SessionMapping.Idle` rides alongside `Active` to track turn-level
state:

- `false` while a turn is in progress — set by `runClod` at start,
  reset by the `sendUserInput` helper that wraps every user-driven
  `task.SendInput` (initial prompt, thread reply, ambig redirect).
- `true` between turns — set when claude emits the per-turn `result`
  / stats message. Because claude doesn't emit `result` until every
  tool call in the turn has resolved, an outstanding permission
  prompt or `AskUserQuestion` modal naturally keeps `Idle=false`
  (the agent is paused on the FIFO / tool result, no result yet).

`ResumeActiveSessions` partitions `Active=true` sessions on startup:

- **Stale** (UpdatedAt older than `CLOD_BOT_RESUME_STALE_AFTER`) —
  flag cleared, no resume. Prevents stopped-then-left-overnight
  threads from all waking up at once.
- **Idle** (`Active && Idle`) — flag cleared, no resume notice. The
  agent had finished its last turn cleanly, so the bot considers the
  task complete. The next user mention in the thread takes the
  existing resume-on-mention path, which posts the standard
  *"Resuming task X..."* notice — i.e. an active user re-engagement,
  not a deploy-time wake-up.
- **Busy** (`Active && !Idle`) — resumed via `runClod` with the
  saved `SessionID` plus a "continue where you left off" nudge.
  Covers mid-stream computation as well as outstanding permission
  / `AskUserQuestion` prompts: claude's `--resume` re-evaluates the
  conversation and re-issues any unresolved tool call, which the
  bot intercepts and re-posts.

This convention works in concert with workspace guidance to use
`AskUserQuestion` for any user-input request (rather than asking in
prose) so the "result with no pending tool call = task complete"
signal stays reliable. See the workspace README's *"When you need
user input"* section.

### Anchor message stability

`ReactionAnchorTS` is the message channel browsers, the home tab,
and Slack search all surface as "this thread"; it's pinned to the
first @-mention that created the session and is never reassigned to
later mentions. A `@bot close` followed by a re-mention reuses the
same anchor so the bot's status reactions (model emoji, plan-mode
💭, monitor-count keycap) stay where users actually look for them.
The reassignment guard lives in `handleNewTask` and the post-init
handler — both gate `ReactionAnchorTS = ev.TimeStamp` (or
`p.MentionTS`) on `if session.ReactionAnchorTS == ""`.

**Permission Patterns**:

```
ToolName                    # Allow all uses
ToolName(pattern:*)         # Pattern-based matching
  - Bash(python:*)          # All python commands
  - Write(src/**)           # Writes in src/ tree
  - Edit(*.test.ts)         # Edit test files
```

**Saved Rules Location**:

```json
{
  "allowedTools": [
    "Read",
    "Bash(git:*)",
    "Write(src/**)"
  ]
}
```

---

## Security Architecture

### Isolation Layers

```
Host System
    │
    ├─ clod CLI: Minimal bash script
    │
    └─ Docker Boundary
        │
        ├─ Container Filesystem (isolated)
        │   ├─ Claude Code executable
        │   └─ Node.js runtime
        │
        ├─ Mounted Volumes (controlled)
        │   ├─ /workdir (rw) - Task directory only
        │   └─ /home/clod/.clod/claude (rw) - Config only
        │
        └─ User Mapping
            └─ Container UID:GID = Host UID:GID
```

### Permission Control Flow

```
User Request
    ↓
Bot Authorization (allowlist)
    ↓
Claude Tool Execution
    ↓
MCP Permission Check
    ↓
Interactive Approval (Slack)
    ↓
Pattern Matching
    ↓
Tool Execution (if approved)
```

### Security Features

1. **Containerization**
   - All Claude operations run in Docker
   - Limited filesystem access
   - Isolated network namespace (default)

2. **User Authorization**
   - Slack user allowlist
   - Per-user session tracking
   - Audit trail in logs

3. **Permission System**
   - Interactive approval for sensitive tools
   - Pattern-based rules (glob-like)
   - Persistent rule storage
   - Per-session permission requests

4. **File Access Control**
   - Only task directory mounted (rw)
   - Config directory mounted (rw, but limited)
   - No access to host system
   - User ID mapping preserves permissions

5. **Audit Trail**
   - All interactions logged
   - Session history in claude.json
   - Slack thread provides history

---

## Configuration Management

### Environment Variables

**Bot Configuration** (full descriptions in README.md):

```bash
# Required
SLACK_BOT_TOKEN=xoxb-...
SLACK_APP_TOKEN=xapp-...
CLOD_BOT_ALLOWED_USERS=U123,U456,U789

# Optional - workspace + domains
CLOD_BOT_WORKSPACE_PATH=/path/to/workspace          # Default: .
CLOD_BOT_DOMAIN_README=README.md                  # Per-domain onboarding README
CLOD_BOT_WORKSPACE_README=README.md               # Workspace-root onboarding README

# Optional - behavior + defaults
CLOD_BOT_DEFAULT_MODEL=                            # e.g. opus, claude-haiku-4-5
CLOD_BOT_PERMISSION_MODE=bypassPermissions         # default|acceptEdits|plan|bypassPermissions
CLOD_BOT_VERBOSITY_LEVEL=0                         # -1 silent / 0 summary / 1 full
CLOD_BOT_VERBOSE_TOOLS=Read,Glob,Grep,...          # Tools the verbosity toggle gates
CLOD_BOT_TIMEOUT=24h                               # Per-invocation timeout
CLOD_BOT_RESUME_STALE_AFTER=30m                    # 0 disables auto-resume

# Optional - storage + lifecycle
CLOD_BOT_SESSION_STORE_PATH=./sessions.json        # Sidecar usage.json next to it
CLOD_BOT_GRACEFUL_SHUTDOWN_TTL=30s
CLOD_BOT_LOG_LEVEL=info                            # trace|debug|info|warn|error|fatal|panic
CLOD_BOT_LOG_FORMAT=json                           # json|console
```

The bot always launches `clod` in concurrent mode so each session gets its
own runtime dir and the permission FIFOs don't collide. `CLOD_CONCURRENT` is
no longer a user-facing toggle.

### Claude Configuration (claude.json)

**Location**: `.clod/claude/claude.json`

**Structure**:

```json
{
  "apiKey": "sk-ant-...",
  "allowedTools": [
    "Read",
    "Bash(git:*)",
    "Write(src/**)"
  ],
  "sessions": {
    "session-id-123": {
      "messages": [...],
      "context": {...}
    }
  }
}
```

### MCP Configuration (mcp_config.json)

**Location**: `.clod/runtime-{suffix}/mcp_config.json`

**Generated by**: Runner at task start. The runtime dir is bind-mounted into
the container, so the in-container path matches the host path.

**Structure**:

```json
{
  "mcpServers": {
    "permission": {
      "command": "/abs/path/to/.clod/runtime-{suffix}/permbridge",
      "args": []
    }
  }
}
```

`permbridge` reads `CLOD_RUNTIME_DIR` from its environment and looks for
`permission_request.fifo` / `permission_response.fifo` inside it.

---

## Extension Points

### 1. Custom Dependencies (Dockerfile_project)

**File**: `.clod/Dockerfile_project`

Users can add custom packages:

```dockerfile
# Example: Add Python and tools
RUN apt-get update && apt-get install -y \
    python3 \
    python3-pip \
    && rm -rf /var/lib/apt/lists/*

RUN pip3 install pandas numpy
```

### 2. Additional MCP Servers

Add to `mcp_config.json` template in runner:

```json
{
  "mcpServers": {
    "permission": { ... },
    "custom-server": {
      "command": "node",
      "args": ["/path/to/server.js"]
    }
  }
}
```

### 3. Custom Permission Patterns

Implement new pattern types in `permission.go`:

```go
// Add new pattern matching logic
func MatchPattern(pattern, value string) bool {
    // Custom matching logic
}
```

### 4. Output Formatters

Add custom formatters in `handlers.go`:

```go
// Handle new tool output types
func formatToolOutput(tool string, output interface{}) string {
    switch tool {
    case "MyCustomTool":
        return formatMyCustomTool(output)
    default:
        return formatDefault(output)
    }
}
```

---

## Technology Stack

### CLI Component

- **Language**: Bash 4.0+
- **Runtime**: Docker
- **Container Base**: Ubuntu/Debian (configurable)
- **Claude Runtime**: Node.js (npm)

### Bot Component

- **Language**: Go 1.23.2+
- **Core Libraries**:
  - `slack-go/slack`: Slack API + Socket Mode
  - `alecthomas/kong`: CLI argument parsing
  - `rs/zerolog`: Structured logging
  - `creack/pty`: Pseudo-terminal for subprocess I/O
  - `gomarkdown/markdown`: Markdown processing
- **Concurrency**: Goroutines for I/O streaming
- **IPC**: FIFO named pipes (host ↔ container) + a static `permbridge`
  Go binary embedded into the bot via `go:embed` and written into each
  per-task runtime dir
- **Persistence**: JSON files (`sessions.json` for per-thread state,
  sidecar `usage.json` for per-result usage samples)

### Integration Layer

- **Claude Code**: `@anthropic-ai/claude-code` (npm package)
- **I/O Format**: stream-json (proprietary format)
- **Permission Protocol**: MCP (Model Context Protocol)
- **Container**: Docker Engine API

---

## Operational Workflows

### First-Time Setup

**CLI**:

```bash
cd /path/to/project
clod                # Initializes .clod/
# Edit .clod/Dockerfile_project if needed
clod                # Builds image and runs
```

**Bot**:

```bash
export SLACK_BOT_TOKEN=xoxb-...
export SLACK_APP_TOKEN=xapp-...
export CLOD_BOT_ALLOWED_USERS=U123,U456
export CLOD_BOT_WORKSPACE_PATH=/path/to/workspace

cd /path/to/workspace/my_domain
clod                # Initialize the domain

cd /path/to/clod/bot
go run . server     # Start bot
```

### Daily Usage

**CLI**:

```bash
cd /path/to/project
clod "Add user authentication"
# or
clod --session-id abc123 "Continue with tests"
```

**Bot** (in Slack):

```
@bot my_task: Implement feature X
# Upload files if needed

# In same thread:
Now add tests for that
```

### Permission Management

**Approve Pattern**:

1. Bot posts permission request with buttons
2. User clicks "Allow Similar"
3. Pattern saved to `.clod/claude/claude.json`
4. Future matching requests auto-approved

**View Saved Patterns**:

```bash
cat .clod/claude/claude.json | jq .allowedTools
```

**Share Patterns**:

```bash
# Commit claude.json to git (without API key)
git add .clod/claude/claude.json
git commit -m "Add approved tool patterns"
```

---

## Performance Considerations

### Bot Performance

**Concurrent Operations**:

- Multiple tasks can run simultaneously (separate goroutines)
- Each task has dedicated PTY and output stream
- Permission handling is per-task

**Resource Limits**:

- Docker container resources (CPU, memory)
- Configurable via Docker run flags in `.clod/system/run`

**Slack Rate Limits**:

- Message updates batched (2-second intervals)
- File uploads throttled
- API calls respect Slack limits

### Optimization Tips

1. **Image Caching**: Reuse built images (tracked by .clod/system/hash)
2. **Session Reuse**: Continue threads for faster context loading
3. **Pattern Approval**: Pre-approve common patterns for automation
4. **Log Levels**: Use `info` or `warn` in production

---

## Troubleshooting

### Common Issues

**CLI**:

```bash
# Container won't start
docker ps -a                    # Check container status
docker logs clod-<name>-<id>    # View logs

# Permission denied
ls -la .clod/system/run         # Check executable bit
chmod +x .clod/system/run       # Fix if needed

# Wrong version
rm .clod/system/version         # Force rebuild
clod                            # Reinitialize
```

**Bot**:

```bash
# Bot not responding
# Check logs for connection errors
# Verify tokens are valid
# Ensure user is in allowlist

# Permission timeout
# Check .clod-runtime/ exists
# Verify FIFO files created
# Look for MCP server errors in logs

# Session not resuming
cat sessions.json | jq          # Check mapping exists
# Ensure ThreadTS matches thread
```

### Debug Logging

**Bot**:

```bash
CLOD_BOT_LOG_LEVEL=debug go run . server
# or
CLOD_BOT_LOG_LEVEL=trace CLOD_BOT_LOG_FORMAT=json go run . server > bot.log
```

**CLI**:

```bash
# Add to .clod/system/run
docker run ... --env DEBUG=1 ...
```

---

## Summary

Clod provides a comprehensive solution for safe Claude Code execution through:

1. **Containerization**: Docker isolation prevents host system access
2. **Slack Integration**: Remote execution with full session management
3. **Permission System**: Interactive approval with pattern-based automation
4. **Session Persistence**: Resume conversations naturally in threads
5. **File Management**: Seamless file transfer between Slack and tasks
6. **Extensibility**: Custom dependencies, MCP servers, and patterns

The architecture balances **security** (isolation, permissions, authorization)
with **usability** (sessions, continuations, file support), making it suitable
for team environments where Claude needs controlled access to codebases.

Key design principles:

- **Defense in depth**: Multiple security layers
- **Principle of least privilege**: Minimal access by default
- **User control**: Interactive approval for sensitive operations
- **Transparency**: Full audit trail and visibility
- **Simplicity**: Easy setup and configuration

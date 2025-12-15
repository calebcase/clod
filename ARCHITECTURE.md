# Clod Architecture

## Overview

Clod is a security-focused wrapper for Claude Code that provides safe execution through Docker containerization, with optional Slack bot integration for remote task execution.

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
│                   │                      │  │  │ TaskRegistry        │    │ ││
│                   │                      │  │  │ (tasks.go)          │    │ ││
│                   │                      │  │  │ - Discover agents   │    │ ││
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
│   │   ├── Dockerfile_wrapper          # Generated: User setup, Claude install
│   │   ├── Dockerfile                  # Combined (auto-generated)
│   │   ├── build                       # Script: Build Docker image
│   │   ├── run                         # Script: Run Docker container
│   │   ├── version                     # clod version
│   │   └── hash                        # Change detection hash
│   ├── Dockerfile_project              # User-editable: Custom dependencies
│   ├── id                              # Unique container ID
│   ├── name                            # Container name
│   ├── image                           # Base image selection
│   ├── concurrent                      # Optional: concurrency setting
│   ├── ssh                             # Optional: SSH forwarding config
│   ├── gpus                            # Optional: GPU config
│   ├── claude-default-flags            # Optional: default flags
│   ├── claude/                         # Claude configuration (gitignored)
│   │   └── claude.json                 # Settings, sessions, permissions
│   └── runtime-{suffix}/               # Runtime files (per instance)
│       ├── permission_request.fifo     # Permission requests → bot
│       ├── permission_response.fifo    # Permission responses → Claude
│       ├── permission_mcp.py           # MCP permission server
│       └── mcp_config.json             # MCP server configuration
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
│  1. Create .clod-runtime/               │
│  2. Create permission FIFOs             │
│  3. Generate mcp_config.json            │
│  4. Copy permission_mcp.py              │
│  5. Execute: cd task_path &&            │
│     .clod/system/run --output-format    │
│     stream-json --prompt "..."          │
│     --mcp-config ...                    │
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
│  MCP Server (permission_mcp.py)  │
│  Tool: request_permission        │
│  Args: {tool, args, description} │
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
│  MCP Server                      │
│  - Read response from FIFO       │
│  - Return to Claude              │
└────────┬─────────────────────────┘
         │
         │  {allowed: true/false}
         ▼
      Claude
   (Continues or
    tries different
     approach)
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
        (Output stream
         as normal)
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

**Configuration**:
```go
type CLI struct {
    SlackBotToken   string
    SlackAppToken   string
    AllowedUsers    []string
    AgentsPath      string
    SessionStore    string
    PermissionMode  string
    ClodTimeout     time.Duration
}
```

#### Bot Core (bot.go)
- Slack client initialization
- Socket mode event handling
- Event routing to handlers

**Event Handlers**:
- `app_mention`: Initial task requests
- `message`: Thread continuations
- `interactive`: Permission buttons
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

#### Task Registry (tasks.go)
- Discover agent directories
- Validate .clod presence
- Map task names to paths

**Discovery Logic**:
```go
// Scans AGENTS_PATH for:
// - Directories with .clod/ subdirectory
// - Containing executable .clod/system/run script
// Maps: lowercase(dirname) → absolute path
```

#### Session Store (session.go)
- Thread-to-session mapping
- JSON persistence
- Atomic file writes

**Session Mapping**:
```go
type SessionMapping struct {
    ChannelID string
    ThreadTS  string
    TaskName  string
    TaskPath  string
    SessionID string
    UserID    string
    CreatedAt time.Time
    UpdatedAt time.Time
}
```

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
1. **MCP Server** (permission_mcp.py): Python script implementing MCP protocol
2. **FIFO Pipes**: Named pipes for IPC
3. **Pattern Matching** (permission.go): Rule evaluation
4. **Persistence**: Save rules to claude.json

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

**Bot Configuration**:
```bash
# Required
SLACK_BOT_TOKEN=xoxb-...
SLACK_APP_TOKEN=xapp-...
ALLOWED_USERS=U123,U456,U789

# Optional
AGENTS_PATH=/path/to/agents       # Default: ./agents
SESSION_STORE_PATH=./sessions.json
PERMISSION_MODE=default           # default|acceptEdits|bypassPermissions
CLOD_TIMEOUT=30m                  # Default: 30 minutes
LOG_LEVEL=info                    # trace|debug|info|warn|error
LOG_FORMAT=console                # json|console
```

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

**Location**: `.clod-runtime/mcp_config.json`
**Generated by**: Runner at task start

**Structure**:
```json
{
  "mcpServers": {
    "permission": {
      "command": "python3",
      "args": [
        "/abs/path/to/.clod-runtime/permission_mcp.py",
        "/abs/path/to/.clod-runtime/permission_request.fifo",
        "/abs/path/to/.clod-runtime/permission_response.fifo"
      ]
    }
  }
}
```

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
- **IPC**: FIFO named pipes
- **Persistence**: JSON files

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
export ALLOWED_USERS=U123,U456
export AGENTS_PATH=/path/to/agents

cd /path/to/agents/my_task
clod                # Initialize task

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
LOG_LEVEL=debug go run . server
# or
LOG_LEVEL=trace LOG_FORMAT=json go run . server > bot.log
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

The architecture balances **security** (isolation, permissions, authorization) with **usability** (sessions, continuations, file support), making it suitable for team environments where Claude needs controlled access to codebases.

Key design principles:
- **Defense in depth**: Multiple security layers
- **Principle of least privilege**: Minimal access by default
- **User control**: Interactive approval for sensitive operations
- **Transparency**: Full audit trail and visibility
- **Simplicity**: Easy setup and configuration

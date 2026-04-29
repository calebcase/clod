package main

import (
	"bufio"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/calebcase/oops"
	"github.com/rs/zerolog"
)

// permBridgeBinary is a statically-linked linux/amd64 build of
// bot/permbridge that claude spawns inside the container for the
// permission-prompt MCP server. Embedding it means we don't have to
// require python3 (or any interpreter) in the user's chosen base image —
// the bot writes the bytes into the per-task runtime dir at startup and
// points claude's --permission-prompt-tool at that path. Rebuild via
// bot/permbridge/build.sh whenever the bridge source changes.
//
//go:embed permbridge/permbridge.linux-amd64
var permBridgeBinary []byte

const (
	// FIFORequestName is the name of the FIFO for permission requests (hook writes, bot reads)
	FIFORequestName = "permission_request.fifo"
	// FIFOResponseName is the name of the FIFO for permission responses (bot writes, hook reads)
	FIFOResponseName = "permission_response.fifo"
	// MCPBridgeName is the filename the embedded bridge binary is written as
	// inside the runtime directory. The container sees it at the same path
	// (the runtime dir is bind-mounted in with its host path).
	MCPBridgeName = "permbridge"
	// MCPConfigName is the name of the MCP config file
	MCPConfigName = "mcp_config.json"
)

// PermissionRequest represents a permission request from the Claude hook.
type PermissionRequest struct {
	SessionID      string                 `json:"session_id"`
	ToolName       string                 `json:"tool_name"`
	ToolInput      map[string]interface{} `json:"tool_input"`
	ToolUseID      string                 `json:"tool_use_id"`
	PermissionMode string                 `json:"permission_mode"`
	CWD            string                 `json:"cwd"`
}

// PermissionResponse represents the bot's response to a permission request.
type PermissionResponse struct {
	Behavior string `json:"behavior"` // "allow" or "deny"
	Message  string `json:"message,omitempty"`
}

// PermissionFIFO manages the FIFO for permission requests/responses.
type PermissionFIFO struct {
	domainPath    string
	runtimeSuffix string
	requestPath   string
	responsePath  string
	contextPath   string // CONTEXT.md if any onboarding sections were written; "" otherwise
	requests      chan PermissionRequest
	responses     chan PermissionResponse
	logger        zerolog.Logger
	cancel        context.CancelFunc
}

// ContextFileName is the filename of the combined onboarding context written
// into the runtime directory. The runner passes this to claude via
// `--append-system-prompt-file` so the workspace + domain READMEs become
// part of the system prompt on every run, regardless of size (CLI argument
// limits would otherwise cap inline `--append-system-prompt` text).
const ContextFileName = "CONTEXT.md"

// NewPermissionFIFO creates and initializes the runtime directory for a
// domain run: the FIFO pair, the embedded permbridge binary, and an
// optional combined onboarding `CONTEXT.md` built from the workspace and
// domain READMEs.
//
// If runtimeSuffix is empty, a random one is generated.
//
// `domainReadmePath` (relative to the domain dir or absolute) and
// `workspaceReadmePath` (absolute) are concatenated into the CONTEXT.md
// with section headers. Either may be missing on disk; missing files
// are silently skipped. If both are empty or both files are missing,
// no CONTEXT.md is written and the runner skips the
// `--append-system-prompt-file` flag.
func NewPermissionFIFO(domainPath string, runtimeSuffix string, domainReadmePath string, workspaceReadmePath string, logger zerolog.Logger) (*PermissionFIFO, error) {
	// Generate random suffix if not provided (for concurrent mode)
	if runtimeSuffix == "" {
		// Generate 6 random hex characters
		randomBytes := make([]byte, 3)
		if _, err := rand.Read(randomBytes); err != nil {
			return nil, oops.Trace(err)
		}
		runtimeSuffix = fmt.Sprintf("%x", randomBytes)
	}

	// Put FIFOs in .clod/runtime-{suffix} for concurrent instances
	runtimeDirName := filepath.Join(".clod", "runtime-"+runtimeSuffix)
	runtimeDir := filepath.Join(domainPath, runtimeDirName)
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		return nil, oops.Trace(err)
	}

	// Build the combined onboarding context. Order:
	//   1. `# Runtime` — bot-internal notice describing the ephemeral
	//      container lifecycle. Always present.
	//   2. `# Workspace context` — workspace-wide guidance.
	//   3. `# Domain: <name>` — domain-specific guidance, last so it can
	//      override workspace-wide on conflict (top-down readers see the
	//      most specific advice last).
	//
	// Claude rejects combining `--append-system-prompt` with
	// `--append-system-prompt-file`, so the runtime notice has to live
	// inside CONTEXT.md alongside the user-authored sections rather than
	// being passed as its own inline `--append-system-prompt` flag.
	var contextBody []byte
	if runtimeNotice := strings.TrimSpace(clodRuntimePrompt); runtimeNotice != "" {
		contextBody = append(contextBody, []byte("# Runtime\n\n")...)
		contextBody = append(contextBody, []byte(runtimeNotice)...)
		contextBody = append(contextBody, '\n')
	}
	if workspaceReadmePath != "" {
		body, err := os.ReadFile(workspaceReadmePath)
		switch {
		case err == nil && len(body) > 0:
			if len(contextBody) > 0 {
				contextBody = append(contextBody, '\n')
			}
			contextBody = append(contextBody, []byte("# Workspace context\n\n")...)
			contextBody = append(contextBody, body...)
			if !endsWithNewline(body) {
				contextBody = append(contextBody, '\n')
			}
		case err != nil && !os.IsNotExist(err):
			return nil, oops.Trace(err)
		case os.IsNotExist(err):
			logger.Debug().Str("path", workspaceReadmePath).Msg("workspace README not found, skipping")
		}
	}
	if domainReadmePath != "" {
		var srcPath string
		if filepath.IsAbs(domainReadmePath) {
			srcPath = domainReadmePath
		} else {
			srcPath = filepath.Join(domainPath, domainReadmePath)
		}
		body, err := os.ReadFile(srcPath)
		switch {
		case err == nil && len(body) > 0:
			if len(contextBody) > 0 {
				contextBody = append(contextBody, '\n')
			}
			contextBody = append(contextBody, []byte(fmt.Sprintf("# Domain: %s\n\n", filepath.Base(domainPath)))...)
			contextBody = append(contextBody, body...)
			if !endsWithNewline(body) {
				contextBody = append(contextBody, '\n')
			}
		case err != nil && !os.IsNotExist(err):
			return nil, oops.Trace(err)
		case os.IsNotExist(err):
			logger.Warn().Str("path", srcPath).Msg("domain README not found, skipping")
		}
	}
	contextPath := ""
	if len(contextBody) > 0 {
		contextPath = filepath.Join(runtimeDir, ContextFileName)
		if err := os.WriteFile(contextPath, contextBody, 0644); err != nil {
			return nil, oops.Trace(err)
		}
		logger.Debug().Str("path", contextPath).Int("bytes", len(contextBody)).Msg("wrote onboarding context")
	}

	requestPath := filepath.Join(runtimeDir, FIFORequestName)
	responsePath := filepath.Join(runtimeDir, FIFOResponseName)

	// Remove existing FIFOs if they exist
	_ = os.Remove(requestPath) // Ignore error if file doesn't exist
	_ = os.Remove(responsePath) // Ignore error if file doesn't exist

	// Write the MCP bridge binary to the runtime directory. The runtime
	// dir is bind-mounted into the container at the same absolute path, so
	// the container sees the binary at the path we write. The Python
	// version of this bridge was embedded at install time; the Go
	// replacement is a static linux/amd64 build so we can drop the python3
	// dependency from the user's base image.
	mcpPath := filepath.Join(runtimeDir, MCPBridgeName)
	if len(permBridgeBinary) == 0 {
		return nil, oops.New("embedded permbridge binary is empty; rebuild via bot/permbridge/build.sh")
	}
	if err := os.WriteFile(mcpPath, permBridgeBinary, 0o755); err != nil {
		return nil, oops.Trace(err)
	}

	// Create the FIFOs
	if err := syscall.Mkfifo(requestPath, 0600); err != nil {
		return nil, oops.Trace(err)
	}

	if err := syscall.Mkfifo(responsePath, 0600); err != nil {
		_ = os.Remove(requestPath) // Cleanup on error, ignore if already removed
		return nil, oops.Trace(err)
	}

	return &PermissionFIFO{
		domainPath:    domainPath,
		runtimeSuffix: runtimeSuffix,
		requestPath:   requestPath,
		responsePath:  responsePath,
		contextPath:   contextPath,
		requests:      make(chan PermissionRequest, 10),
		responses:     make(chan PermissionResponse, 10),
		logger:        logger.With().Str("component", "permission_fifo").Logger(),
	}, nil
}

// endsWithNewline reports whether b ends with '\n'. Tiny helper to keep
// section concatenation tidy.
func endsWithNewline(b []byte) bool {
	return len(b) > 0 && b[len(b)-1] == '\n'
}

// Start begins listening for permission requests and sending responses.
func (p *PermissionFIFO) Start(ctx context.Context) {
	ctx, p.cancel = context.WithCancel(ctx)

	// Read requests from FIFO
	go p.readRequests(ctx)

	// Write responses to FIFO
	go p.writeResponses(ctx)
}

// readRequests reads permission requests from the FIFO.
func (p *PermissionFIFO) readRequests(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Open FIFO for reading (blocks until writer connects)
		// We need to open in non-blocking mode first to allow select to work
		file, err := os.OpenFile(p.requestPath, os.O_RDONLY, 0)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			p.logger.Error().Err(err).Msg("failed to open request FIFO")
			continue
		}

		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}

			var req PermissionRequest
			if err := json.Unmarshal([]byte(line), &req); err != nil {
				p.logger.Error().Err(err).Str("line", line).Msg("failed to parse permission request")
				continue
			}

			p.logger.Info().
				Str("tool_name", req.ToolName).
				Str("tool_use_id", req.ToolUseID).
				Msg("received permission request")

			select {
			case p.requests <- req:
			case <-ctx.Done():
				_ = file.Close()
				return
			}
		}

		if err := file.Close(); err != nil {
			p.logger.Error().Err(err).Msg("failed to close request FIFO")
		}

		if ctx.Err() != nil {
			return
		}
	}
}

// writeResponses writes permission responses to the FIFO.
func (p *PermissionFIFO) writeResponses(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case resp := <-p.responses:
			// Open FIFO for writing (blocks until reader connects)
			file, err := os.OpenFile(p.responsePath, os.O_WRONLY, 0)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				p.logger.Error().Err(err).Msg("failed to open response FIFO")
				continue
			}

			data, err := json.Marshal(resp)
			if err != nil {
				p.logger.Error().Err(err).Msg("failed to marshal response")
				_ = file.Close()
				continue
			}

			_, err = file.Write(append(data, '\n'))
			if err != nil {
				p.logger.Error().Err(err).Msg("failed to write response")
			}

			if err := file.Close(); err != nil {
				p.logger.Error().Err(err).Msg("failed to close response FIFO")
			}

			p.logger.Debug().
				Str("behavior", resp.Behavior).
				Msg("sent permission response")
		}
	}
}

// Requests returns the channel for receiving permission requests.
func (p *PermissionFIFO) Requests() <-chan PermissionRequest {
	return p.requests
}

// SendResponse sends a permission response (non-blocking).
func (p *PermissionFIFO) SendResponse(resp PermissionResponse) {
	select {
	case p.responses <- resp:
		p.logger.Debug().Str("behavior", resp.Behavior).Msg("queued permission response")
	default:
		p.logger.Warn().Str("behavior", resp.Behavior).Msg("response channel full, dropping")
	}
}

// Close cleans up the FIFO.
func (p *PermissionFIFO) Close() {
	if p.cancel != nil {
		p.cancel()
	}

	// Remove the FIFOs
	_ = os.Remove(p.requestPath)  // Ignore error if already removed
	_ = os.Remove(p.responsePath) // Ignore error if already removed

	close(p.requests)
	close(p.responses)

	p.logger.Debug().Msg("permission FIFO closed")
}

// RequestPath returns the path to the request FIFO.
func (p *PermissionFIFO) RequestPath() string {
	return p.requestPath
}

// ResponsePath returns the path to the response FIFO.
func (p *PermissionFIFO) ResponsePath() string {
	return p.responsePath
}

// RuntimeSuffix returns the runtime directory suffix for this FIFO.
func (p *PermissionFIFO) RuntimeSuffix() string {
	return p.runtimeSuffix
}

// ContextPath returns the absolute path to CONTEXT.md (combined workspace
// + domain onboarding) when at least one source README produced content,
// or empty string when neither was present. The runner passes this to
// claude via `--append-system-prompt-file`.
func (p *PermissionFIFO) ContextPath() string {
	return p.contextPath
}

// MCPScriptPath returns the path to the in-container permission bridge
// executable. (Name kept for historical reasons; the bridge is now a Go
// binary written by NewPermissionFIFO.)
func (p *PermissionFIFO) MCPScriptPath() string {
	return filepath.Join(filepath.Dir(p.requestPath), MCPBridgeName)
}

// MCPConfigPath returns the path to the MCP config file.
func (p *PermissionFIFO) MCPConfigPath() string {
	return filepath.Join(filepath.Dir(p.requestPath), MCPConfigName)
}

// CreateMCPConfig generates an MCP config file for the permission server.
// Returns the path to the config file and the tool name for --permission-prompt-tool.
func (p *PermissionFIFO) CreateMCPConfig() (configPath string, toolName string, err error) {
	configPath = p.MCPConfigPath()
	mcpScript := p.MCPScriptPath()

	// MCP server config — claude spawns the bridge binary directly.
	// Running the binary as `command` (no interpreter) is the whole
	// point of replacing the Python version: no host/container package
	// dependency, works on any linux base image that can exec an ELF.
	config := map[string]interface{}{
		"mcpServers": map[string]interface{}{
			"permission": map[string]interface{}{
				"command": mcpScript,
				"args":    []string{},
			},
		},
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return "", "", oops.Trace(err)
	}

	if err := os.WriteFile(configPath, data, 0644); err != nil {
		return "", "", oops.Trace(err)
	}

	// The tool name is mcp__<server>__<tool>
	toolName = "mcp__permission__request_permission"

	p.logger.Debug().
		Str("config_path", configPath).
		Str("mcp_script", mcpScript).
		Str("tool_name", toolName).
		Msg("created MCP config for permission server")

	return configPath, toolName, nil
}

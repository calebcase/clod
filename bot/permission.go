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
	taskPath      string
	runtimeSuffix string
	requestPath   string
	responsePath  string
	requests      chan PermissionRequest
	responses     chan PermissionResponse
	logger        zerolog.Logger
	cancel        context.CancelFunc
}

// NewPermissionFIFO creates and initializes the permission FIFO.
// FIFOs are created in .clod/runtime so they're accessible from inside the
// Docker container where .clod/runtime is mounted read-write.
// If runtimeSuffix is provided, it will be used to create a unique runtime directory.
// If empty, generates a random suffix for concurrent instances.
// If agentsPromptPath is provided and not empty, that file's content is
// copied into the runtime directory as AGENT.md (the per-task prompt).
// If agentsSharedPromptPath is provided and not empty, that file's
// content is copied as AGENTS.md (the workspace-wide prompt). Either
// may be missing on disk; missing files are silently skipped.
func NewPermissionFIFO(taskPath string, runtimeSuffix string, agentsPromptPath string, agentsSharedPromptPath string, logger zerolog.Logger) (*PermissionFIFO, error) {
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
	runtimeDir := filepath.Join(taskPath, runtimeDirName)
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		return nil, oops.Trace(err)
	}

	// Copy the agent prompt file to the runtime directory if configured.
	if agentsPromptPath != "" {
		var srcPath string
		if filepath.IsAbs(agentsPromptPath) {
			srcPath = agentsPromptPath
		} else {
			srcPath = filepath.Join(taskPath, agentsPromptPath)
		}

		promptContent, err := os.ReadFile(srcPath)
		if err != nil {
			if os.IsNotExist(err) {
				logger.Warn().Str("path", srcPath).Msg("agent prompt file not found, skipping")
			} else {
				return nil, oops.Trace(err)
			}
		} else if len(promptContent) > 0 {
			agentMDPath := filepath.Join(runtimeDir, "AGENT.md")
			if err := os.WriteFile(agentMDPath, promptContent, 0644); err != nil {
				return nil, oops.Trace(err)
			}
			logger.Debug().Str("src", srcPath).Str("dst", agentMDPath).Msg("copied agent prompt file")
		}
	}

	// Copy the workspace-wide (shared) prompt. Resolved absolutely
	// upstream in cli.go, so we don't attempt task-relative
	// interpretation here — a relative path reaching this point would
	// resolve against the bot's working directory, which isn't
	// meaningful for this feature.
	if agentsSharedPromptPath != "" {
		sharedContent, err := os.ReadFile(agentsSharedPromptPath)
		if err != nil {
			if os.IsNotExist(err) {
				logger.Debug().Str("path", agentsSharedPromptPath).Msg("shared agent prompt file not found, skipping")
			} else {
				return nil, oops.Trace(err)
			}
		} else if len(sharedContent) > 0 {
			sharedMDPath := filepath.Join(runtimeDir, "AGENTS.md")
			if err := os.WriteFile(sharedMDPath, sharedContent, 0644); err != nil {
				return nil, oops.Trace(err)
			}
			logger.Debug().Str("src", agentsSharedPromptPath).Str("dst", sharedMDPath).Msg("copied shared agent prompt file")
		}
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
		taskPath:      taskPath,
		runtimeSuffix: runtimeSuffix,
		requestPath:   requestPath,
		responsePath:  responsePath,
		requests:      make(chan PermissionRequest, 10),
		responses:     make(chan PermissionResponse, 10),
		logger:        logger.With().Str("component", "permission_fifo").Logger(),
	}, nil
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

// AgentPromptPath returns the path to AGENT.md if it exists, empty string otherwise.
func (p *PermissionFIFO) AgentPromptPath() string {
	agentPath := filepath.Join(filepath.Dir(p.requestPath), "AGENT.md")
	if _, err := os.Stat(agentPath); err == nil {
		return agentPath
	}
	return ""
}

// SharedPromptPath returns the path to AGENTS.md (the workspace-wide
// prompt materialized into the runtime dir) if it exists, empty string
// otherwise. Companion to AgentPromptPath — the runner uses both to
// decide which --append-system-prompt directives to emit.
func (p *PermissionFIFO) SharedPromptPath() string {
	sharedPath := filepath.Join(filepath.Dir(p.requestPath), "AGENTS.md")
	if _, err := os.Stat(sharedPath); err == nil {
		return sharedPath
	}
	return ""
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

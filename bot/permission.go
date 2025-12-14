package main

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"

	"github.com/calebcase/oops"
	"github.com/rs/zerolog"
)

//go:embed permission_hook.py
var permissionHookScript []byte

//go:embed permission_mcp.py
var permissionMCPScript []byte

const (
	// FIFORequestName is the name of the FIFO for permission requests (hook writes, bot reads)
	FIFORequestName = "permission_request.fifo"
	// FIFOResponseName is the name of the FIFO for permission responses (bot writes, hook reads)
	FIFOResponseName = "permission_response.fifo"
	// MCPScriptName is the name of the MCP server script
	MCPScriptName = "permission_mcp.py"
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
	taskPath     string
	requestPath  string
	responsePath string
	requests     chan PermissionRequest
	responses    chan PermissionResponse
	logger       zerolog.Logger
	cancel       context.CancelFunc
}

// HookScriptName is the name of the permission hook script.
const HookScriptName = "permission_hook.py"

// NewPermissionFIFO creates and initializes the permission FIFO.
// FIFOs are created in the task directory (not .clod) so they're accessible
// from inside the Docker container where .clod is mounted read-only.
func NewPermissionFIFO(taskPath string, logger zerolog.Logger) (*PermissionFIFO, error) {
	// Put FIFOs in .clod-runtime which will be writable from inside container
	runtimeDir := filepath.Join(taskPath, ".clod-runtime")
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		return nil, oops.Trace(err)
	}
	requestPath := filepath.Join(runtimeDir, FIFORequestName)
	responsePath := filepath.Join(runtimeDir, FIFOResponseName)

	// Remove existing FIFOs if they exist
	os.Remove(requestPath)
	os.Remove(responsePath)

	// Write the hook script to the runtime directory (legacy, kept for compatibility)
	hookPath := filepath.Join(runtimeDir, HookScriptName)
	if err := os.WriteFile(hookPath, permissionHookScript, 0755); err != nil {
		return nil, oops.Trace(err)
	}

	// Write the MCP server script to the runtime directory
	mcpPath := filepath.Join(runtimeDir, MCPScriptName)
	if err := os.WriteFile(mcpPath, permissionMCPScript, 0755); err != nil {
		return nil, oops.Trace(err)
	}

	// Create the FIFOs
	if err := syscall.Mkfifo(requestPath, 0600); err != nil {
		return nil, oops.Trace(err)
	}

	if err := syscall.Mkfifo(responsePath, 0600); err != nil {
		os.Remove(requestPath)
		return nil, oops.Trace(err)
	}

	return &PermissionFIFO{
		taskPath:     taskPath,
		requestPath:  requestPath,
		responsePath: responsePath,
		requests:     make(chan PermissionRequest, 10),
		responses:    make(chan PermissionResponse, 10),
		logger:       logger.With().Str("component", "permission_fifo").Logger(),
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
				file.Close()
				return
			}
		}

		file.Close()

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
				file.Close()
				continue
			}

			_, err = file.Write(append(data, '\n'))
			if err != nil {
				p.logger.Error().Err(err).Msg("failed to write response")
			}

			file.Close()

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
	os.Remove(p.requestPath)
	os.Remove(p.responsePath)

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

// HookScriptPath returns the path to the permission hook script.
func (p *PermissionFIFO) HookScriptPath() string {
	return filepath.Join(filepath.Dir(p.requestPath), HookScriptName)
}

// SettingsPath returns the path to the generated settings file with hook configuration.
// This file can be passed to claude via --settings flag.
func (p *PermissionFIFO) SettingsPath() string {
	return filepath.Join(filepath.Dir(p.requestPath), "settings.json")
}

// CreateSettings generates a settings.json file with the permission hook configured.
// Returns the path to the settings file which can be passed via --settings flag.
func (p *PermissionFIFO) CreateSettings() (string, error) {
	settingsPath := p.SettingsPath()
	hookCmd := p.HookScriptPath()

	settings := map[string]interface{}{
		"hooks": map[string]interface{}{
			"PermissionRequest": []map[string]interface{}{
				{
					"matcher": "*",
					"hooks": []map[string]interface{}{
						{
							"type":    "command",
							"command": hookCmd,
						},
					},
				},
			},
		},
	}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return "", oops.Trace(err)
	}

	if err := os.WriteFile(settingsPath, data, 0644); err != nil {
		return "", oops.Trace(err)
	}

	p.logger.Debug().
		Str("settings_path", settingsPath).
		Str("hook_command", hookCmd).
		Msg("created settings file with permission hook")

	return settingsPath, nil
}

// MCPScriptPath returns the path to the MCP server script.
func (p *PermissionFIFO) MCPScriptPath() string {
	return filepath.Join(filepath.Dir(p.requestPath), MCPScriptName)
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

	// MCP server config
	config := map[string]interface{}{
		"mcpServers": map[string]interface{}{
			"permission": map[string]interface{}{
				"command": "python3",
				"args":    []string{mcpScript},
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

// Command permbridge is the in-container half of clod's permission prompt
// system. Claude spawns this binary via `--permission-prompt-tool` plus an
// `--mcp-config` that points to it. permbridge speaks JSON-RPC MCP on
// stdin/stdout, advertises a single `request_permission` tool, and forwards
// each invocation over the pair of FIFOs in CLOD_RUNTIME_DIR to the bot
// process running on the host. The bot reads the request, asks the user in
// Slack, and writes back an allow/deny decision that permbridge marshals as
// the tool result.
//
// Design goal: run in *any* Linux container image without extra packages.
// The bot used to ship a Python version of this bridge, which forced every
// Dockerfile to carry python3 just so the bot could function — a fresh task
// with a minimal `ubuntu:24.04` base would fail with "MCP tools: none"
// before the user saw anything. This replacement is built statically
// (CGO_ENABLED=0 GOOS=linux GOARCH=amd64) and embedded into the bot
// binary; the bot writes it into the per-task runtime dir and marks it
// executable at startup.
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	fifoRequestName  = "permission_request.fifo"
	fifoResponseName = "permission_response.fifo"
	toolName         = "request_permission"
)

// jsonrpcResponse models the shape of JSON-RPC 2.0 responses we write back
// on stdout. `Result` and `Error` are both json.RawMessage so the caller
// can marshal arbitrary structures without extra plumbing.
type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// jsonrpcRequest models the incoming method calls. params stays as
// RawMessage so we only fully decode it for the method we care about.
type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[permbridge] "+format+"\n", args...)
}

func writeResponse(r jsonrpcResponse) {
	r.JSONRPC = "2.0"
	enc, err := json.Marshal(r)
	if err != nil {
		logf("marshal response: %v", err)
		return
	}
	// MCP requires one JSON document per line on stdout.
	fmt.Println(string(enc))
}

func respondResult(id json.RawMessage, result any) {
	raw, err := json.Marshal(result)
	if err != nil {
		logf("marshal result: %v", err)
		return
	}
	writeResponse(jsonrpcResponse{ID: id, Result: raw})
}

func respondError(id json.RawMessage, code int, message string) {
	writeResponse(jsonrpcResponse{ID: id, Error: &jsonrpcError{Code: code, Message: message}})
}

// initializeResult mirrors what the Python bridge advertised; claude is
// tolerant of minor variation but we keep the exact shape for parity.
func handleInitialize(id json.RawMessage) {
	respondResult(id, map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    "permission-mcp",
			"version": "1.0.0",
		},
	})
}

func handleToolsList(id json.RawMessage) {
	respondResult(id, map[string]any{
		"tools": []map[string]any{
			{
				"name":        toolName,
				"description": "Request permission from the user for a tool operation",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"tool_name": map[string]any{
							"type":        "string",
							"description": "Name of the tool requesting permission",
						},
						"input": map[string]any{
							"type":        "object",
							"description": "Input parameters for the tool",
						},
					},
					"required": []string{"tool_name", "input"},
				},
			},
		},
	})
}

// toolsCallParams unpacks just the fields we use.
type toolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type permissionArguments struct {
	ToolName string          `json:"tool_name"`
	Input    json.RawMessage `json:"input"`
}

// botResponse is the wire format the bot writes back on the response FIFO.
type botResponse struct {
	Behavior string `json:"behavior"`
	Message  string `json:"message"`
}

// denyResult formats a tool result that signals deny.
func denyResult(message string) map[string]any {
	payload, _ := json.Marshal(map[string]any{
		"behavior": "deny",
		"message":  message,
	})
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": string(payload)},
		},
	}
}

// allowResult formats a tool result that signals allow and echoes the
// original input back as updatedInput (claude requires this field for the
// allow branch).
func allowResult(input json.RawMessage) map[string]any {
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	payload, _ := json.Marshal(map[string]any{
		"behavior":     "allow",
		"updatedInput": json.RawMessage(input),
	})
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": string(payload)},
		},
	}
}

func handleToolCall(id json.RawMessage, rawParams json.RawMessage) {
	var params toolsCallParams
	if err := json.Unmarshal(rawParams, &params); err != nil {
		respondError(id, -32602, fmt.Sprintf("invalid params: %v", err))
		return
	}
	if params.Name != toolName {
		respondError(id, -32601, fmt.Sprintf("unknown tool: %s", params.Name))
		return
	}
	var args permissionArguments
	if err := json.Unmarshal(params.Arguments, &args); err != nil {
		respondError(id, -32602, fmt.Sprintf("invalid arguments: %v", err))
		return
	}

	runtimeDir := os.Getenv("CLOD_RUNTIME_DIR")
	if runtimeDir == "" {
		logf("CLOD_RUNTIME_DIR not set; defaulting to deny")
		respondResult(id, denyResult("Permission system not available (CLOD_RUNTIME_DIR unset)"))
		return
	}
	requestFIFO := filepath.Join(runtimeDir, fifoRequestName)
	responseFIFO := filepath.Join(runtimeDir, fifoResponseName)

	// Per-request correlation tag. Pairs with the bot's resp_id
	// so log lines from a hung FIFO open can be matched across
	// process boundaries. The June 2026 deadlock had both sides
	// blocked in wait_for_partner on the same path; without
	// per-request ids the cause was unrecoverable from the logs.
	reqID := shortHexID()
	reqStat := fifoStat(requestFIFO)
	respStat := fifoStat(responseFIFO)
	logf("[req=%s] permission requested tool=%s req_fifo=%s req_stat=%s resp_fifo=%s resp_stat=%s",
		reqID, args.ToolName, requestFIFO, reqStat, responseFIFO, respStat)

	if reqStat == "missing" {
		respondResult(id, denyResult("Permission system not available (request FIFO missing)"))
		return
	}
	if respStat == "missing" {
		respondResult(id, denyResult("Permission system not available (response FIFO missing)"))
		return
	}

	// Build the request the bot expects and send it over the request FIFO.
	// Opening a FIFO for write blocks until a reader is present (the bot);
	// same for the response FIFO.
	reqPayload, err := json.Marshal(map[string]any{
		"tool_name":  args.ToolName,
		"tool_input": json.RawMessage(args.Input),
	})
	if err != nil {
		respondResult(id, denyResult(fmt.Sprintf("Marshal request: %v", err)))
		return
	}

	logf("[req=%s] opening request FIFO for write", reqID)
	openStart := time.Now()
	openDone := make(chan struct{})
	go pingBlocked(openDone, "[req="+reqID+"] still blocked opening request FIFO", openStart)
	reqFile, err := os.OpenFile(requestFIFO, os.O_WRONLY, 0)
	close(openDone)
	if err != nil {
		logf("[req=%s] open request FIFO failed after %s: %v", reqID, time.Since(openStart), err)
		respondResult(id, denyResult(fmt.Sprintf("Open request FIFO: %v", err)))
		return
	}
	logf("[req=%s] opened request FIFO after %s; writing payload (%d bytes)", reqID, time.Since(openStart), len(reqPayload)+1)
	if _, err := reqFile.Write(append(reqPayload, '\n')); err != nil {
		_ = reqFile.Close()
		logf("[req=%s] write request FIFO: %v", reqID, err)
		respondResult(id, denyResult(fmt.Sprintf("Write request FIFO: %v", err)))
		return
	}
	_ = reqFile.Close()
	logf("[req=%s] request FIFO closed; opening response FIFO for read", reqID)

	respOpenStart := time.Now()
	respOpenDone := make(chan struct{})
	go pingBlocked(respOpenDone, "[req="+reqID+"] still blocked opening response FIFO", respOpenStart)
	respFile, err := os.Open(responseFIFO)
	close(respOpenDone)
	if err != nil {
		logf("[req=%s] open response FIFO failed after %s: %v", reqID, time.Since(respOpenStart), err)
		respondResult(id, denyResult(fmt.Sprintf("Open response FIFO: %v", err)))
		return
	}
	defer respFile.Close()
	logf("[req=%s] opened response FIFO after %s; waiting for line", reqID, time.Since(respOpenStart))

	scanner := bufio.NewScanner(respFile)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	readStart := time.Now()
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			logf("[req=%s] read response FIFO failed after %s: %v", reqID, time.Since(readStart), err)
		} else {
			logf("[req=%s] empty response from bot after %s", reqID, time.Since(readStart))
		}
		respondResult(id, denyResult("Empty response from permission system"))
		return
	}
	line := scanner.Text()
	logf("[req=%s] read %d-byte response after %s", reqID, len(line), time.Since(readStart))

	var resp botResponse
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		logf("parse bot response %q: %v", line, err)
		respondResult(id, denyResult(fmt.Sprintf("Bad response from permission system: %v", err)))
		return
	}

	logf("bot decision: behavior=%s", resp.Behavior)

	if resp.Behavior == "allow" {
		respondResult(id, allowResult(args.Input))
		return
	}
	msg := resp.Message
	if msg == "" {
		msg = "Permission denied by user"
	}
	respondResult(id, denyResult(msg))
}

func main() {
	logf("starting permission MCP bridge")
	reader := bufio.NewScanner(os.Stdin)
	reader.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for reader.Scan() {
		line := reader.Bytes()
		if len(line) == 0 {
			continue
		}
		var req jsonrpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			logf("invalid JSON: %v", err)
			continue
		}
		logf("received method: %s", req.Method)
		switch req.Method {
		case "initialize":
			handleInitialize(req.ID)
		case "notifications/initialized":
			// no response required
		case "tools/list":
			handleToolsList(req.ID)
		case "tools/call":
			handleToolCall(req.ID, req.Params)
		default:
			logf("unknown method: %s", req.Method)
			if len(req.ID) > 0 && string(req.ID) != "null" {
				respondError(req.ID, -32601, fmt.Sprintf("method not found: %s", req.Method))
			}
		}
	}
	if err := reader.Err(); err != nil {
		logf("stdin scanner: %v", err)
	}
}

// shortHexID returns an 8-character random hex string to tag log
// lines for one MCP request_permission call. Pairs with the bot's
// resp_id when comparing the two halves of the FIFO handshake.
func shortHexID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "????????"
	}
	return fmt.Sprintf("%x", b[:])
}

// fifoStat returns a short human-readable description of a FIFO at
// `path`: "missing" if the path doesn't exist, "not-fifo" if it's
// a regular file/symlink/etc, or "fifo inode=N dev=N mode=...". The
// inode/dev pair is what we'd compare across processes to detect a
// path that was re-created behind our back; mode catches the
// chmod-stripped-the-write-bit class of misconfiguration.
func fifoStat(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "missing"
		}
		return fmt.Sprintf("stat-error:%v", err)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Sprintf("mode=%s (non-unix sys)", st.Mode())
	}
	kind := "regular"
	if st.Mode()&os.ModeNamedPipe != 0 {
		kind = "fifo"
	}
	return fmt.Sprintf("%s inode=%d dev=%d mode=%s", kind, sys.Ino, sys.Dev, st.Mode())
}

// pingBlocked logs a "still blocked" notice every 30s until done is
// closed. Intended to be launched in a goroutine before a syscall
// expected to be fast but known to wedge under certain failure
// modes (FIFO opens with no rendezvous partner). The launching
// goroutine MUST close `done` after the syscall returns or this
// will leak.
func pingBlocked(done <-chan struct{}, msg string, start time.Time) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			logf("%s (blocked for %s)", msg, time.Since(start))
		}
	}
}

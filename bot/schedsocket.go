package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/calebcase/oops"
	"github.com/rs/zerolog"
)

// This file wires the SchedulingRegistry to a per-session Unix socket
// that the in-container `schedbridge` MCP shim connects to. On the
// wire we speak JSON-lines JSON-RPC 2.0, matching MCP's transport
// shape — tools/list and tools/call are the only methods that
// actually do anything; initialize is a stub for MCP client
// niceness.
//
// One socket per session (mirrors permbridge's per-session FIFO
// pattern). The bot creates the socket in the session's runtime dir
// when the session starts; schedbridge inside the container connects
// to that same path via the runtime-dir bind mount.
//
// v1 of mcp-shim.md §4.2 picked Unix socket over the FIFO trio for
// bidirectional messaging with less framing ceremony.

// SchedSocket is one per-session Unix-socket listener. Owns its
// accept goroutine and cleans up its socket file on Stop.
type SchedSocket struct {
	sessionKey string
	domainDir  string // for cwd validation on cron_create
	socketPath string
	listener   net.Listener
	reg        *SchedulingRegistry
	logger     zerolog.Logger

	closed atomic.Bool
	wg     sync.WaitGroup
}

// StartSocket opens a Unix socket in runtimeDir and begins accepting
// connections from the in-container schedbridge shim. domainDir is
// the session's project dir; every cwd passed via cron_create is
// verified to sit under it.
func (r *SchedulingRegistry) StartSocket(sessionKey, runtimeDir, domainDir string) (*SchedSocket, error) {
	if sessionKey == "" || runtimeDir == "" || domainDir == "" {
		return nil, oops.Trace(fmt.Errorf("StartSocket requires sessionKey, runtimeDir, domainDir"))
	}
	socketPath := filepath.Join(runtimeDir, "schedbridge.sock")

	// Remove any leftover socket from a prior aborted run. Fine to
	// ignore ENOENT; anything else is a real problem.
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return nil, oops.Trace(fmt.Errorf("remove stale socket: %w", err))
	}

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, oops.Trace(err)
	}
	// 0600: only the bot user (owner of the runtime dir) can talk to
	// this socket. Container process runs as the same UID (see docker
	// run's --user, matching host uid), so it still connects fine.
	if err := os.Chmod(socketPath, 0600); err != nil {
		ln.Close()
		return nil, oops.Trace(err)
	}

	s := &SchedSocket{
		sessionKey: sessionKey,
		domainDir:  domainDir,
		socketPath: socketPath,
		listener:   ln,
		reg:        r,
		logger: r.logger.With().
			Str("component", "sched_socket").
			Str("session_key", sessionKey).
			Str("socket", socketPath).
			Logger(),
	}

	s.wg.Add(1)
	go s.acceptLoop()

	s.logger.Info().Msg("schedbridge socket listening")
	return s, nil
}

// Stop closes the listener, removes the socket file, and waits for
// the accept goroutine to exit. Safe to call more than once.
func (s *SchedSocket) Stop() {
	if !s.closed.CompareAndSwap(false, true) {
		return
	}
	_ = s.listener.Close()
	_ = os.Remove(s.socketPath)
	s.wg.Wait()
	s.logger.Info().Msg("schedbridge socket closed")
}

// acceptLoop pulls new connections off the listener until it's
// closed. Each connection gets its own goroutine so multiple
// concurrent tool calls from the shim don't head-of-line block.
// Every accepted connection logs at Info; the counter tags per-
// connection log lines so a flapping schedbridge (repeatedly
// dialing + hanging up) is obvious in the log without goroutine
// forensics.
func (s *SchedSocket) acceptLoop() {
	defer s.wg.Done()
	var connSeq uint64
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if s.closed.Load() {
				return
			}
			s.logger.Warn().Err(err).Msg("accept error; retrying")
			time.Sleep(100 * time.Millisecond)
			continue
		}
		connSeq++
		id := connSeq
		s.logger.Info().Uint64("conn_id", id).Msg("schedbridge connected")
		s.wg.Add(1)
		go func(c net.Conn, cid uint64) {
			defer s.wg.Done()
			s.handleConn(c, cid)
		}(conn, id)
	}
}

// handleConn reads JSON-lines JSON-RPC 2.0 requests from conn,
// dispatches each to the tool router, and writes responses back
// on the same connection. One request per line; \n-delimited.
// Logs each dispatched method at Info so a flapping or
// misbehaving MCP client is diagnosable from the log alone.
func (s *SchedSocket) handleConn(conn net.Conn, connID uint64) {
	defer conn.Close()
	connLog := s.logger.With().Uint64("conn_id", connID).Logger()
	start := time.Now()
	var msgs uint64
	defer func() {
		connLog.Info().
			Uint64("messages", msgs).
			Dur("lifetime", time.Since(start)).
			Msg("schedbridge disconnected")
	}()

	// bufio.Reader with a generous buffer for very long tool_call
	// input schemas (e.g. long shell commands in cron_create).
	reader := bufio.NewReaderSize(conn, 64*1024)
	encoder := json.NewEncoder(conn)

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			msgs++
			// Peek method for the log — dispatch parses again;
			// cheap because lines are small JSON-RPC docs.
			var peek jsonrpcReq
			_ = json.Unmarshal(line, &peek)
			method := peek.Method
			if method == "" {
				method = "<parse-error>"
			}
			connLog.Info().Str("method", method).Msg("schedbridge dispatch")
			resp := s.dispatch(line)
			if resp != nil {
				if err := encoder.Encode(resp); err != nil {
					connLog.Warn().Err(err).Msg("schedbridge write response failed; closing")
					return
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				connLog.Info().Err(err).Msg("schedbridge read error; closing")
			}
			return
		}
	}
}

// jsonrpcReq / jsonrpcResp mirror the MCP wire shape. Params/result
// stay as RawMessage so we only unmarshal per-method.
type jsonrpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonrpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcErr     `json:"error,omitempty"`
}

type jsonrpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// dispatch parses one JSON-RPC line and returns the response envelope
// (nil for notifications, which don't get a reply). Errors are
// converted to JSON-RPC error responses so the shim sees a valid
// message either way.
func (s *SchedSocket) dispatch(line []byte) *jsonrpcResp {
	var req jsonrpcReq
	if err := json.Unmarshal(line, &req); err != nil {
		return errResp(nil, -32700, "parse error: "+err.Error())
	}
	// A missing id means notification; MCP spec says no response.
	isNotification := len(req.ID) == 0 || string(req.ID) == "null"

	if req.JSONRPC != "" && req.JSONRPC != "2.0" {
		if isNotification {
			return nil
		}
		return errResp(req.ID, -32600, "unsupported jsonrpc version")
	}

	switch req.Method {
	case "initialize":
		return okResp(req.ID, initializeResult())
	case "tools/list":
		return okResp(req.ID, toolsListResult())
	case "tools/call":
		return s.handleToolsCall(req.ID, req.Params)
	case "notifications/initialized":
		// MCP handshake completion notification — no response.
		return nil
	default:
		if isNotification {
			return nil
		}
		return errResp(req.ID, -32601, "method not found: "+req.Method)
	}
}

// initializeResult is a minimal MCP initialize response. Advertising
// tools is the only capability we need for phase 1.
func initializeResult() json.RawMessage {
	body, _ := json.Marshal(map[string]any{
		"protocolVersion": "2024-11-05",
		"serverInfo": map[string]any{
			"name":    "clod-schedbridge",
			"version": "0.1.0",
		},
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
	})
	return body
}

// toolsListResult returns the phase-1 cron tool schemas. Kept as
// literal Go maps so schema drift is easy to spot in code review.
func toolsListResult() json.RawMessage {
	tools := []map[string]any{
		{
			"name": "cron_create",
			"description": "Schedule a shell command to run on a recurring interval, driven by the bot on the host. Survives container restarts and turn-end-waiting — unlike Claude's native CronCreate which stops firing when a turn ends with no outstanding tool call. Use this for any interval work that must keep advancing across turns. The command runs via `bash -c` on the host, outside the docker container.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"spec":        map[string]any{"type": "string", "description": "Cron expression (5-field) or shorthand: '@every 5m', '@hourly', '@daily'. Minimum interval 30s."},
					"command":     map[string]any{"type": "string", "description": "Shell command to execute at each tick, via `bash -c`."},
					"cwd":         map[string]any{"type": "string", "description": "Working directory. Must be under the session's domain dir. Defaults to the domain dir if omitted."},
					"description": map[string]any{"type": "string", "description": "Human-readable label. Shows up in cron_list and log headers."},
				},
				"required": []string{"spec", "command", "description"},
			},
		},
		{
			"name":        "cron_list",
			"description": "List active crons for this session, including last-fire timestamp, exit code, and log path. Read the log_path file to inspect a cron's captured stdout+stderr.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			"name":        "cron_delete",
			"description": "Cancel and remove a scheduled cron by id. The id comes from cron_create's return value or cron_list.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{"type": "string", "description": "Cron id (form 'cron_<8-hex>')."},
				},
				"required": []string{"id"},
			},
		},
	}
	body, _ := json.Marshal(map[string]any{"tools": tools})
	return body
}

// callParams is the tools/call params envelope. Arguments varies by
// tool — we RawMessage it and unmarshal per branch.
type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

func (s *SchedSocket) handleToolsCall(id json.RawMessage, raw json.RawMessage) *jsonrpcResp {
	var p callParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return errResp(id, -32602, "invalid params: "+err.Error())
	}
	switch p.Name {
	case "cron_create":
		return s.callCronCreate(id, p.Arguments)
	case "cron_list":
		return s.callCronList(id)
	case "cron_delete":
		return s.callCronDelete(id, p.Arguments)
	default:
		return errResp(id, -32601, "unknown tool: "+p.Name)
	}
}

// toolResultText wraps a plain-text message in the MCP tools/call
// result shape. Errors set isError so the client can surface them.
func toolResultText(text string, isError bool) json.RawMessage {
	body, _ := json.Marshal(map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": text},
		},
		"isError": isError,
	})
	return body
}

func okResp(id json.RawMessage, result json.RawMessage) *jsonrpcResp {
	return &jsonrpcResp{JSONRPC: "2.0", ID: id, Result: result}
}

func errResp(id json.RawMessage, code int, msg string) *jsonrpcResp {
	return &jsonrpcResp{JSONRPC: "2.0", ID: id, Error: &jsonrpcErr{Code: code, Message: msg}}
}

// Tool call parameter types — separate from json.RawMessage so per-
// tool argument shapes are documented in Go.

type cronCreateArgs struct {
	Spec        string `json:"spec"`
	Command     string `json:"command"`
	CWD         string `json:"cwd"`
	Description string `json:"description"`
}

type cronDeleteArgs struct {
	ID string `json:"id"`
}

// callCronCreate implements the cron_create tool. Validates spec and
// cwd, adds to the registry (which starts the ticker), persists, and
// returns the newly-assigned id + next-fire projection.
func (s *SchedSocket) callCronCreate(id json.RawMessage, raw json.RawMessage) *jsonrpcResp {
	var a cronCreateArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return okResp(id, toolResultText("invalid arguments: "+err.Error(), true))
	}
	if a.Spec == "" || a.Command == "" || a.Description == "" {
		return okResp(id, toolResultText("cron_create requires spec, command, and description", true))
	}

	// Resolve cwd (defaults to domain dir) and enforce containment.
	cwd, err := ValidateCWDUnderDomain(a.CWD, s.domainDir)
	if err != nil {
		return okResp(id, toolResultText("cwd rejected: "+err.Error(), true))
	}

	entry := &CronEntry{
		SessionKey:  s.sessionKey,
		Spec:        a.Spec,
		Mode:        "command",
		Command:     a.Command,
		CWD:         cwd,
		Description: a.Description,
	}
	if err := s.reg.Add(entry); err != nil {
		return okResp(id, toolResultText("cron_create failed: "+err.Error(), true))
	}
	if err := s.reg.Save(); err != nil {
		// Persistence failed but the entry is scheduled. Log and
		// let the caller know; next successful save reconciles.
		s.logger.Warn().Err(err).Str("cron_id", entry.ID).Msg("cron_create: save failed")
	}

	// Read back next-fire from the entry (scheduleByID populates it).
	nextFire := ""
	if e := s.reg.Get(entry.ID); e != nil && !e.NextFireAt.IsZero() {
		nextFire = e.NextFireAt.UTC().Format(time.RFC3339)
	}
	body, _ := json.Marshal(map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": fmt.Sprintf(
				"cron scheduled\n id: %s\n spec: %s\n cwd: %s\n log: %s\n next: %s",
				entry.ID, entry.Spec, entry.CWD, entry.LastOutputPath, nextFire,
			)},
		},
		"structuredContent": map[string]any{
			"id":            entry.ID,
			"spec":          entry.Spec,
			"cwd":           entry.CWD,
			"log_path":      entry.LastOutputPath,
			"next_fire_at":  nextFire,
			"description":   entry.Description,
		},
	})
	return okResp(id, body)
}

// callCronList returns the current crons for this session as both a
// human-readable summary and structured content.
func (s *SchedSocket) callCronList(id json.RawMessage) *jsonrpcResp {
	entries := s.reg.ListForSession(s.sessionKey)
	structured := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		item := map[string]any{
			"id":          e.ID,
			"spec":        e.Spec,
			"cwd":         e.CWD,
			"description": e.Description,
			"log_path":    e.LastOutputPath,
		}
		if !e.LastFireAt.IsZero() {
			item["last_fire_at"] = e.LastFireAt.UTC().Format(time.RFC3339)
			item["last_exit_code"] = e.LastExitCode
			if e.LastError != "" {
				item["last_error"] = e.LastError
			}
		}
		if !e.NextFireAt.IsZero() {
			item["next_fire_at"] = e.NextFireAt.UTC().Format(time.RFC3339)
		}
		structured = append(structured, item)
	}

	// Human summary
	var text string
	if len(entries) == 0 {
		text = "no active crons in this session"
	} else {
		var lines []string
		for _, e := range entries {
			line := fmt.Sprintf("- %s [%s] %s — %s", e.ID, e.Spec, e.Description, e.LastOutputPath)
			if !e.LastFireAt.IsZero() {
				line += fmt.Sprintf(" (last: %s, exit=%d)", e.LastFireAt.UTC().Format(time.RFC3339), e.LastExitCode)
			}
			lines = append(lines, line)
		}
		text = fmt.Sprintf("%d active cron(s):\n%s", len(entries), joinLines(lines))
	}

	body, _ := json.Marshal(map[string]any{
		"content":           []map[string]any{{"type": "text", "text": text}},
		"structuredContent": map[string]any{"crons": structured},
	})
	return okResp(id, body)
}

// callCronDelete cancels and removes a cron by id.
func (s *SchedSocket) callCronDelete(id json.RawMessage, raw json.RawMessage) *jsonrpcResp {
	var a cronDeleteArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return okResp(id, toolResultText("invalid arguments: "+err.Error(), true))
	}
	if a.ID == "" {
		return okResp(id, toolResultText("cron_delete requires id", true))
	}

	// Only allow deletion of crons owned by this session — a cron_id
	// from a different session shouldn't be reachable, but check to
	// keep the isolation invariant airtight.
	existing := s.reg.Get(a.ID)
	if existing == nil {
		return okResp(id, toolResultText("no cron with id "+a.ID, true))
	}
	if existing.SessionKey != s.sessionKey {
		return okResp(id, toolResultText("cron "+a.ID+" is not owned by this session", true))
	}

	if !s.reg.Delete(a.ID) {
		return okResp(id, toolResultText("cron_delete: no such id (race?)", true))
	}
	if err := s.reg.Save(); err != nil {
		s.logger.Warn().Err(err).Str("cron_id", a.ID).Msg("cron_delete: save failed")
	}

	body, _ := json.Marshal(map[string]any{
		"content":           []map[string]any{{"type": "text", "text": "deleted cron " + a.ID}},
		"structuredContent": map[string]any{"deleted": true, "id": a.ID},
	})
	return okResp(id, body)
}

// joinLines is a tiny local helper to keep the tool result formatting
// legible without pulling in strings.Join at every call site.
func joinLines(lines []string) string {
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n"
		}
		out += l
	}
	return out
}

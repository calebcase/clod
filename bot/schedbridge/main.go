// Command schedbridge is the in-container half of clod's scheduling
// MCP shim. Claude (or any MCP-speaking agent) spawns this binary via
// its --mcp-config; schedbridge speaks JSON-lines JSON-RPC 2.0 on
// stdin/stdout, and forwards every message over a Unix domain socket
// to the bot process running on the host.
//
// The bot creates the socket at $CLOD_RUNTIME_DIR/schedbridge.sock and
// bind-mounts the runtime dir into the container, so this process can
// dial it as an ordinary path. All tool advertising (tools/list) and
// dispatch (tools/call) happens on the bot side — this binary is a
// pure transparent forwarder, which means adding new scheduling tools
// later doesn't require redeploying schedbridge.
//
// Design goals mirror permbridge: (a) run inside *any* Linux container
// image without extra packages, (b) build statically so no libc is
// needed, (c) fail loudly with a synthesized JSON-RPC error rather
// than a silent hang if the socket isn't reachable.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"
)

const socketName = "schedbridge.sock"

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[schedbridge] "+format+"\n", args...)
}

// socketPath resolves the Unix socket path from CLOD_RUNTIME_DIR.
// This env var is set by the bot when it spawns the container,
// matching how permbridge picks up its FIFO pair.
func socketPath() (string, error) {
	dir := os.Getenv("CLOD_RUNTIME_DIR")
	if dir == "" {
		return "", fmt.Errorf("CLOD_RUNTIME_DIR is not set")
	}
	return filepath.Join(dir, socketName), nil
}

// dialWithRetry retries a few times with backoff because the bot may
// still be finishing socket setup when the container spawns. Cap total
// wait at ~5s so genuine misconfigurations surface promptly.
func dialWithRetry(path string) (net.Conn, error) {
	var lastErr error
	backoffs := []time.Duration{
		50 * time.Millisecond,
		100 * time.Millisecond,
		250 * time.Millisecond,
		500 * time.Millisecond,
		1 * time.Second,
		1500 * time.Millisecond,
		1500 * time.Millisecond,
	}
	for _, d := range backoffs {
		conn, err := net.Dial("unix", path)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		time.Sleep(d)
	}
	return nil, fmt.Errorf("dial %s: %w", path, lastErr)
}

// synthErrorResponse writes a JSON-RPC error to stdout, keyed to the
// request's id. Called when we can't reach the bot at all — better
// than a silent stall in the agent's tools/list loop.
func synthErrorResponse(id json.RawMessage, msg string) {
	out, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    -32603,
			"message": msg,
		},
	})
	fmt.Println(string(out))
}

func main() {
	path, err := socketPath()
	if err != nil {
		logf("%v", err)
		os.Exit(1)
	}

	conn, err := dialWithRetry(path)
	if err != nil {
		logf("could not connect to %s: %v", path, err)
		// Don't os.Exit — the agent expects a well-formed MCP process
		// at least long enough to read one line. Serve a synthetic
		// error to whatever request comes first, then bail on EOF.
		serveDead(err)
		return
	}
	defer conn.Close()

	// Two goroutines: stdin → socket, socket → stdout. Whichever side
	// hits EOF first tears down the other. Match sensible buffer size
	// for long tool inputs (e.g. multi-line shell commands).
	done := make(chan struct{}, 2)

	go func() {
		defer func() { done <- struct{}{} }()
		if err := forwardLines(os.Stdin, conn, "in→sock"); err != nil {
			logf("in→sock: %v", err)
		}
		// Half-close so the bot sees EOF and drops its handler.
		if uc, ok := conn.(*net.UnixConn); ok {
			_ = uc.CloseWrite()
		}
	}()

	go func() {
		defer func() { done <- struct{}{} }()
		if err := forwardLines(conn, os.Stdout, "sock→out"); err != nil {
			logf("sock→out: %v", err)
		}
	}()

	<-done
}

// forwardLines copies newline-delimited JSON documents from src to
// dst, one at a time, preserving message boundaries. bufio.Reader
// makes ReadBytes efficient; the write side flushes per line so the
// bot doesn't stall waiting for a partial JSON-RPC document.
func forwardLines(src io.Reader, dst io.Writer, label string) error {
	r := bufio.NewReaderSize(src, 64*1024)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := dst.Write(line); werr != nil {
				return fmt.Errorf("write (%s): %w", label, werr)
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("read (%s): %w", label, err)
		}
	}
}

// serveDead handles the "socket never came up" case by responding
// with a synthetic MCP error to the first request we see, then
// exiting cleanly. Better UX than a mysterious hang; the agent's
// tools/list gets a well-formed error message.
func serveDead(reason error) {
	r := bufio.NewReader(os.Stdin)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var req struct {
				ID json.RawMessage `json:"id,omitempty"`
			}
			_ = json.Unmarshal(line, &req)
			synthErrorResponse(req.ID, "schedbridge cannot reach bot socket: "+reason.Error())
		}
		if err != nil {
			return
		}
	}
}

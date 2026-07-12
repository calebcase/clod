// Command clodproxy is the in-container HTTP → HTTPS reverse proxy
// that stands between claude and api.anthropic.com. Two responsibilities:
//
//  1. Force turnover of the outbound keep-alive pool to Anthropic so
//     claude doesn't wedge on a silently half-closed socket. See the
//     upstream claude-code #54434 write-up.
//  2. Enforce a body-idle watchdog on active SSE responses so a
//     mid-stream stall (headers received, then bytes stop flowing)
//     is aborted instead of hanging forever. Anthropic's own
//     server-side pings are sent as SSE comments on active streams,
//     so ANY response-body silence for more than a few tens of
//     seconds is anomalous. When the watchdog fires we close the
//     upstream connection, claude sees EOF, undici drops the pool
//     entry, and the next request opens fresh.
//
// Design: claude speaks plain HTTP to 127.0.0.1:$port (set via
// ANTHROPIC_BASE_URL by the clod wrapper). We own the outbound TCP
// pool to Anthropic (IdleConnTimeout=15s reaps pool connections
// during between-turn think windows; the body-idle watchdog reaps
// mid-stream stalls). Loopback between claude and us can't wedge
// (no middlebox on 127.0.0.1), so claude's own agent state stays
// healthy regardless of what Anthropic's edge is doing.
//
// SSE streaming: FlushInterval=-1 on httputil.ReverseProxy flushes
// after every Write for token-by-token pass-through.
//
// Observability: every request logs START (method/path/req_id), an
// UPSTREAM_HEADERS line when the response headers land (status +
// upstream latency), a BODY_PROGRESS heartbeat every 30s while
// bytes are still flowing, an END line when the stream closes
// naturally, and a BODY_IDLE_ABORT if the watchdog fires. That
// trail is enough to distinguish "Anthropic is slow" from "the
// pipe wedged completely" from a single log skim.
package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sync/atomic"
	"time"
)

func envDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[clodproxy] "+format+"\n", args...)
}

// tracedTransport wraps a base http.RoundTripper with per-request
// logging + body-idle enforcement. Each RoundTrip is assigned a
// monotonic id used in every log line so a request's whole life can
// be grepped by "req#N".
type tracedTransport struct {
	inner       http.RoundTripper
	seq         atomic.Uint64
	idleTimeout time.Duration
}

func (t *tracedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	id := t.seq.Add(1)
	started := time.Now()
	logf("req#%d START %s %s ua=%q", id, req.Method, req.URL.RequestURI(), req.Header.Get("User-Agent"))
	resp, err := t.inner.RoundTrip(req)
	if err != nil {
		logf("req#%d UPSTREAM_ERROR dur=%s err=%v", id, time.Since(started), err)
		return nil, err
	}
	logf("req#%d UPSTREAM_HEADERS status=%d dur=%s content_type=%q",
		id, resp.StatusCode, time.Since(started), resp.Header.Get("Content-Type"))
	resp.Body = newIdleBody(resp.Body, id, started, t.idleTimeout)
	return resp, nil
}

// idleBody wraps an upstream response body with an inactivity
// watchdog. Every Read bumps lastRead; a goroutine ticking at
// timeout/4 closes the underlying body once no Read has happened
// within `timeout`. Closing the body severs the TCP connection,
// so claude sees EOF and undici reaps the pool entry.
//
// The watchdog is bounded: it exits on Close or on firing exactly
// once. reqID + started are captured only for the log lines.
type idleBody struct {
	inner    io.ReadCloser
	reqID    uint64
	started  time.Time
	timeout  time.Duration
	lastRead atomic.Int64 // unix nanos of most recent Read
	bytes    atomic.Int64
	closed   atomic.Bool
	stopCh   chan struct{}
}

func newIdleBody(inner io.ReadCloser, reqID uint64, started time.Time, timeout time.Duration) *idleBody {
	b := &idleBody{
		inner:   inner,
		reqID:   reqID,
		started: started,
		timeout: timeout,
		stopCh:  make(chan struct{}),
	}
	b.lastRead.Store(time.Now().UnixNano())
	go b.watchdog()
	return b
}

func (b *idleBody) watchdog() {
	// Tick at timeout/4 so a stall is detected within ~timeout+25%
	// wall clock. Also emit a BODY_PROGRESS heartbeat every 30s
	// while bytes flow so an operator can see a long streaming
	// response is alive, not stuck.
	tick := b.timeout / 4
	if tick < 5*time.Second {
		tick = 5 * time.Second
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()

	var lastHeartbeatBytes int64
	for {
		select {
		case <-b.stopCh:
			return
		case now := <-ticker.C:
			if b.closed.Load() {
				return
			}
			last := time.Unix(0, b.lastRead.Load())
			since := now.Sub(last)
			if since > b.timeout {
				logf("req#%d BODY_IDLE_ABORT idle=%s bytes=%d total_dur=%s — closing upstream",
					b.reqID, since.Round(time.Millisecond), b.bytes.Load(), time.Since(b.started).Round(time.Millisecond))
				_ = b.inner.Close()
				b.closed.Store(true)
				return
			}
		case <-heartbeat.C:
			if b.closed.Load() {
				return
			}
			bytesNow := b.bytes.Load()
			if bytesNow != lastHeartbeatBytes {
				logf("req#%d BODY_PROGRESS bytes=%d total_dur=%s",
					b.reqID, bytesNow, time.Since(b.started).Round(time.Millisecond))
				lastHeartbeatBytes = bytesNow
			}
		}
	}
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.inner.Read(p)
	if n > 0 {
		b.lastRead.Store(time.Now().UnixNano())
		b.bytes.Add(int64(n))
	}
	if err != nil {
		return n, err
	}
	return n, nil
}

func (b *idleBody) Close() error {
	if b.closed.CompareAndSwap(false, true) {
		close(b.stopCh)
		logf("req#%d END bytes=%d total_dur=%s",
			b.reqID, b.bytes.Load(), time.Since(b.started).Round(time.Millisecond))
	}
	return b.inner.Close()
}

func main() {
	listenAddr := flag.String("listen", envDefault("CLOD_PROXY_LISTEN", "127.0.0.1:8788"), "listen address")
	upstream := flag.String("upstream", envDefault("CLOD_PROXY_UPSTREAM", "https://api.anthropic.com"), "upstream base URL")
	idleConnTimeout := flag.Duration("idle-conn-timeout", 15*time.Second, "reap keep-alive pool entries idle longer than this")
	bodyIdleTimeout := flag.Duration("body-idle-timeout", 90*time.Second, "abort a streaming response body if no bytes read for this long")
	flag.Parse()

	u, err := url.Parse(*upstream)
	if err != nil {
		logf("parse upstream %q: %v", *upstream, err)
		os.Exit(1)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		logf("upstream scheme must be http or https, got %q", u.Scheme)
		os.Exit(1)
	}

	// See the file-level comment for the rationale behind these
	// values. Short summary: aggressive IdleConnTimeout stops
	// between-turn pool wedges; body-idle watchdog stops mid-stream
	// stalls; HTTP/1.1 forced so pool reaping actually reaps.
	baseTransport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 10 * time.Second,
		}).DialContext,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       *idleConnTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     false,
		DisableCompression:    false,
	}
	traced := &tracedTransport{
		inner:       baseTransport,
		idleTimeout: *bodyIdleTimeout,
	}

	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.Transport = traced
	// Per-write flush is essential for SSE — without it
	// httputil.ReverseProxy buffers response bytes and claude
	// sees no tokens until the whole message closes.
	proxy.FlushInterval = -1

	origDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		origDirector(req)
		req.Host = u.Host
		req.Header.Del("Proxy-Connection")
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		logf("upstream error path=%s: %v", r.URL.Path, err)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "clodproxy upstream error: "+err.Error()+"\n")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.Handle("/", proxy)

	srv := &http.Server{
		Addr:              *listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
	}

	logf("listening on %s → %s (conn_idle=%v body_idle=%v)",
		*listenAddr, u.String(), *idleConnTimeout, *bodyIdleTimeout)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logf("serve: %v", err)
		os.Exit(1)
	}
}

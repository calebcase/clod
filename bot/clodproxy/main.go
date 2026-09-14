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

	tunnelIdleTimeout := *bodyIdleTimeout // reuse the same knob for CONNECT tunnels
	var tunnelSeq atomic.Uint64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// HTTPS_PROXY intercepts every outbound HTTPS from claude
		// and issues CONNECT to us before doing the TLS handshake.
		// We can't inspect the tunneled bytes (they're TLS), but
		// we CAN detect "no bytes flowing in either direction for
		// N seconds" at the TCP splice level and close the tunnel.
		// That is enough to break the between-turn CLOSE_WAIT pool
		// wedge for endpoints outside api.anthropic.com (statsig,
		// sentry, etc.) — same failure mode we observed on eagle
		// at 13:19Z with a wedged direct connection to a Datadog
		// / Statsig-adjacent IP.
		if r.Method == http.MethodConnect {
			handleConnect(w, r, tunnelIdleTimeout, tunnelSeq.Add(1))
			return
		}
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ok\n")
			return
		}
		proxy.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:              *listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
	}

	logf("listening on %s → %s (conn_idle=%v body_idle=%v tunnel_idle=%v)",
		*listenAddr, u.String(), *idleConnTimeout, *bodyIdleTimeout, tunnelIdleTimeout)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logf("serve: %v", err)
		os.Exit(1)
	}
}

// handleConnect implements HTTP CONNECT tunnel mode. Claude sends
// `CONNECT api.anthropic.com:443 HTTP/1.1` when HTTPS_PROXY points
// at us; we dial the upstream, hijack the client conn, splice bytes
// bidirectionally, and enforce an idle-timeout at the TCP level.
// Body-level tracing isn't possible (TLS-encrypted) — we only see
// byte counts and idle time.
//
// The idle-timeout closure is what makes this useful. Between
// turns the tunnel sits idle; if the upstream (or a middlebox)
// silently half-closes, claude's undici agent won't notice until
// it tries to write on the tunnel again — that's the eagle wedge
// class. Our timeout closes both ends so claude sees EOF and
// drops the pool entry.
func handleConnect(w http.ResponseWriter, r *http.Request, idleTimeout time.Duration, id uint64) {
	target := r.Host // for CONNECT, r.Host holds "host:port"
	started := time.Now()
	logf("connect#%d TARGET %s ua=%q", id, target, r.Header.Get("User-Agent"))

	upstream, err := net.DialTimeout("tcp", target, 30*time.Second)
	if err != nil {
		logf("connect#%d DIAL_FAIL dur=%s err=%v", id, time.Since(started).Round(time.Millisecond), err)
		http.Error(w, "clodproxy CONNECT dial failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer upstream.Close()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		logf("connect#%d HIJACK_UNSUPPORTED", id)
		http.Error(w, "clodproxy CONNECT: hijack unsupported", http.StatusInternalServerError)
		return
	}
	client, buf, err := hijacker.Hijack()
	if err != nil {
		upstream.Close()
		logf("connect#%d HIJACK_ERR: %v", id, err)
		return
	}
	defer client.Close()

	// Ack the CONNECT. The write must happen BEFORE any tunneled
	// bytes flow — claude waits for this line before starting its
	// TLS handshake over the tunnel.
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		logf("connect#%d ACK_WRITE_ERR: %v", id, err)
		return
	}

	// If the hijacked reader has any bytes already buffered from the
	// CONNECT request line (unusual but possible), forward them to
	// upstream so the TLS handshake sees a clean stream.
	if buf != nil && buf.Reader != nil {
		if n := buf.Reader.Buffered(); n > 0 {
			pre := make([]byte, n)
			if _, err := io.ReadFull(buf.Reader, pre); err == nil {
				_, _ = upstream.Write(pre)
			}
		}
	}

	logf("connect#%d ESTABLISHED dur=%s", id, time.Since(started).Round(time.Millisecond))
	tunnelBytes(client, upstream, idleTimeout, id, target, started)
}

// tunnelBytes runs two goroutines splicing bytes between the client
// and upstream, plus an idle watchdog that closes both sides when
// no data has moved in either direction for `idleTimeout`. Returns
// when both directions have terminated. Total byte counts and
// close reason logged on exit.
func tunnelBytes(client, upstream net.Conn, idleTimeout time.Duration, id uint64, target string, started time.Time) {
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	var toUpstream, toClient atomic.Int64
	var idleAborted atomic.Bool

	stopWatchdog := make(chan struct{})
	go func() {
		tick := idleTimeout / 4
		if tick < 5*time.Second {
			tick = 5 * time.Second
		}
		ticker := time.NewTicker(tick)
		defer ticker.Stop()
		for {
			select {
			case <-stopWatchdog:
				return
			case now := <-ticker.C:
				last := time.Unix(0, lastActivity.Load())
				since := now.Sub(last)
				if since > idleTimeout {
					idleAborted.Store(true)
					logf("connect#%d IDLE_ABORT target=%s idle=%s to_upstream=%d to_client=%d",
						id, target, since.Round(time.Millisecond),
						toUpstream.Load(), toClient.Load())
					_ = client.Close()
					_ = upstream.Close()
					return
				}
			}
		}
	}()

	done := make(chan struct{}, 2)
	copyOne := func(dst, src net.Conn, counter *atomic.Int64) {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 32*1024)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				lastActivity.Store(time.Now().UnixNano())
				counter.Add(int64(n))
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	go copyOne(upstream, client, &toUpstream)
	go copyOne(client, upstream, &toClient)

	// Wait for one direction to end; force the other side to unblock.
	<-done
	close(stopWatchdog)
	_ = client.Close()
	_ = upstream.Close()
	<-done

	reason := "peer_close"
	if idleAborted.Load() {
		reason = "idle_abort"
	}
	logf("connect#%d END target=%s reason=%s dur=%s to_upstream=%d to_client=%d",
		id, target, reason, time.Since(started).Round(time.Millisecond),
		toUpstream.Load(), toClient.Load())
}

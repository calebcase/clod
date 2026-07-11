// Command clodproxy is the in-container HTTP → HTTPS reverse proxy
// that stands between claude and api.anthropic.com. Its sole job is
// to force turnover of the HTTP keep-alive connection pool so
// claude doesn't wedge on a silently half-closed socket.
//
// Failure mode we're working around (upstream claude-code #54434):
// claude finishes a turn, sits idle waiting for user input. During
// that idle window some middlebox (or Anthropic's LB) closes the
// keep-alive TCP connection but Node's undici agent doesn't
// observe the FIN. When the user's next input arrives claude
// grabs the "healthy" pool connection, writes the POST body
// successfully (writes succeed on a half-closed socket), and then
// blocks forever on the SSE read. The between-turn window is the
// exact case Anthropic's own 5-minute byte watchdog does NOT
// cover — the wedge fires before any bytes flow.
//
// Design: claude speaks plain HTTP to 127.0.0.1:$port (set via
// ANTHROPIC_BASE_URL by the clod wrapper). We own the outbound
// TCP pool to Anthropic and set an IdleConnTimeout short enough
// that a between-turn wedge cannot happen — any connection idle
// for more than the timeout is reaped and a new one dialed on the
// next request. Loopback between claude and us can't wedge (no
// middlebox on a Unix loopback), so claude's own agent state
// stays healthy.
//
// SSE streaming: FlushInterval=-1 tells httputil.ReverseProxy to
// flush after every Write, so token-by-token streaming pass-
// through has no additional latency vs. talking to Anthropic
// directly. ResponseHeaderTimeout is generous (60s) because
// claude's own request timeout logic is upstream of us.
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

func main() {
	listenAddr := flag.String("listen", envDefault("CLOD_PROXY_LISTEN", "127.0.0.1:8788"), "listen address")
	upstream := flag.String("upstream", envDefault("CLOD_PROXY_UPSTREAM", "https://api.anthropic.com"), "upstream base URL")
	idleTimeout := flag.Duration("idle-timeout", 15*time.Second, "idle conn reap interval to upstream")
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

	// The whole point of this binary lives here: aggressive
	// IdleConnTimeout ensures no pooled connection outlives a
	// between-turn user think window. Values chosen by hand:
	//   - IdleConnTimeout=15s: shorter than any realistic
	//     between-turn silence, longer than a single tool-use
	//     round trip so we don't churn connections during a turn.
	//   - MaxIdleConnsPerHost=4: enough parallelism for
	//     claude's occasional concurrent requests, small enough
	//     that a wedge can't hide across many pool slots.
	//   - DialContext KeepAlive=10s: OS-level TCP keepalive
	//     probes so any half-closed socket is detected within
	//     ~30s even if it stays in the pool.
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 10 * time.Second,
		}).DialContext,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       *idleTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// Force HTTP/1.1: HTTP/2 multiplexing means a single TCP
		// connection carries every request, so pool reaping is
		// useless. HTTP/1.1 with a per-connection pool is what we
		// need to force turnover.
		ForceAttemptHTTP2: false,
		DisableCompression: false,
	}

	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.Transport = transport
	// Per-write flush is essential for SSE — without it
	// httputil.ReverseProxy buffers response bytes and claude
	// sees no tokens until the whole message closes.
	proxy.FlushInterval = -1

	origDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		origDirector(req)
		req.Host = u.Host
		// Strip any inbound Proxy-* headers to avoid confusing
		// Anthropic's edge. Not strictly required for direct HTTP
		// but harmless.
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
		Addr:    *listenAddr,
		Handler: mux,
		// No timeouts on the client-side (claude) — long SSE
		// streams need to be able to sit idle-of-tokens for
		// minutes during model thinking. Idle handling is
		// entirely on the upstream (Anthropic) side.
		ReadHeaderTimeout: 30 * time.Second,
	}

	logf("listening on %s → %s (idle=%v)", *listenAddr, u.String(), *idleTimeout)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logf("serve: %v", err)
		os.Exit(1)
	}
}

// Package machine proxies requests to the Meticulous machine HTTP API.
//
// Rather than re-modelling every endpoint as typed Go, caffeine forwards
// /api/machine/* to the machine and lets the frontend speak the upstream
// JSON directly. This keeps the backend thin for read-only operations
// and gives us a single place to add auth, caching, and rate-limiting
// later.
package machine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Proxy forwards requests to the configured machine base URL.
type Proxy struct {
	base    *url.URL
	rp      *httputil.ReverseProxy
	client  *http.Client
	machine string

	// Tracks the last time the machine answered any HTTP probe. Used to
	// keep the StatusPill from flapping when the machine's wifi chip
	// burns a probe attempt on TX retries.
	mu     sync.Mutex
	lastOK time.Time
}

// stickyOnlineWindow is how long a previously-reachable machine is
// reported as "degraded" (still online, but a probe just failed) before
// we admit it's actually unreachable. Picked to comfortably cover a
// burst of wifi TX retries without hiding a real outage.
const stickyOnlineWindow = 90 * time.Second

// New builds a Proxy. baseURL must include scheme (e.g. http://meticulous.local).
func New(baseURL string) (*Proxy, error) {
	if baseURL == "" {
		return nil, errors.New("machine base URL is empty")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse machine url: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("machine url must include scheme and host: %q", baseURL)
	}

	// Generous dial budget — the machine's wifi chip is known to do
	// long bursts of TX retries on the first packet of a connection.
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ResponseHeaderTimeout: 20 * time.Second,
		IdleConnTimeout:       60 * time.Second,
		MaxIdleConns:          20,
	}

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			pr.Out.Host = u.Host
			// Strip our /api/machine prefix and prepend the machine's /api
			// so /api/machine/v1/settings → ${machine}/api/v1/settings.
			rest := strings.TrimPrefix(pr.In.URL.Path, "/api/machine")
			if rest == "" {
				rest = "/"
			}
			pr.Out.URL.Path = "/api" + rest
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
		},
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = fmt.Fprintf(w, `{"error":"machine unreachable","detail":%q}`, err.Error())
		},
	}

	return &Proxy{
		base:    u,
		rp:      rp,
		client:  &http.Client{Transport: transport, Timeout: 25 * time.Second},
		machine: u.String(),
	}, nil
}

// Handler returns the reverse proxy handler (mount at /api/machine).
func (p *Proxy) Handler() http.Handler { return p.rp }

// Status probes the machine and reports reachability.
type StatusResult struct {
	Reachable  bool   `json:"reachable"`
	MachineURL string `json:"machine_url"`
	Error      string `json:"error,omitempty"`
	StatusCode int    `json:"status_code,omitempty"`

	// Degraded is true when the *current* probe failed but the machine
	// answered within stickyOnlineWindow. The pill stays green-ish but
	// the UI can show a "flaky" hint. Degraded implies Reachable=true.
	Degraded bool `json:"degraded,omitempty"`

	// LastSeenUnix is the wall-clock seconds at which the machine last
	// answered any probe. Zero if it never has.
	LastSeenUnix int64 `json:"last_seen_unix,omitempty"`

	// Attempts is how many probe attempts the call made before returning.
	// Useful for surfacing "machine took 2 retries" in the UI/logs.
	Attempts int `json:"attempts,omitempty"`
}

// Status probes the machine's root URL and reports reachability, with
// retry + sticky-online behavior tuned for the machine's flaky wifi.
//
//   - We probe `/` (the SPA root, always 200, tiny payload). Some
//     firmwares 404 on `/api/v1/settings`.
//   - Up to 3 attempts per call, ~3s each, with 250ms backoff. A burst
//     of TX retries on the wifi chip blows one attempt without flipping
//     the pill.
//   - Any HTTP response — including 5xx — counts as reachable.
//   - If the current probe fails but we've heard from the machine in
//     the last ~90 seconds, we report Reachable=true + Degraded=true so
//     the UI doesn't oscillate between online/offline on transient drops.
func (p *Proxy) Status(ctx context.Context) StatusResult {
	result := StatusResult{MachineURL: p.machine}

	const attempts = 3
	const perAttempt = 3 * time.Second
	const backoff = 250 * time.Millisecond

	var lastErr error
	var lastCode int
	for i := 0; i < attempts; i++ {
		result.Attempts = i + 1
		if err := ctx.Err(); err != nil {
			lastErr = err
			break
		}

		actx, cancel := context.WithTimeout(ctx, perAttempt)
		req, err := http.NewRequestWithContext(actx, http.MethodGet, p.machine+"/", nil)
		if err != nil {
			cancel()
			lastErr = err
			break
		}
		resp, err := p.client.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			if i < attempts-1 {
				time.Sleep(backoff)
			}
			continue
		}
		lastCode = resp.StatusCode
		resp.Body.Close()
		cancel()

		now := time.Now()
		p.mu.Lock()
		p.lastOK = now
		p.mu.Unlock()

		result.Reachable = true
		result.StatusCode = lastCode
		result.LastSeenUnix = now.Unix()
		return result
	}

	// All attempts failed. Decide between hard-unreachable and degraded.
	p.mu.Lock()
	lastOK := p.lastOK
	p.mu.Unlock()

	if lastErr != nil {
		result.Error = lastErr.Error()
	}
	result.StatusCode = lastCode
	if !lastOK.IsZero() {
		result.LastSeenUnix = lastOK.Unix()
		if time.Since(lastOK) <= stickyOnlineWindow {
			result.Reachable = true
			result.Degraded = true
		}
	}
	return result
}

// Package machine proxies requests to the Meticulous machine.
package machine

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProxyRewritesPath(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	p, err := New(upstream.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/machine/v1/settings?x=1", nil)
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/api/v1/settings" {
		t.Fatalf("upstream got path %q, want /api/v1/settings", gotPath)
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("body %q", rec.Body.String())
	}
}

func TestProxyStatusUnreachable(t *testing.T) {
	// Port 1 is almost never listening.
	p, err := New("http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res := p.Status(context.Background())
	if res.Reachable {
		t.Fatalf("expected unreachable")
	}
}

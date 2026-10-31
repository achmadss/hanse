package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// probe performs one request against the health routes without any
// Authorization header: the two routes must need no authentication.
func probe(t *testing.T, handler http.Handler, method, path string) (*http.Response, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec.Result(), rec.Body.String()
}

// TestHealthzReportsTheProcess keeps /healthz independent of every
// dependency: it answers 200 while the process runs, without a token.
func TestHealthzReportsTheProcess(t *testing.T) {
	handler := newServer(nil, nil, nil, 0)
	response, body := probe(t, handler, http.MethodGet, "/healthz")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if body != healthBody {
		t.Errorf("body = %q, want %q", body, healthBody)
	}
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", got)
	}
	if got := response.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

// TestReadyzWithoutACheckIs503 requires the endpoint to fail closed when no
// readiness check is wired.
func TestReadyzWithoutACheckIs503(t *testing.T) {
	handler := newServer(nil, nil, nil, 0)
	response, body := probe(t, handler, http.MethodGet, "/readyz")
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusServiceUnavailable)
	}
	if body != notReadyBody {
		t.Errorf("body = %q, want %q", body, notReadyBody)
	}
	assertNoInternalDetail(t, body)
}

// TestReadyzFollowsTheCheck requires the endpoint to report the wired
// readiness value, and to answer 503 again once shutdown starts.
func TestReadyzFollowsTheCheck(t *testing.T) {
	var ready atomic.Bool
	handler := newServer(nil, nil, nil, 0, WithReadiness(ready.Load))

	response, body := probe(t, handler, http.MethodGet, "/readyz")
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("before ready: status = %d, want %d", response.StatusCode, http.StatusServiceUnavailable)
	}
	assertNoInternalDetail(t, body)

	ready.Store(true)
	response, body = probe(t, handler, http.MethodGet, "/readyz")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("ready: status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if body != readyBody {
		t.Errorf("body = %q, want %q", body, readyBody)
	}

	ready.Store(false)
	response, _ = probe(t, handler, http.MethodGet, "/readyz")
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("not ready again: status = %d, want %d", response.StatusCode, http.StatusServiceUnavailable)
	}
}

// TestProbesAcceptOnlyGET keeps a probe from changing state and keeps the
// routes out of the POST surface.
func TestProbesAcceptOnlyGET(t *testing.T) {
	handler := newServer(nil, nil, nil, 0)
	for _, path := range []string{"/healthz", "/readyz"} {
		response, _ := probe(t, handler, http.MethodPost, path)
		if response.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: status = %d, want %d", path, response.StatusCode, http.StatusMethodNotAllowed)
		}
	}
}

// assertNoInternalDetail checks that an unauthenticated 503 body leaks
// nothing about the Gateway, such as a store name, a path, or a version.
func assertNoInternalDetail(t *testing.T, body string) {
	t.Helper()
	for _, word := range []string{"sqlite", "store", "registry", "state", "/", "hanse"} {
		if strings.Contains(strings.ToLower(body), word) {
			t.Errorf("body %q mentions %q; a probe answer must carry no internal detail", body, word)
		}
	}
}

package api

import (
	"io"
	"net/http"
)

// Health OK and not-ready bodies. They are fixed text: a probe needs an
// answer, not a report, and the body must never name a store, a path, a
// version, or any other internal detail (docs/guides/health.md).
const (
	healthBody   = "ok\n"
	notReadyBody = "not ready\n"
	readyBody    = "ready\n"
)

// WithReadiness sets the readiness check of GET /readyz. A nil check, or a
// check that reports false, makes the endpoint answer 503. The Gateway
// passes a check that turns true once the stores are open and the
// registries are loaded, and false again when shutdown starts.
func WithReadiness(ready func() bool) Option {
	return func(s *server) { s.ready = ready }
}

// healthz answers GET /healthz: the process is alive. It never reports a
// dependency, so a failing database cannot make an orchestrator restart a
// healthy process. The route needs no authentication.
func (s *server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeProbe(w, http.StatusOK, healthBody)
}

// readyz answers GET /readyz: the Gateway can serve API traffic. The 503
// answer carries no internal detail, so an unauthenticated probe learns
// only that the Gateway is not ready. The route needs no authentication.
func (s *server) readyz(w http.ResponseWriter, _ *http.Request) {
	if s.ready == nil || !s.ready() {
		writeProbe(w, http.StatusServiceUnavailable, notReadyBody)
		return
	}
	writeProbe(w, http.StatusOK, readyBody)
}

// writeProbe writes a plain-text probe answer. Probes must not be cached:
// the next request is the whole point.
func writeProbe(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

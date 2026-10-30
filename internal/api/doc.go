// Package api serves the read-only discovery endpoints of the versioned
// HTTP API under /v1 (docs/architecture.md section 11). Execution
// endpoints and the server wiring come later; New returns a handler that a
// server mounts behind authentication.
package api

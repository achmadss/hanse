// Package api serves the versioned HTTP API under /v1: the read-only
// discovery endpoints (docs/architecture.md section 11) and the execution
// endpoints for submitting, validating, listing, reading, and cancelling
// operations (issue #16). The server mounts the returned handler under /v1
// and wraps it with auth.Store.Middleware.
package api

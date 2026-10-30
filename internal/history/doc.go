// Package history stores operation records in a local SQLite database so
// that they survive a Gateway restart (docs/architecture.md section 9.6).
// It also recovers operations a previous process left active and removes
// records older than the configured retention. Per-operation logs are
// issue #19; this package will hold them too.
package history

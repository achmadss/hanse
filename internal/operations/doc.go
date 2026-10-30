// Package operations manages the lifecycle of operation runs. It accepts
// requests, validates them, resolves the executor, enforces idempotency
// and concurrency, records the operation, dispatches it to a runtime, and
// handles cancellation, following docs/architecture.md sections 4 and 9.
//
// The store is an interface. The Gateway currently keeps records in
// memory (MemoryStore); issue #17 supplies the SQLite history store behind
// the same interface, and issue #19 adds the operation logs. No secret
// value reaches a record: the elevation password lives only for the
// dispatch, and the argument values are reduced to a fingerprint.
package operations

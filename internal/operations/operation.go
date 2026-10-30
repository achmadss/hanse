package operations

import (
	"encoding/json"
	"time"
)

// State is one operation lifecycle state, docs/architecture.md section 9.1.
type State string

const (
	// StateQueued means the operation is accepted and stored, not yet started.
	StateQueued State = "queued"
	// StateRunning means the handler runs on the executor.
	StateRunning State = "running"
	// StateCompleted means the handler finished and reported success.
	StateCompleted State = "completed"
	// StateFailed means the handler reported an error, timed out, could not
	// start, or the Gateway stopped while it ran.
	StateFailed State = "failed"
	// StateCancelled means a client cancelled the operation before it finished.
	StateCancelled State = "cancelled"
)

// Valid reports whether s is one of the documented states.
func (s State) Valid() bool {
	switch s {
	case StateQueued, StateRunning, StateCompleted, StateFailed, StateCancelled:
		return true
	}
	return false
}

// Final reports whether s is a state the operation never leaves.
func (s State) Final() bool {
	return s == StateCompleted || s == StateFailed || s == StateCancelled
}

// active reports whether s is a state that still counts against the
// concurrency limit. A queued run is about to run, so it counts too.
func (s State) active() bool {
	return s == StateQueued || s == StateRunning
}

// Failure is why an operation ended failed or cancelled. Code is a wire
// code: one of the shared Gateway codes or the capability's own code from
// the handler response (docs/handler-contract.md sections 3 and 5).
type Failure struct {
	Code    string
	Message string
	Details map[string]any
}

// clone copies a failure so callers cannot mutate a stored record.
func (f *Failure) clone() *Failure {
	if f == nil {
		return nil
	}
	out := &Failure{Code: f.Code, Message: f.Message}
	if f.Details != nil {
		out.Details = make(map[string]any, len(f.Details))
		for k, v := range f.Details {
			out.Details[k] = v
		}
	}
	return out
}

// Record is one stored operation. It holds no secret value: the elevation
// password never reaches it, and the arguments have their secret values
// replaced by [redacted] (docs/architecture.md sections 9.6 and 15).
type Record struct {
	ID         string
	Capability string
	Operation  string
	Requester  string
	Executor   string
	// ExecutorSource is "explicit" or "default", docs/architecture.md
	// section 5.
	ExecutorSource string
	// Mode is "sync" or "async", from the operation's manifest.
	Mode   string
	Status State
	// CreatedAt is when the request was accepted; StartedAt is when the
	// run began and FinishedAt when it reached a final state. Zero means
	// it has not happened.
	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time
	// Result and Summary come from an ok or cancelled handler response.
	Result  json.RawMessage
	Summary string
	// Failure is set when Status is StateFailed or StateCancelled.
	Failure *Failure
	// IdempotencyKey is the client's Idempotency-Key header, scoped to
	// Requester (docs/architecture.md section 9.3).
	IdempotencyKey string
	// ArgumentFingerprint is a hash of the canonical arguments, so a
	// retry with the same key can be checked for the same request
	// without storing argument values.
	ArgumentFingerprint string
	// Arguments are the request arguments with every secret value
	// replaced by [redacted]. The elevation password never appears here
	// (docs/architecture.md sections 8.5 and 9.6).
	Arguments json.RawMessage
	// ConcurrencyKey is the combination of `concurrency.key` argument
	// values of this run, or "" when the operation sets no key.
	ConcurrencyKey string
}

// clone copies a record, including the byte slice and the failure, so the
// store never shares mutable state with a caller.
func (r Record) clone() Record {
	out := r
	if r.Result != nil {
		out.Result = append(json.RawMessage(nil), r.Result...)
	}
	if r.Arguments != nil {
		out.Arguments = append(json.RawMessage(nil), r.Arguments...)
	}
	out.Failure = r.Failure.clone()
	return out
}

// IsFinal reports whether the record reached a final state.
func (r Record) IsFinal() bool { return r.Status.Final() }

// Elevation is the optional elevation input of a privileged operation,
// docs/architecture.md section 8.5. The Gateway passes it to dispatch and
// never stores or returns it. Code outside this package must not log it;
// String and GoString redact the password so an accidental print is safe.
type Elevation struct {
	Password string
}

// String implements fmt.Stringer without the password.
func (e *Elevation) String() string { return "elevation{password:[redacted]}" }

// GoString implements fmt.GoStringer without the password.
func (e *Elevation) GoString() string { return "elevation{password:[redacted]}" }

// Request is one client operation request. Legacy field names follow the
// API body (docs/architecture.md section 11).
type Request struct {
	Capability string
	Operation  string
	// Arguments are the caller's argument values, validated against the
	// operation's schema.
	Arguments json.RawMessage
	// Executor is the node the request named, or "" when it named none.
	Executor string
	// Elevation is the optional elevation input. It is passed to dispatch
	// and never stored.
	Elevation *Elevation
	// IdempotencyKey is the Idempotency-Key header, or "" when absent.
	IdempotencyKey string
	// Requester is the authenticated token name, which scopes the
	// idempotency key.
	Requester string
}

// Check is the result of validating a request without running it: the
// executor that would run it, how it was chosen, and the operation's mode.
type Check struct {
	Executor       string
	ExecutorSource string
	Mode           string
}

// Filter narrows a list of operations. An empty field matches everything.
type Filter struct {
	Capability string
	Operation  string
	Executor   string
	Status     State
	Requester  string
}

// matches reports whether rec satisfies every filter field.
func (f Filter) matches(rec Record) bool {
	return (f.Capability == "" || rec.Capability == f.Capability) &&
		(f.Operation == "" || rec.Operation == f.Operation) &&
		(f.Executor == "" || rec.Executor == f.Executor) &&
		(f.Status == "" || rec.Status == f.Status) &&
		(f.Requester == "" || rec.Requester == f.Requester)
}

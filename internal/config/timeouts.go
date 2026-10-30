// Package config will load and validate the Gateway, node, and CLI
// configuration files described in docs/configuration.md. This file holds
// only the timeout defaults and the per-operation resolution; the full
// loader is issue #8.
package config

import "time"

// Timeouts holds the Gateway's default timeouts from gateway.yaml
// (docs/configuration.md, timeouts.*). Zero means "not configured"; the
// defaults below are what the Gateway uses in that case.
type Timeouts struct {
	// Operation is the timeout of an operation whose manifest sets none.
	Operation time.Duration
	// Options is the timeout of an option provider call.
	Options time.Duration
	// GracePeriod is the time between a stop request and the kill, when the
	// manifest sets none.
	GracePeriod time.Duration
}

// DefaultTimeouts are the documented defaults: timeouts.operation 5m,
// timeouts.options 10s, timeouts.grace_period 10s.
func DefaultTimeouts() Timeouts {
	return Timeouts{
		Operation:   5 * time.Minute,
		Options:     10 * time.Second,
		GracePeriod: 10 * time.Second,
	}
}

// OperationTimeouts is the per-operation override parsed from the
// capability manifest (manifest fields timeout and grace_period). A nil
// field keeps the configured default; zero and negative values are ignored
// the same way, because a non-positive timeout would make every run fail.
type OperationTimeouts struct {
	Timeout     *time.Duration
	GracePeriod *time.Duration
}

// ResolveTimeouts applies a per-operation override over the configured
// defaults and returns the effective timeouts for the operation.
func ResolveTimeouts(defaults Timeouts, override *OperationTimeouts) Timeouts {
	effective := defaults
	if override == nil {
		return effective
	}
	if v := positive(override.Timeout); v > 0 {
		effective.Operation = v
	}
	if v := positive(override.GracePeriod); v > 0 {
		effective.GracePeriod = v
	}
	return effective
}

func positive(d *time.Duration) time.Duration {
	if d == nil || *d <= 0 {
		return 0
	}
	return *d
}

// HandlerDeadline computes the deadline sent to a handler: the run's start
// time plus the operation's timeout, in UTC. The handler contract requires
// RFC 3339 in UTC; time.Time encodes as RFC 3339 when a JSON formatter or
// Format is applied.
func HandlerDeadline(start time.Time, timeout time.Duration) time.Time {
	return start.Add(timeout).UTC()
}

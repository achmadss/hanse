package operations

import "time"

// Log records one log line or system event that arrived for a run this
// process did not dispatch itself. The node agent connection hub calls it
// for every stderr line, heartbeat notice, and run-state event it receives
// over a node's WebSocket, so those lines reach the same operation log,
// with the same redaction terms and the same live followers, as a line of a
// run on the Gateway host (docs/architecture.md section 10).
//
// The line is routed to the sink of the Manager, which the manager opened
// with the run's redaction terms before it dispatched the run. The arrival
// timestamp is not stored: the log sink stamps each entry with its own
// clock, so a skewed node clock cannot reorder the log.
func (m *Manager) Log(runID string, _ time.Time, stream, line string) {
	if m == nil {
		return
	}
	m.logs.Append(runID, stream, line)
}

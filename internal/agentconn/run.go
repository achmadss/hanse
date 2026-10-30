package agentconn

import (
	"sync"
	"time"

	"github.com/achmadss/hanse/internal/agentproto"
	"github.com/achmadss/hanse/internal/runtime"
)

// run is one run the hub sent and has not resolved. It is shared between
// the dispatching goroutine, the connection's read loop, and the reconnect
// window timer, so every mutable field is guarded by mu.
type run struct {
	id         string
	node       string
	option     bool
	privileged bool
	// message is the run message to send again when the agent's hello
	// does not list the run (docs/agent-protocol.md section 8.2).
	message *agentproto.Run
	created time.Time

	mu sync.Mutex
	// acked is the highest sequence number the Gateway stored.
	acked int64
	// response and err are the dispatch result once done is closed.
	response runtime.Response
	err      error
	// finished reports that the run has an outcome.
	finished bool
	// abandoned reports that the run ended while the agent was away, or
	// must not run again. A reconnect answers abandon for it.
	abandoned bool
	// done closes once the run has an outcome.
	done chan struct{}
}

// finish stores the outcome and closes done exactly once.
func (r *run) finish(resp runtime.Response, err error) {
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return
	}
	r.finished = true
	r.response = resp
	r.err = err
	close(r.done)
	r.mu.Unlock()
}

// result returns the run's stored outcome.
func (r *run) result() (runtime.Response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.response, r.err
}

// markAbandoned records that the run must not resume after a reconnect.
func (r *run) markAbandoned() {
	r.mu.Lock()
	r.abandoned = true
	r.mu.Unlock()
}

// isAbandoned reports whether the run must not resume.
func (r *run) isAbandoned() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.abandoned
}

// createdAt returns when the hub accepted the run.
func (r *run) createdAt() time.Time { return r.created }

// isFinished reports whether the run has an outcome.
func (r *run) isFinished() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.finished
}

// ackedSeq returns the highest sequence number the Gateway stored.
func (r *run) ackedSeq() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.acked
}

// acceptSeq records one sequence number of a log or result message. It
// returns the highest stored sequence after the update, and whether the
// message is new: a message at or below acked was stored already, so the
// Gateway ignores it (docs/agent-protocol.md section 8.2).
func (r *run) acceptSeq(seq int64) (int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if seq <= r.acked {
		return r.acked, false
	}
	r.acked = seq
	return r.acked, true
}

// abandon marks the run ended without a result and keeps it so a
// reconnect still learns the abandon action.
func (r *run) abandon(resp runtime.Response, err error) {
	r.markAbandoned()
	r.finish(resp, err)
}

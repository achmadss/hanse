package nodeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/achmadss/hanse/internal/agentproto"
	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/runner"
	"github.com/achmadss/hanse/internal/runtime"
)

// pendingMessage is one message of a run that the Gateway has not
// acknowledged. Seq is 0 for `started`, which carries no sequence number.
type pendingMessage struct {
	seq     int64
	message agentproto.Message
	// size is the log data the message adds to the disconnected buffer:
	// the line length for a log, 0 otherwise.
	size int
	// sent records that this connection already wrote the message. A new
	// connection clears it, so a resume resends everything above
	// acked_seq.
	sent bool
}

// run is one run the agent accepted and has not finished reporting.
type run struct {
	id string
	// cancel stops the handler through the runner's cancellation steps.
	cancel context.CancelFunc
	// seq is the highest sequence number the agent produced; acked is the
	// highest the Gateway stored.
	seq     int64
	acked   int64
	pending []*pendingMessage
	// logBytes is the pending log data of this run.
	logBytes int64
	// resultSeq is the sequence number of the result once it exists.
	resultSeq int64
	finished  bool
	// abandoned reports that the Gateway answered abandon: the handler
	// stops and nothing more is sent for the run.
	abandoned bool
	// forgotten reports that the run left the agent's state.
	forgotten bool
}

// acceptRun starts one run unless the agent knows it already or is at its
// concurrency limit (docs/agent-protocol.md sections 4.3, 4.5, and 6).
func (a *Agent) acceptRun(msg *agentproto.Run) {
	a.mu.Lock()
	if _, known := a.runs[msg.RunID]; known {
		a.mu.Unlock()
		return
	}
	if a.journal.has(msg.RunID) {
		// A run whose outcome a restart lost is reported as lost and is
		// never run again (docs/agent-protocol.md section 8.1).
		a.mu.Unlock()
		return
	}
	active := 0
	for _, r := range a.runs {
		if !r.finished {
			active++
		}
	}
	if active >= a.cfg.MaxConcurrentRuns {
		a.mu.Unlock()
		a.sendControl(&agentproto.Rejected{
			Type:    agentproto.TypeRejected,
			RunID:   msg.RunID,
			Code:    herrors.AgentBusy.Name(),
			Message: fmt.Sprintf("the agent already runs %d handlers", a.cfg.MaxConcurrentRuns),
		})
		return
	}
	r := &run{id: msg.RunID}
	a.runs[msg.RunID] = r
	a.mu.Unlock()
	if err := a.journal.add(msg.RunID); err != nil {
		a.logf("cannot record run %s in the journal: %v", msg.RunID, err)
	}
	go a.execute(r, msg)
}

// execute runs one accepted run: it fetches the bundle, runs the handler
// with the shared runner, and records the result.
func (a *Agent) execute(r *run, msg *agentproto.Run) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.mu.Lock()
	if r.forgotten {
		a.mu.Unlock()
		return
	}
	r.cancel = cancel
	a.mu.Unlock()

	req, err := decodeRequest(msg.Request)
	if err != nil {
		a.reject(r, herrors.Internal, err.Error())
		return
	}
	if req.Contract != "" && req.Contract != runtime.ContractV1 {
		a.reject(r, herrors.UnsupportedContract,
			fmt.Sprintf("handler contract %q is not %s", req.Contract, runtime.ContractV1))
		return
	}
	password := ""
	if msg.Privileged {
		if msg.Elevation != nil {
			password = msg.Elevation.Password
		}
		// The password lives only in this call. Dropping it here keeps a
		// retry, a buffer, or a diagnostic from ever seeing it
		// (docs/agent-protocol.md section 9).
		msg.Elevation = nil
		if req.Kind == runtime.KindOptions {
			// An option provider never runs with administrator rights
			// (docs/handler-contract.md section 4.5).
			a.reject(r, herrors.ElevationDenied,
				"an option provider never runs with administrator rights")
			return
		}
		if allowed, code, reason := a.elevation.check(req.Capability, req.Operation, password); !allowed {
			a.reject(r, code, reason)
			return
		}
	}
	dir, err := a.cache.Ensure(ctx, msg.Bundle)
	if err != nil {
		if a.isAbandoned(r) {
			a.forgetRun(r)
			return
		}
		code := herrors.BundleUnavailable
		if errors.Is(err, herrors.HandlerIntegrity) {
			code = herrors.HandlerIntegrity
		}
		a.reject(r, code, err.Error())
		return
	}
	if a.isAbandoned(r) {
		a.forgetRun(r)
		return
	}
	limit := msg.Limits.LogLineBytes
	outcome := &runner.Outcome{}
	rep := &runner.Runner{
		EnvAllow:      append([]string(nil), a.cfg.EnvAllow...),
		ResponseLimit: int64(msg.Limits.ResponseBytes),
		GracePeriod:   time.Duration(msg.GracePeriod) * time.Millisecond,
		WorkRoot:      a.workDir,
		Supervisor:    append([]string(nil), a.cfg.SupervisorCommand...),
	}
	_, runErr := rep.Run(ctx, runner.Spec{
		Request: req,
		Raw:     msg.Request,
		Entry:   msg.Entry,
		Dir:     dir,
		// A privileged handler runs through sudo and the root supervisor
		// on POSIX systems, and with the service's full rights on
		// Windows (docs/handler-contract.md section 7.2).
		Privileged: msg.Privileged,
		// The password reaches sudo on its stdin and is cleared as soon
		// as the launcher wrote the bytes (docs/agent-protocol.md
		// section 9).
		Password: password,
		Log: func(line string) {
			a.emitLog(r, agentproto.StreamStderr, line, limit)
		},
		Started: func() { a.emitStarted(r) },
		Outcome: outcome,
	})
	if !outcome.Started {
		if a.isAbandoned(r) {
			a.forgetRun(r)
			return
		}
		message := "the handler process did not start"
		if runErr != nil {
			message = runErr.Error()
		}
		a.reject(r, herrors.HandlerStartFailed, message)
		return
	}
	a.recordResult(r, outcome)
}

// emitStarted queues the run's started message before any of its log
// lines.
func (a *Agent) emitStarted(r *run) {
	a.mu.Lock()
	if r.forgotten {
		a.mu.Unlock()
		return
	}
	r.pending = append(r.pending, &pendingMessage{message: &agentproto.Started{
		Type:  agentproto.TypeStarted,
		RunID: r.id,
		TS:    agentproto.Timestamp(a.cfg.Now()),
	}})
	s := a.current
	a.mu.Unlock()
	if s != nil {
		s.wakeUp()
	}
}

// emitLog queues one log line of a run, split to the log line limit, and
// enforces the disconnected buffer limit.
func (a *Agent) emitLog(r *run, stream, line string, limit int) {
	chunks := splitLine(line, limit)
	if stream == agentproto.StreamSystem {
		chunks = []string{line}
	}
	a.mu.Lock()
	if r.forgotten {
		a.mu.Unlock()
		return
	}
	for _, chunk := range chunks {
		a.appendLogLocked(r, stream, chunk)
	}
	a.enforceBufferLocked()
	s := a.current
	a.mu.Unlock()
	if s != nil {
		s.wakeUp()
	}
}

// appendLogLocked adds one log message and its buffer accounting. The
// agent mutex must be held.
func (a *Agent) appendLogLocked(r *run, stream, line string) {
	r.seq++
	r.pending = append(r.pending, &pendingMessage{
		seq: r.seq,
		message: &agentproto.Log{
			Type:   agentproto.TypeLog,
			RunID:  r.id,
			Seq:    r.seq,
			TS:     agentproto.Timestamp(a.cfg.Now()),
			Stream: stream,
			Line:   line,
		},
		size: len(line),
	})
	a.buffered += int64(len(line))
	r.logBytes += int64(len(line))
}

// enforceBufferLocked drops the oldest log lines of the run with the most
// buffered data until the buffer fits, then adds one system line that
// counts the dropped lines. It never drops a result. The agent mutex must
// be held (docs/agent-protocol.md section 8.1).
func (a *Agent) enforceBufferLocked() {
	if a.buffered <= a.cfg.BufferLimit {
		return
	}
	// Leave room for the system line that counts the dropped lines, so
	// the buffer still fits the limit after it.
	target := a.cfg.BufferLimit - 64
	if target < 0 {
		target = 0
	}
	dropped := 0
	var victim *run
	for a.buffered > target {
		victim = a.mostBufferedLocked()
		if victim == nil {
			break
		}
		index := oldestLog(victim)
		if index < 0 {
			break
		}
		pm := victim.pending[index]
		victim.pending = append(victim.pending[:index], victim.pending[index+1:]...)
		a.buffered -= int64(pm.size)
		victim.logBytes -= int64(pm.size)
		dropped++
	}
	if dropped > 0 && victim != nil {
		a.appendLogLocked(victim, agentproto.StreamSystem,
			fmt.Sprintf("dropped %d buffered log lines", dropped))
	}
}

// mostBufferedLocked returns the run with the most buffered log data. The
// agent mutex must be held.
func (a *Agent) mostBufferedLocked() *run {
	var victim *run
	for _, r := range a.runs {
		if r.forgotten || r.logBytes <= 0 {
			continue
		}
		if victim == nil || r.logBytes > victim.logBytes {
			victim = r
		}
	}
	return victim
}

// oldestLog returns the index of the run's oldest log message, or -1.
func oldestLog(r *run) int {
	for i, pm := range r.pending {
		if _, ok := pm.message.(*agentproto.Log); ok {
			return i
		}
	}
	return -1
}

// cancelRun carries out one cancel message
// (docs/agent-protocol.md section 4.9).
func (a *Agent) cancelRun(runID string) {
	a.mu.Lock()
	r := a.runs[runID]
	if r == nil || r.finished || r.abandoned || r.forgotten || r.cancel == nil {
		a.mu.Unlock()
		return
	}
	// The system line is recorded under the same lock as the result, so
	// it always precedes the result in seq order.
	a.appendLogLocked(r, agentproto.StreamSystem, "stop requested")
	s := a.current
	cancel := r.cancel
	a.mu.Unlock()
	if s != nil {
		s.wakeUp()
	}
	cancel()
}

// handleAck drops the messages the Gateway stored and forgets a run whose
// result was acknowledged (docs/agent-protocol.md section 4.8).
func (a *Agent) handleAck(runID string, seq int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r := a.runs[runID]; r != nil {
		a.applyAckLocked(r, seq)
	}
}

// applyAckLocked drops one run's acknowledged messages. The agent mutex
// must be held.
func (a *Agent) applyAckLocked(r *run, seq int64) {
	if seq > r.acked {
		r.acked = seq
	}
	kept := r.pending[:0]
	for _, pm := range r.pending {
		if pm.seq > 0 && pm.seq <= r.acked {
			a.buffered -= int64(pm.size)
			r.logBytes -= int64(pm.size)
			continue
		}
		kept = append(kept, pm)
	}
	r.pending = kept
	if r.resultSeq > 0 && r.acked >= r.resultSeq {
		a.forgetLocked(r)
	}
}

// abandonLocked stops a run the Gateway answered abandon for and discards
// its buffered messages without sending anything more. The agent mutex
// must be held (docs/agent-protocol.md section 8.2).
func (a *Agent) abandonLocked(r *run) {
	r.abandoned = true
	if r.cancel != nil {
		r.cancel()
	}
	a.forgetLocked(r)
}

// forgetLocked removes one run from the agent's state and the journal. The
// agent mutex must be held.
func (a *Agent) forgetLocked(r *run) {
	r.forgotten = true
	for _, pm := range r.pending {
		a.buffered -= int64(pm.size)
		r.logBytes -= int64(pm.size)
	}
	r.pending = nil
	if a.runs[r.id] == r {
		delete(a.runs, r.id)
	}
	_ = a.journal.remove(r.id)
}

// forgetRun removes one run from the caller's goroutine.
func (a *Agent) forgetRun(r *run) {
	a.mu.Lock()
	a.forgetLocked(r)
	a.mu.Unlock()
}

// isAbandoned reports whether the Gateway abandoned a run.
func (a *Agent) isAbandoned(r *run) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return r.abandoned || r.forgotten
}

// recordResult records the run's result message and queues it. The result
// is never dropped from the buffer (docs/agent-protocol.md section 8.1).
func (a *Agent) recordResult(r *run, outcome *runner.Outcome) {
	a.mu.Lock()
	abandoned := r.abandoned
	forgotten := r.forgotten
	a.mu.Unlock()
	if forgotten {
		return
	}
	if outcome.StopReason == agentproto.StopDeadline && !abandoned {
		a.emitLog(r, agentproto.StreamSystem, "the deadline passed", 0)
	}
	result := buildResult(r.id, outcome, abandoned, a.cfg.Now())
	a.mu.Lock()
	if r.forgotten {
		a.mu.Unlock()
		return
	}
	r.seq++
	result.Seq = r.seq
	r.resultSeq = r.seq
	r.finished = true
	r.pending = append(r.pending, &pendingMessage{seq: r.seq, message: result})
	s := a.current
	a.mu.Unlock()
	if s != nil {
		s.wakeUp()
	}
}

// buildResult turns the runner's outcome into the protocol's result
// message (docs/agent-protocol.md section 4.7).
func buildResult(runID string, outcome *runner.Outcome, abandoned bool, now time.Time) *agentproto.Result {
	result := &agentproto.Result{
		Type:       agentproto.TypeResult,
		RunID:      runID,
		TS:         agentproto.Timestamp(now),
		ExitCode:   outcome.ExitCode,
		DurationMS: outcome.Duration.Milliseconds(),
	}
	if outcome.Signal != "" {
		signal := outcome.Signal
		result.Signal = &signal
	}
	stopReason := outcome.StopReason
	if abandoned {
		stopReason = agentproto.StopAbandon
	}
	if stopReason != "" {
		reason := stopReason
		result.StopReason = &reason
	}
	if outcome.OutputError != "" {
		outputError := outcome.OutputError
		result.OutputError = &outputError
	} else if response := bytes.TrimSpace(outcome.Stdout); len(response) > 0 && json.Valid(response) {
		result.Response = json.RawMessage(response)
	}
	return result
}

// reject answers that the handler did not start and forgets the run
// (docs/agent-protocol.md section 4.5).
func (a *Agent) reject(r *run, code *herrors.Code, message string) {
	a.mu.Lock()
	if r.forgotten {
		a.mu.Unlock()
		return
	}
	a.forgetLocked(r)
	a.mu.Unlock()
	a.sendControl(&agentproto.Rejected{
		Type:    agentproto.TypeRejected,
		RunID:   r.id,
		Code:    code.Name(),
		Message: shorten(message, 1024),
	})
}

// shorten keeps one message under a byte limit.
func shorten(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := text[:limit]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// splitLine splits one log line to the log line limit without breaking a
// UTF-8 rune (docs/handler-contract.md section 4.4).
func splitLine(line string, limit int) []string {
	if limit <= 0 || len(line) <= limit {
		return []string{line}
	}
	var chunks []string
	for len(line) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		if cut == 0 {
			cut = limit
		}
		chunks = append(chunks, line[:cut])
		line = line[cut:]
	}
	return append(chunks, line)
}

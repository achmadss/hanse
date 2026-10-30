package agentconn

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/achmadss/hanse/internal/agentproto"
	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/runtime"
)

// welcomeFor decides the action for every run of hello and collects the
// runs the Gateway must send again. It follows the table of
// docs/agent-protocol.md section 8.2.
func (h *Hub) welcomeFor(nodeID string, hello *agentproto.Hello) (agentproto.Welcome, []*agentproto.Run) {
	welcome := agentproto.Welcome{
		Type:              agentproto.TypeWelcome,
		Node:              nodeID,
		GatewayVersion:    h.cfg.GatewayVersion,
		HeartbeatInterval: secondsAtLeastOne(h.cfg.HeartbeatInterval),
		HeartbeatTimeout:  secondsAtLeastOne(h.cfg.HeartbeatTimeout),
		Runs:              []agentproto.WelcomeRun{},
	}
	listed := make(map[string]bool, len(hello.Runs))
	var resends []*agentproto.Run
	var systemLines []string
	failures := map[*run]error{}

	h.mu.Lock()
	for _, state := range hello.Runs {
		listed[state.RunID] = true
		r := h.runs[state.RunID]
		if r == nil {
			// A run the Gateway no longer tracks cannot resume: it
			// either finished (its ack was lost) or the Gateway
			// restarted. The agent must stop it and forget its buffer.
			welcome.Runs = append(welcome.Runs, agentproto.WelcomeRun{
				RunID: state.RunID, Action: agentproto.ActionAbandon,
			})
			continue
		}
		switch {
		case r.isAbandoned():
			welcome.Runs = append(welcome.Runs, agentproto.WelcomeRun{
				RunID: state.RunID, Action: agentproto.ActionAbandon,
			})
		case r.option:
			// Option calls are not resumed: the client request already
			// failed.
			r.markAbandoned()
			welcome.Runs = append(welcome.Runs, agentproto.WelcomeRun{
				RunID: state.RunID, Action: agentproto.ActionAbandon,
			})
		case state.State == agentproto.RunStateLost:
			failures[r] = herrors.New(herrors.ExecutorConnectionLost,
				"the agent restarted and lost the run's outcome",
				map[string]any{"node": nodeID, "run_id": r.id})
			welcome.Runs = append(welcome.Runs, agentproto.WelcomeRun{
				RunID: state.RunID, Action: agentproto.ActionAbandon,
			})
			systemLines = append(systemLines, r.id)
		case r.isFinished():
			welcome.Runs = append(welcome.Runs, agentproto.WelcomeRun{
				RunID: state.RunID, Action: agentproto.ActionAbandon,
			})
		default:
			acked := r.ackedSeq()
			welcome.Runs = append(welcome.Runs, agentproto.WelcomeRun{
				RunID: state.RunID, Action: agentproto.ActionResume, AckedSeq: &acked,
			})
		}
	}
	for _, r := range h.runs {
		if r.node != nodeID || listed[r.id] || r.isAbandoned() || r.isFinished() {
			continue
		}
		if r.privileged {
			// The Gateway does not keep the elevation password, so it
			// must not send a privileged run again
			// (docs/agent-protocol.md section 9).
			failures[r] = herrors.New(herrors.ExecutorConnectionLost,
				"the agent never accepted the privileged run, and the Gateway no longer holds its password",
				map[string]any{"node": nodeID, "run_id": r.id})
			systemLines = append(systemLines, r.id)
			continue
		}
		resends = append(resends, r.message)
	}
	h.mu.Unlock()

	for r, failure := range failures {
		r.abandon(runtime.Response{}, failure)
	}
	for _, id := range systemLines {
		h.system(id, "the run ended with executor_connection_lost after a reconnect")
	}
	return welcome, resends
}

// handle processes one agent message. A non-zero close code means the
// connection loop writes reply, when it is not nil, and closes with that
// code (docs/agent-protocol.md section 10).
func (h *Hub) handle(c *conn, msg agentproto.Message) (reply agentproto.Message, code int, reason string) {
	switch m := msg.(type) {
	case *agentproto.Heartbeat:
		return nil, 0, ""
	case *agentproto.Started:
		return nil, 0, ""
	case *agentproto.Log:
		h.receiveLog(c, m)
		return nil, 0, ""
	case *agentproto.Result:
		h.receiveResult(c, m)
		return nil, 0, ""
	case *agentproto.Rejected:
		h.receiveRejected(m)
		return nil, 0, ""
	case *agentproto.Error:
		h.system(m.RunID, "the agent reported a protocol error: "+m.Code+": "+m.Message)
		return nil, 0, ""
	default:
		return &agentproto.Error{
			Type:    agentproto.TypeError,
			Code:    "unexpected_message",
			Message: fmt.Sprintf("message type %q is not sent by the agent", msg.MessageType()),
		}, closeProtocolError, "unexpected message"
	}
}

// receiveLog stores one log line and acknowledges it at once, so a
// reconnecting agent resends only what the Gateway still misses
// (docs/agent-protocol.md sections 4.6 and 4.8).
func (h *Hub) receiveLog(c *conn, m *agentproto.Log) {
	h.mu.Lock()
	r := h.runs[m.RunID]
	h.mu.Unlock()
	if r == nil {
		return
	}
	seq, fresh := r.acceptSeq(m.Seq)
	if !fresh {
		c.enqueue(&agentproto.Ack{Type: agentproto.TypeAck, RunID: m.RunID, Seq: seq})
		return
	}
	ts := parseTimestamp(m.TS, h.now())
	h.logLine(m.RunID, ts, m.Stream, trimLine(m.Line, h.cfg.LogLineBytes))
	c.enqueue(&agentproto.Ack{Type: agentproto.TypeAck, RunID: m.RunID, Seq: seq})
}

// receiveResult finishes one run and acknowledges its last sequence
// number.
func (h *Hub) receiveResult(c *conn, m *agentproto.Result) {
	h.mu.Lock()
	r := h.runs[m.RunID]
	h.mu.Unlock()
	if r == nil {
		return
	}
	seq, fresh := r.acceptSeq(m.Seq)
	c.enqueue(&agentproto.Ack{Type: agentproto.TypeAck, RunID: m.RunID, Seq: seq})
	if !fresh {
		return
	}
	resp, err := resultResponse(m)
	r.finish(resp, err)
	h.remove(r)
}

// receiveRejected fails one run with the code the agent reported
// (docs/agent-protocol.md section 4.5).
func (h *Hub) receiveRejected(m *agentproto.Rejected) {
	h.mu.Lock()
	r := h.runs[m.RunID]
	h.mu.Unlock()
	if r == nil {
		return
	}
	err := herrors.New(rejectedCode(m.Code), m.Message, map[string]any{
		"run_id":        m.RunID,
		"rejected_code": m.Code,
	})
	r.finish(runtime.Response{}, err)
	h.remove(r)
}

// resultResponse turns a result message into the runtime response the
// operation manager records. A result without a valid response envelope
// fails with a handler code, per docs/handler-contract.md section 5.
func resultResponse(m *agentproto.Result) (runtime.Response, error) {
	if len(m.Response) > 0 && string(m.Response) != "null" {
		var envelope struct {
			Status  string          `json:"status"`
			Result  json.RawMessage `json:"result"`
			Summary string          `json:"summary"`
			Error   *struct {
				Code    string         `json:"code"`
				Message string         `json:"message"`
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		if err := json.Unmarshal(m.Response, &envelope); err != nil {
			return runtime.Response{}, resultFailure(herrors.HandlerOutputInvalid,
				"the handler response is not one JSON object")
		}
		resp := runtime.Response{
			Status:  envelope.Status,
			Result:  append(json.RawMessage(nil), envelope.Result...),
			Summary: envelope.Summary,
		}
		if envelope.Error != nil {
			resp.Err = &runtime.ErrInfo{
				Code:    envelope.Error.Code,
				Message: envelope.Error.Message,
				Details: envelope.Error.Details,
			}
		}
		switch envelope.Status {
		case runtime.StatusOK:
			return resp, nil
		case runtime.StatusError:
			if resp.Err == nil {
				return runtime.Response{}, resultFailure(herrors.HandlerOutputInvalid,
					"the handler reported an error without an error object")
			}
			return resp, nil
		case runtime.StatusCancelled:
			return resp, nil
		default:
			return runtime.Response{}, resultFailure(herrors.HandlerOutputInvalid,
				fmt.Sprintf("the handler returned status %q", envelope.Status))
		}
	}
	if m.StopReason != nil && *m.StopReason == agentproto.StopCancel {
		return runtime.Response{Status: runtime.StatusCancelled}, nil
	}
	if m.OutputError != nil {
		return runtime.Response{}, resultFailure(herrors.HandlerOutputInvalid,
			"the handler produced no valid response: "+*m.OutputError)
	}
	if m.ExitCode != nil && *m.ExitCode != 0 {
		return runtime.Response{}, resultFailure(herrors.HandlerExit,
			fmt.Sprintf("the handler exited with code %d", *m.ExitCode))
	}
	return runtime.Response{}, resultFailure(herrors.HandlerOutputInvalid,
		"the handler produced no response")
}

// resultFailure builds a dispatch error with the run's protocol details.
func resultFailure(code *herrors.Code, message string) error {
	return herrors.New(code, message, nil)
}

// rejectedCode maps a rejected code to the shared code set. An unknown
// code from a newer agent maps to handler_start_failed, because the
// handler did not start.
func rejectedCode(name string) *herrors.Code {
	for _, code := range herrors.All() {
		if code.Name() == name {
			return code
		}
	}
	return herrors.HandlerStartFailed
}

// parseTimestamp reads one protocol timestamp, falling back to fallback
// when it does not parse.
func parseTimestamp(value string, fallback time.Time) time.Time {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return fallback
	}
	return t.UTC()
}

// secondsAtLeastOne renders a duration in whole seconds, at least 1, for
// welcome's integer fields.
func secondsAtLeastOne(d time.Duration) int {
	seconds := int(d / time.Second)
	if seconds < 1 {
		return 1
	}
	return seconds
}

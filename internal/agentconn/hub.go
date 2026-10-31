// Package agentconn accepts node agent connections on the Gateway, tracks
// which nodes are online, and sends runs, cancellations, and acks over the
// connection (docs/agent-protocol.md sections 3, 4, 7, and 8).
//
// The hub is the Gateway's side of the protocol. It authenticates each
// connection with the node credential from internal/enroll, checks the
// agent's hello against nodes.yaml, keeps at most one connection per node,
// exchanges heartbeats, and records presence in the node registry. When a
// connection drops during a run it waits for the configured reconnect
// window before it fails the run with executor_connection_lost.
//
// A Hub implements enroll.Disconnecter, so revoking a node's credential or
// enrolling it again closes its live connection.
package agentconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/achmadss/hanse/internal/agentproto"
	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/runtime"
)

// Defaults of the protocol and the Gateway configuration
// (docs/agent-protocol.md section 11, docs/configuration.md section 3).
const (
	// DefaultHeartbeatInterval is the heartbeat interval in welcome.
	DefaultHeartbeatInterval = 15 * time.Second
	// DefaultHeartbeatTimeout is how long silence may last before the
	// connection counts as lost.
	DefaultHeartbeatTimeout = 45 * time.Second
	// DefaultReconnectWindow is how long the Gateway waits for a
	// disconnected agent before it fails the node's running operations.
	DefaultReconnectWindow = 5 * time.Minute
	// DefaultGracePeriod is the time between a stop request and the kill.
	DefaultGracePeriod = 10 * time.Second
	// DefaultResponseBytes is the handler response limit.
	DefaultResponseBytes = 1 << 20
	// DefaultLogLineBytes is the log line limit.
	DefaultLogLineBytes = 16 << 10
)

// LogSink receives one log line of a run. RunID is the operation ID or an
// `opt_` ID. Stream is stderr for a handler line and system for a Gateway
// event. Issue #19 wires the operation log store here; a nil sink
// discards every line.
type LogSink interface {
	Log(runID string, ts time.Time, stream, line string)
}

// Config is what a Hub needs.
type Config struct {
	// Registry holds the loaded capabilities and nodes. Required.
	Registry *registry.Registry
	// Verify returns the node ID of a plain node credential. It is
	// enroll.Service.Verify. Required.
	Verify func(credential string) (string, error)
	// GatewayVersion is reported in welcome.
	GatewayVersion string
	// HeartbeatInterval and HeartbeatTimeout are sent in welcome. Zero
	// fields take the documented defaults.
	HeartbeatInterval time.Duration
	HeartbeatTimeout  time.Duration
	// ReconnectWindow is how long the Gateway waits for a disconnected
	// agent before it fails the node's running operations.
	ReconnectWindow time.Duration
	// GracePeriod is the default time between a stop request and the
	// kill, used when the dispatcher does not pass one.
	GracePeriod time.Duration
	// Logs receives log lines and Gateway system events. Nil discards.
	Logs LogSink
	// Now returns the current time. Nil means time.Now. Tests inject a
	// clock here.
	Now func() time.Time
	// After returns a channel that receives once after d. Nil means
	// time.After. Tests inject it for the heartbeat and reconnect timers.
	After func(d time.Duration) <-chan time.Time
	// HelloTimeout bounds the hello and welcome exchange. Zero means
	// agentproto.HelloTimeout.
	HelloTimeout time.Duration
	// MaxMessageBytes is the largest accepted WebSocket message. Zero
	// means agentproto.MaxMessageBytes.
	MaxMessageBytes int64
	// LogLineBytes bounds one stored log line. Zero means
	// DefaultLogLineBytes.
	LogLineBytes int
}

// Call is one run the hub sends to a node agent.
type Call struct {
	// RunID is the operation ID or an `opt_` ID.
	RunID string
	// Node is the executor node ID. It comes from the request envelope,
	// never from the agent.
	Node string
	// Capability and Operation name the call's operation. They are used
	// for logging and to know whether the call is an option provider
	// call, which is never resumed.
	Capability string
	Operation  string
	// Option marks an option provider call. Those are not resumed
	// (docs/agent-protocol.md section 8.2).
	Option bool
	// Bundle is the capability's bundle hash and Entry its entry command
	// for the node's OS.
	Bundle string
	Entry  []string
	// Request is the handler request envelope, already encoded as the
	// handler contract's JSON.
	Request json.RawMessage
	// Privileged reports whether the run needs administrator rights.
	// Password is the elevation password of a privileged run: it travels
	// in the run message only and is never stored.
	Privileged bool
	Password   string
	// GracePeriod is the time between a stop request and the kill.
	// Zero means Config.GracePeriod.
	GracePeriod time.Duration
	// ResponseBytes and LogLineBytes are the limits the agent enforces.
	// Zero fields take the documented defaults.
	ResponseBytes int
	LogLineBytes  int
}

// Hub is the Gateway's connection hub. It is safe for concurrent use: a
// connection goroutine, the dispatcher, and the enrollment service call it
// at the same time.
type Hub struct {
	cfg Config

	mu    sync.Mutex
	conns map[string]*conn
	runs  map[string]*run
	// windows maps a node with a pending reconnect wait to the channel
	// that cancels it.
	windows map[string]chan struct{}
}

// New builds a hub. It fails when the registry or the credential check is
// missing, because a hub without either cannot accept a connection.
func New(cfg Config) (*Hub, error) {
	if cfg.Registry == nil {
		return nil, herrors.New(herrors.Internal,
			"the agent connection hub needs a node registry", nil)
	}
	if cfg.Verify == nil {
		return nil, herrors.New(herrors.Internal,
			"the agent connection hub needs a node credential check", nil)
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if cfg.HeartbeatTimeout <= 0 {
		cfg.HeartbeatTimeout = DefaultHeartbeatTimeout
	}
	if cfg.ReconnectWindow <= 0 {
		cfg.ReconnectWindow = DefaultReconnectWindow
	}
	if cfg.GracePeriod <= 0 {
		cfg.GracePeriod = DefaultGracePeriod
	}
	if cfg.HelloTimeout <= 0 {
		cfg.HelloTimeout = agentproto.HelloTimeout
	}
	if cfg.MaxMessageBytes <= 0 {
		cfg.MaxMessageBytes = agentproto.MaxMessageBytes
	}
	if cfg.LogLineBytes <= 0 {
		cfg.LogLineBytes = DefaultLogLineBytes
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.After == nil {
		cfg.After = time.After
	}
	return &Hub{
		cfg:     cfg,
		conns:   map[string]*conn{},
		runs:    map[string]*run{},
		windows: map[string]chan struct{}{},
	}, nil
}

// Config returns the effective configuration, including the defaults New
// filled. It exists so tests and wiring can read the resolved values.
func (h *Hub) Config() Config { return h.cfg }

// now returns the hub's current time.
func (h *Hub) now() time.Time { return h.cfg.Now().UTC() }

// after waits d on the hub's injected clock.
func (h *Hub) after(d time.Duration) <-chan time.Time { return h.cfg.After(d) }

// Authenticate returns the node ID a plain node credential belongs to. A
// missing, wrong, or revoked credential is the AuthenticationFailed code,
// HTTP 401 (docs/agent-protocol.md section 3.1).
func (h *Hub) Authenticate(credential string) (string, error) {
	if h == nil || h.cfg.Verify == nil {
		return "", herrors.New(herrors.Internal,
			"the agent connection hub is not available", nil)
	}
	node, err := h.cfg.Verify(credential)
	if err != nil {
		return "", err
	}
	if node == "" {
		return "", herrors.New(herrors.AuthenticationFailed, "", nil)
	}
	return node, nil
}

// Disconnect closes a node's live connection with the revoked-credential
// close code. It implements enroll.Disconnecter, so a revoke or a new
// enrollment closes the connection that used the replaced credential
// (docs/agent-protocol.md sections 2.1 and 10).
func (h *Hub) Disconnect(nodeID string) {
	if h == nil || nodeID == "" {
		return
	}
	h.mu.Lock()
	c := h.conns[nodeID]
	h.mu.Unlock()
	if c != nil {
		c.close(closeRevoked, "the node credential was revoked")
	}
}

// NodeRemoved closes the live connection of a node that a configuration
// reload removed from nodes.yaml, with the node-mismatch close code the
// protocol uses when the node is gone (docs/agent-protocol.md section 10).
// The agent cannot connect again until the node is back in nodes.yaml.
func (h *Hub) NodeRemoved(nodeID string) {
	if h == nil || nodeID == "" {
		return
	}
	h.mu.Lock()
	c := h.conns[nodeID]
	h.mu.Unlock()
	if c != nil {
		c.close(closeNodeMismatch, "the node was removed from nodes.yaml")
	}
}

// Online reports whether the node's agent is connected.
func (h *Hub) Online(nodeID string) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	c := h.conns[nodeID]
	h.mu.Unlock()
	return c != nil
}

// Execute sends one run and waits for its result. A node without a
// connection fails with ExecutorUnavailable, so the request never silently
// runs elsewhere. A cancelled context sends a cancel message and then waits
// for the agent's result, so a handler that finishes during the grace
// period reports its own final state. A disconnected agent has the run
// recorded for an abandon answer on reconnect.
func (h *Hub) Execute(ctx context.Context, call *Call) (runtime.Response, error) {
	if h == nil {
		return runtime.Response{}, herrors.New(herrors.Internal,
			"the agent connection hub is not available", nil)
	}
	if call == nil || call.RunID == "" || call.Node == "" {
		return runtime.Response{}, herrors.New(herrors.Internal,
			"the agent dispatcher sent a call without a run ID or node", nil)
	}
	msg, err := h.runMessage(call)
	if err != nil {
		return runtime.Response{}, err
	}
	r := &run{
		id:         call.RunID,
		node:       call.Node,
		option:     call.Option,
		privileged: call.Privileged,
		// The stored copy never holds the elevation password: a
		// privileged run is not sent again after a reconnect
		// (docs/agent-protocol.md section 9).
		message: storedRunMessage(msg),
		created: h.now(),
		done:    make(chan struct{}),
	}
	h.mu.Lock()
	if _, duplicate := h.runs[call.RunID]; duplicate {
		h.mu.Unlock()
		return runtime.Response{}, herrors.New(herrors.Internal,
			fmt.Sprintf("run %q is already active", call.RunID), nil)
	}
	h.purgeStaleLocked(call.Node)
	h.runs[call.RunID] = r
	c := h.conns[call.Node]
	h.mu.Unlock()

	if c == nil {
		h.remove(r)
		return runtime.Response{}, herrors.New(herrors.ExecutorUnavailable,
			fmt.Sprintf("executor %q has no agent connection", call.Node),
			map[string]any{"executor": call.Node})
	}
	if !c.enqueue(msg) {
		h.remove(r)
		return runtime.Response{}, herrors.New(herrors.ExecutorUnavailable,
			fmt.Sprintf("the agent connection of %q is not accepting runs", call.Node),
			map[string]any{"executor": call.Node})
	}

	select {
	case <-r.done:
		return r.result()
	case <-ctx.Done():
		h.stop(r, ctx.Err())
		// The agent runs the handler's grace period and then reports how
		// it ended. Wait for that result, so a handler that returns ok
		// during the grace period ends completed
		// (docs/handler-contract.md section 5), and the operation stays
		// running until then (docs/architecture.md section 9.5).
		select {
		case <-r.done:
			return r.result()
		case <-h.after(stopWait(msg)):
			return runtime.Response{}, ctx.Err()
		}
	}
}

// stopWait is how long the Gateway waits for the agent's result after it
// sends the stop request: the handler's grace period plus time for the
// result message to travel back. The agent starts its own grace timer when
// the cancel message arrives, so the extra time covers that hop.
func stopWait(msg *agentproto.Run) time.Duration {
	grace := time.Duration(msg.GracePeriod) * time.Millisecond
	return grace + stopResultSlack
}

// stopResultSlack is the allowance after the grace period for the agent's
// result message.
const stopResultSlack = 250 * time.Millisecond

// stop cancels one run. A connected agent gets a cancel message; a
// disconnected one has the run marked for abandon, so the agent stops it
// after it reconnects (docs/agent-protocol.md sections 7 and 8.2). cause is
// the dispatch context's error, which names whether a client cancelled the
// run or its deadline passed.
func (h *Hub) stop(r *run, cause error) {
	h.mu.Lock()
	c := h.conns[r.node]
	h.mu.Unlock()
	deadline := errors.Is(cause, context.DeadlineExceeded)
	if c != nil && c.enqueue(&agentproto.Cancel{Type: agentproto.TypeCancel, RunID: r.id}) {
		// The client asked to stop the run, so a reconnect must not
		// resume it even if the agent's result never arrives.
		r.markAbandoned()
		h.forgetAfter(r)
		if deadline {
			h.system(r.id, "the operation reached its deadline; a stop request was sent to the agent")
		} else {
			h.system(r.id, "the client cancelled the operation; a stop request was sent to the agent")
		}
		return
	}
	r.markAbandoned()
	h.forgetAfter(r)
	if deadline {
		h.system(r.id, "the agent is disconnected: the operation reached its deadline and the agent is told to abandon it after it reconnects")
		return
	}
	h.system(r.id, "the agent is disconnected: the operation is cancelled and the agent is told to abandon it after it reconnects")
}

// forgetAfter drops a cancelled or abandoned run once a reconnect can no
// longer ask for its abandon answer. A reconnect inside the window gets
// the answer from welcome and drops the run at once.
func (h *Hub) forgetAfter(r *run) {
	go func() {
		<-h.after(h.cfg.ReconnectWindow)
		h.remove(r)
	}()
}

// runMessage builds the run message of one call.
func (h *Hub) runMessage(call *Call) (*agentproto.Run, error) {
	if len(call.Request) == 0 {
		return nil, herrors.New(herrors.Internal,
			"the agent dispatcher sent a call without a request envelope", nil)
	}
	if len(call.Entry) == 0 {
		return nil, herrors.New(herrors.ExecutorNotEligible,
			fmt.Sprintf("operation %q of capability %q has no entry command for node %q",
				call.Operation, call.Capability, call.Node),
			map[string]any{"capability": call.Capability, "operation": call.Operation, "executor": call.Node})
	}
	grace := call.GracePeriod
	if grace <= 0 {
		grace = h.cfg.GracePeriod
	}
	responseBytes := call.ResponseBytes
	if responseBytes <= 0 {
		responseBytes = DefaultResponseBytes
	}
	logLineBytes := call.LogLineBytes
	if logLineBytes <= 0 {
		logLineBytes = DefaultLogLineBytes
	}
	msg := &agentproto.Run{
		Type:        agentproto.TypeRun,
		RunID:       call.RunID,
		Bundle:      call.Bundle,
		Entry:       append([]string(nil), call.Entry...),
		Request:     append([]byte(nil), call.Request...),
		GracePeriod: grace.Milliseconds(),
		Limits: agentproto.Limits{
			ResponseBytes: responseBytes,
			LogLineBytes:  logLineBytes,
		},
	}
	if call.Privileged {
		if call.Password == "" {
			return nil, herrors.New(herrors.ElevationRequired,
				"a privileged run needs a password", nil)
		}
		msg.Privileged = true
		msg.Elevation = &agentproto.Elevation{Password: call.Password}
	}
	if err := msg.Validate(); err != nil {
		return nil, herrors.New(herrors.Internal,
			"the run message is invalid: "+err.Error(), nil)
	}
	return msg, nil
}

// storedRunMessage returns a copy of a run message without its elevation
// input. The hub keeps the copy for a possible resend on reconnect, and a
// password must never be stored (docs/agent-protocol.md section 9).
func storedRunMessage(msg *agentproto.Run) *agentproto.Run {
	stored := *msg
	stored.Elevation = nil
	return &stored
}

// remove drops one run from the hub.
func (h *Hub) remove(r *run) {
	h.mu.Lock()
	if h.runs[r.id] == r {
		delete(h.runs, r.id)
	}
	h.mu.Unlock()
}

// purgeStaleLocked drops runs of one node that ended and can no longer
// matter to a reconnect: an abandoned run younger than two reconnect
// windows is kept so a late hello still gets abandon
// (docs/agent-protocol.md section 8.2).
func (h *Hub) purgeStaleLocked(node string) {
	cutoff := h.now().Add(-2 * h.cfg.ReconnectWindow)
	for id, r := range h.runs {
		if r.node != node || !r.isAbandoned() {
			continue
		}
		if r.createdAt().Before(cutoff) {
			delete(h.runs, id)
		}
	}
}

// system records one Gateway system log line for a run.
func (h *Hub) system(runID, line string) {
	h.logLine(runID, h.now(), agentproto.StreamSystem, line)
}

// logLine sends one line to the configured sink.
func (h *Hub) logLine(runID string, ts time.Time, stream, line string) {
	if h.cfg.Logs == nil {
		return
	}
	h.cfg.Logs.Log(runID, ts, stream, line)
}

// trimLine bounds one log line to limit bytes without splitting a UTF-8
// rune. A shorter line is returned unchanged.
func trimLine(line string, limit int) string {
	if limit <= 0 || len(line) <= limit {
		return line
	}
	cut := line[:limit]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// normalizeReason keeps a close reason under the WebSocket 123-byte limit.
func normalizeReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if len(reason) <= 120 {
		return reason
	}
	return reason[:120]
}

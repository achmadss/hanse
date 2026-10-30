package agentconn

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/achmadss/hanse/internal/agentproto"
	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/runtime"
)

// WebSocket close codes of the agent protocol (docs/agent-protocol.md
// section 10). Codes 4001 to 4005 are sent by the Gateway.
const (
	closeProtocolError       = websocket.StatusCode(4000)
	closeRevoked             = websocket.StatusCode(4001)
	closeReplaced            = websocket.StatusCode(4002)
	closeUnsupportedProtocol = websocket.StatusCode(4003)
	closeNodeMismatch        = websocket.StatusCode(4004)
	closeNodeDisabled        = websocket.StatusCode(4005)
)

// writeTimeout bounds one WebSocket write, so a stalled peer cannot block
// the connection loop forever.
const writeTimeout = 10 * time.Second

// conn is one live agent connection. Only the connection loop writes to
// the socket, and it is the only reader after the hello exchange; hub
// goroutines hand it messages through out. The websocket library allows a
// concurrent Close with one read and one write, which is how a revoke or a
// replacement interrupts the loop.
type conn struct {
	hub          *Hub
	node         string
	agentVersion string
	ws           *websocket.Conn
	ctx          context.Context
	cancel       context.CancelFunc
	out          chan agentproto.Message

	mu          sync.Mutex
	closeCode   websocket.StatusCode
	closeReason string
	closing     bool
}

// readEvent is one message or one read failure of the connection loop.
type readEvent struct {
	msg agentproto.Message
	err error
}

// ServeConnect answers GET /v1/agent/connect. It authenticates the node
// credential before it upgrades, checks the subprotocol, reads the hello,
// checks it against nodes.yaml, replaces an older connection of the same
// node, and answers welcome (docs/agent-protocol.md section 3).
func (h *Hub) ServeConnect(w http.ResponseWriter, r *http.Request) {
	if h == nil {
		writeAgentError(w, herrors.New(herrors.Internal,
			"the agent connection hub is not available", nil))
		return
	}
	credential, ok := bearerToken(r)
	if !ok {
		writeAgentError(w, herrors.New(herrors.AuthenticationFailed, "", nil))
		return
	}
	nodeID, err := h.Authenticate(credential)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	if !offersSubprotocol(r) {
		// The Gateway supports none of the offered versions
		// (docs/agent-protocol.md section 3.1).
		w.Header().Set("Sec-WebSocket-Protocol", agentproto.SubprotocolV1)
		http.Error(w, "the agent protocol version is not supported", http.StatusUpgradeRequired)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{agentproto.SubprotocolV1},
	})
	if err != nil {
		// Accept has already written its own error.
		return
	}
	if got := ws.Subprotocol(); got != agentproto.SubprotocolV1 {
		// The client offered a version the Gateway does not speak.
		_ = ws.Close(closeUnsupportedProtocol, "the agent protocol version is not supported")
		return
	}
	ws.SetReadLimit(h.cfg.MaxMessageBytes)
	ctx, cancel := context.WithCancel(r.Context())
	c := &conn{
		hub:    h,
		node:   nodeID,
		ws:     ws,
		ctx:    ctx,
		cancel: cancel,
		out:    make(chan agentproto.Message, 64),
	}
	defer cancel()

	events := make(chan readEvent, 8)
	go readMessages(ctx, ws, events)
	hello, ok := h.exchangeHello(ctx, c, events)
	if !ok {
		code, reason := c.closeInfo()
		_ = ws.Close(code, reason)
		return
	}
	c.agentVersion = hello.AgentVersion
	welcome, resends := h.welcomeFor(nodeID, hello)
	h.register(c)
	if err := c.writeNow(ctx, &welcome); err != nil {
		h.unregister(c)
		_ = ws.Close(websocket.StatusAbnormalClosure, "")
		return
	}
	for _, msg := range resends {
		if !c.enqueue(msg) {
			h.unregister(c)
			_ = ws.Close(websocket.StatusAbnormalClosure, "")
			return
		}
	}
	h.runLoop(c, events)
}

// exchangeHello reads and checks the agent's hello. It fails and closes
// the connection with the documented code when the message is missing,
// malformed, unsupported, or does not match nodes.yaml.
func (h *Hub) exchangeHello(ctx context.Context, c *conn, events <-chan readEvent) (*agentproto.Hello, bool) {
	timeout := h.after(h.cfg.HelloTimeout)
	for {
		select {
		case <-ctx.Done():
			return nil, false
		case <-timeout:
			c.requestClose(closeProtocolError, "the hello message did not arrive in time")
			return nil, false
		case ev := <-events:
			if ev.err != nil {
				var unknown *agentproto.UnknownMessageError
				if errors.As(ev.err, &unknown) {
					// A newer agent may probe for a feature before its
					// hello. Answer and keep waiting for the hello.
					_ = c.writeNow(ctx, &agentproto.Error{
						Type:    agentproto.TypeError,
						Code:    "unknown_message",
						Message: "unknown message type: " + unknown.Type,
					})
					continue
				}
				_ = c.writeNow(ctx, &agentproto.Error{
					Type:    agentproto.TypeError,
					Code:    "protocol_error",
					Message: "the first message is not a valid hello",
				})
				c.requestClose(closeProtocolError, "the hello message is invalid")
				return nil, false
			}
			hello, ok := ev.msg.(*agentproto.Hello)
			if !ok {
				c.requestClose(closeProtocolError, "the first message is not hello")
				return nil, false
			}
			if hello.Protocol != agentproto.ProtocolV1 {
				c.requestClose(closeUnsupportedProtocol,
					fmt.Sprintf("protocol %s is not supported", hello.Protocol))
				return nil, false
			}
			if code, reason := h.checkHello(c.node, hello); code != 0 {
				c.requestClose(code, reason)
				return nil, false
			}
			return hello, true
		}
	}
}

// checkHello compares the hello with nodes.yaml. The hello never decides
// which node this is: the node ID comes from the credential, and the hello
// must match that node's entry (docs/agent-protocol.md section 4.1).
func (h *Hub) checkHello(nodeID string, hello *agentproto.Hello) (websocket.StatusCode, string) {
	if h.cfg.Registry == nil || h.cfg.Registry.Nodes == nil {
		return closeNodeMismatch, "the node registry is not available"
	}
	view, ok := h.cfg.Registry.Nodes.Get(nodeID)
	if !ok {
		return closeNodeMismatch, "the node is not in nodes.yaml"
	}
	if !view.Enabled {
		return closeNodeDisabled, "the node is disabled in nodes.yaml"
	}
	if view.OS != hello.OS || view.Arch != hello.Arch {
		return closeNodeMismatch, "the agent's OS or architecture differs from nodes.yaml"
	}
	return 0, ""
}

// register makes c the node's one connection and marks the node online. A
// previous connection is closed with 4002.
func (h *Hub) register(c *conn) {
	h.mu.Lock()
	old := h.conns[c.node]
	h.conns[c.node] = c
	h.cancelWindowLocked(c.node)
	h.mu.Unlock()
	if old != nil && old != c {
		old.requestClose(closeReplaced, "a newer connection for this node replaced this one")
	}
	h.setPresence(c.node, registry.StatusOnline, c.agentVersion)
}

// unregister drops c when it is still the node's current connection. A
// replaced connection must not mark its node offline or start a wait.
func (h *Hub) unregister(c *conn) {
	h.mu.Lock()
	current := h.conns[c.node] == c
	if current {
		delete(h.conns, c.node)
	}
	h.mu.Unlock()
	if !current {
		return
	}
	// Keep the last reported agent version so GET /v1/nodes still shows
	// which agent release last connected.
	h.setPresence(c.node, registry.StatusOffline, c.agentVersion)
	h.startReconnectWindow(c.node)
}

// setPresence records a node's connection state and agent version in the
// node registry (docs/architecture.md section 8.2).
func (h *Hub) setPresence(nodeID string, status registry.NodeStatus, agentVersion string) {
	if h.cfg.Registry == nil || h.cfg.Registry.Nodes == nil {
		return
	}
	_ = h.cfg.Registry.Nodes.SetAgentPresence(nodeID, status, agentVersion)
}

// cancelWindowLocked stops a pending reconnect wait of one node. The hub
// mutex must be held.
func (h *Hub) cancelWindowLocked(nodeID string) {
	if cancel, ok := h.windows[nodeID]; ok {
		close(cancel)
		delete(h.windows, nodeID)
	}
}

// startReconnectWindow waits for a disconnected agent and then fails its
// active runs with executor_connection_lost (docs/agent-protocol.md
// section 8.1).
func (h *Hub) startReconnectWindow(nodeID string) {
	h.mu.Lock()
	if _, pending := h.windows[nodeID]; pending {
		h.mu.Unlock()
		return
	}
	if !h.hasPendingLocked(nodeID) {
		h.mu.Unlock()
		return
	}
	cancel := make(chan struct{})
	h.windows[nodeID] = cancel
	h.mu.Unlock()

	go func() {
		select {
		case <-cancel:
		case <-h.after(h.cfg.ReconnectWindow):
			h.expireWindow(nodeID, cancel)
		}
	}()
}

// hasPendingLocked reports whether the node has a run that still waits for
// its result. The hub mutex must be held.
func (h *Hub) hasPendingLocked(nodeID string) bool {
	for _, r := range h.runs {
		if r.node == nodeID && !r.isFinished() && !r.isAbandoned() {
			return true
		}
	}
	return false
}

// expireWindow fails every active run of one node after the reconnect
// wait. The run records stay so a later hello still gets abandon
// (docs/agent-protocol.md section 8.2).
func (h *Hub) expireWindow(nodeID string, cancel chan struct{}) {
	h.mu.Lock()
	if h.windows[nodeID] != cancel {
		h.mu.Unlock()
		return
	}
	delete(h.windows, nodeID)
	var expired []*run
	for _, r := range h.runs {
		if r.node == nodeID && !r.isFinished() && !r.isAbandoned() {
			expired = append(expired, r)
		}
	}
	h.mu.Unlock()

	for _, r := range expired {
		failure := herrors.New(herrors.ExecutorConnectionLost,
			"the node agent did not reconnect within the reconnect window",
			map[string]any{"node": nodeID, "run_id": r.id})
		r.abandon(runtime.Response{}, failure)
		h.system(r.id, "the node agent did not reconnect in time: the operation failed with executor_connection_lost")
	}
}

// runLoop reads agent messages, writes queued messages, sends heartbeats,
// and treats silence over the heartbeat timeout as a lost connection. The
// events channel belongs to the reader goroutine that started before the
// hello exchange, so only one goroutine ever reads the socket.
func (h *Hub) runLoop(c *conn, events <-chan readEvent) {
	defer c.cancel()
	interval := h.cfg.HeartbeatInterval
	timeout := h.cfg.HeartbeatTimeout
	ticker := h.after(interval)
	lastRecv := h.now()
	lastSend := h.now()

	for {
		select {
		case <-c.ctx.Done():
			h.finishConn(c)
			return
		case ev := <-events:
			if ev.err != nil {
				var unknown *agentproto.UnknownMessageError
				if errors.As(ev.err, &unknown) {
					// A newer agent may probe for a message type this
					// Gateway does not know. Answer and keep the
					// connection open (docs/agent-protocol.md §10).
					_ = c.writeNow(c.ctx, &agentproto.Error{
						Type:    agentproto.TypeError,
						Code:    "unknown_message",
						Message: "unknown message type: " + unknown.Type,
					})
					continue
				}
				if isTransportError(ev.err) {
					h.finishConn(c)
					return
				}
				_ = c.writeNow(c.ctx, &agentproto.Error{
					Type:    agentproto.TypeError,
					Code:    "protocol_error",
					Message: "a message is not valid for this protocol",
				})
				c.requestClose(closeProtocolError, "a message is not valid for this protocol")
				continue
			}
			lastRecv = h.now()
			if reply, code, reason := h.handle(c, ev.msg); code != 0 {
				if reply != nil {
					_ = c.writeNow(c.ctx, reply)
				}
				c.requestClose(code, reason)
				continue
			}
		case msg := <-c.out:
			if err := c.writeNow(c.ctx, msg); err != nil {
				h.finishConn(c)
				return
			}
			lastSend = h.now()
		case <-ticker:
			now := h.now()
			if now.Sub(lastRecv) >= timeout {
				c.requestClose(websocket.StatusNormalClosure, "heartbeat timeout")
				ticker = h.after(interval)
				continue
			}
			if now.Sub(lastSend) >= interval {
				if err := c.writeNow(c.ctx, &agentproto.Heartbeat{Type: agentproto.TypeHeartbeat, TS: agentproto.Timestamp(now)}); err != nil {
					h.finishConn(c)
					return
				}
				lastSend = now
			}
			ticker = h.after(interval)
		}
	}
}

// finishConn closes the socket with the requested close code and records
// the node offline.
func (h *Hub) finishConn(c *conn) {
	code, reason := c.closeInfo()
	_ = c.ws.Close(code, reason)
	h.unregister(c)
}

// isTransportError reports whether a read failure ended the connection
// itself, rather than describing a message that the protocol rejects.
func isTransportError(err error) bool {
	var decode *agentproto.DecodeError
	var unknown *agentproto.UnknownMessageError
	return !errors.As(err, &decode) && !errors.As(err, &unknown)
}

// readMessages reads and decodes messages until the connection fails. The
// channel must have room for at least one event.
func readMessages(ctx context.Context, ws *websocket.Conn, events chan<- readEvent) {
	for {
		_, data, err := ws.Read(context.Background())
		if err != nil {
			select {
			case events <- readEvent{err: err}:
			case <-ctx.Done():
			}
			return
		}
		msg, err := agentproto.Decode(data)
		select {
		case events <- readEvent{msg: msg, err: err}:
		case <-ctx.Done():
			return
		}
	}
}

// enqueue hands one message to the connection loop without blocking.
func (c *conn) enqueue(msg agentproto.Message) bool {
	select {
	case c.out <- msg:
		return true
	default:
		return false
	}
}

// writeNow writes one message on the connection loop's goroutine.
func (c *conn) writeNow(ctx context.Context, msg agentproto.Message) error {
	data, err := agentproto.Encode(msg)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return c.ws.Write(writeCtx, websocket.MessageText, data)
}

// requestClose asks the connection loop to close the socket with a code
// and a reason.
func (c *conn) requestClose(code websocket.StatusCode, reason string) {
	c.mu.Lock()
	if !c.closing {
		c.closing = true
		c.closeCode = code
		c.closeReason = normalizeReason(reason)
	}
	c.mu.Unlock()
	c.cancel()
}

// closeInfo returns the close code and reason of a closing connection.
func (c *conn) closeInfo() (websocket.StatusCode, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closing {
		return websocket.StatusNormalClosure, ""
	}
	return c.closeCode, c.closeReason
}

// close implements enroll.Disconnecter's per-node close.
func (c *conn) close(code websocket.StatusCode, reason string) {
	c.requestClose(code, reason)
}

// bearerToken reads the node credential of an Authorization header.
func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// offersSubprotocol reports whether the request offers the supported
// WebSocket subprotocol.
func offersSubprotocol(r *http.Request) bool {
	for _, header := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, offered := range strings.Split(header, ",") {
			if strings.TrimSpace(offered) == agentproto.SubprotocolV1 {
				return true
			}
		}
	}
	return false
}

// writeAgentError writes one API error object with its documented status.
func writeAgentError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(herrors.HTTPStatus(err))
	_ = herrors.WriteJSON(w, err)
}

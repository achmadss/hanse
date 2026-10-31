package agentconn

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/achmadss/hanse/internal/agentproto"
	"github.com/achmadss/hanse/internal/enroll"
	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/runtime"
)

// demoManifest is the fixture capability: an exec handler that node-a may
// run.
const demoManifest = `version: 1
id: demo
name: Demo
description: Demonstration capability
contract: hanse.handler/v1

handler:
  kind: exec
  entry:
    linux: [sh, handler.sh]

operations:
  run:
    description: Run the demonstration handler
    mode: sync
    arguments:
      type: object
`

// recordingSink keeps the log lines the hub stores.
type recordingSink struct {
	mu    sync.Mutex
	lines []recordedLine
}

// recordedLine is one stored log line.
type recordedLine struct {
	runID  string
	ts     time.Time
	stream string
	line   string
}

// Log implements LogSink.
func (s *recordingSink) Log(runID string, ts time.Time, stream, line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, recordedLine{runID: runID, ts: ts, stream: stream, line: line})
}

// find returns the stored lines that contain text.
func (s *recordingSink) find(text string) []recordedLine {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []recordedLine
	for _, line := range s.lines {
		if strings.Contains(line.line, text) {
			out = append(out, line)
		}
	}
	return out
}

// fixture is one hub with its registry, enrollment service, and test server.
type fixture struct {
	hub     *Hub
	reg     *registry.Registry
	service *enroll.Service
	server  *httptest.Server
	logs    *recordingSink
	wsURL   string
	capsDir string
}

// newFixture builds a hub over two nodes: node-a (linux/arm64) and node-b
// (windows/amd64). configure may shorten the timers.
func newFixture(t *testing.T, configure func(*Config)) *fixture {
	t.Helper()
	capsDir := filepath.Join(t.TempDir(), "capabilities")
	demoDir := filepath.Join(capsDir, "demo")
	if err := os.MkdirAll(demoDir, 0o755); err != nil {
		t.Fatalf("create capability directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(demoDir, "manifest.yaml"), []byte(demoManifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(demoDir, "handler.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatalf("write handler: %v", err)
	}
	reg, warnings, err := registry.Load(registry.Options{
		Dirs: []string{capsDir},
		Input: registry.Input{
			Nodes: []registry.Node{
				{ID: "node-a", OS: "linux", Arch: "arm64", Roles: []string{"server"}, Enabled: true},
				{ID: "node-b", OS: "windows", Arch: "amd64", Roles: []string{"server"}, Enabled: true},
			},
			Routes: registry.Routes{Capabilities: map[string]registry.CapabilityRoute{
				"demo": {Executors: []string{"node-a"}, DefaultExecutor: "node-a"},
			}},
		},
	})
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %+v", warnings)
	}
	logs := &recordingSink{}
	var service *enroll.Service
	cfg := Config{
		Registry:       reg,
		GatewayVersion: "1.0.0",
		Logs:           logs,
		// The enrollment service needs the hub as its Disconnecter and
		// the hub needs the service's Verify, so one indirection breaks
		// the cycle.
		Verify: func(credential string) (string, error) {
			if service == nil {
				return "", herrors.New(herrors.AuthenticationFailed, "", nil)
			}
			return service.Verify(credential)
		},
	}
	if configure != nil {
		configure(&cfg)
	}
	hub, err := New(cfg)
	if err != nil {
		t.Fatalf("build hub: %v", err)
	}
	service, err = enroll.Open(enroll.Options{StateDir: t.TempDir(), Disconnect: hub})
	if err != nil {
		t.Fatalf("open enrollment store: %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })
	server := httptest.NewServer(http.HandlerFunc(hub.ServeConnect))
	t.Cleanup(server.Close)
	return &fixture{
		hub:     hub,
		reg:     reg,
		service: service,
		server:  server,
		logs:    logs,
		wsURL:   "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/agent/connect",
		capsDir: capsDir,
	}
}

// credential enrolls one node and returns its credential.
func (f *fixture) credential(t *testing.T, node string) string {
	t.Helper()
	token, err := f.service.IssueJoinToken(node)
	if err != nil {
		t.Fatalf("issue join token: %v", err)
	}
	outcome, err := f.service.Join(token.Plain, nil)
	if err != nil {
		t.Fatalf("join node %s: %v", node, err)
	}
	return outcome.Credential
}

// replaceNodes swaps in a node registry with the same capability routes
// but node-a disabled or enabled, the way a configuration reload changes
// the entry a connection is checked against.
func (f *fixture) replaceNodes(t *testing.T, nodeAEnabled bool) {
	t.Helper()
	alt, warnings, err := registry.Load(registry.Options{
		Dirs: []string{f.capsDir},
		Input: registry.Input{
			Nodes: []registry.Node{
				{ID: "node-a", OS: "linux", Arch: "arm64", Roles: []string{"server"}, Enabled: nodeAEnabled},
				{ID: "node-b", OS: "windows", Arch: "amd64", Roles: []string{"server"}, Enabled: true},
			},
			Routes: registry.Routes{Capabilities: map[string]registry.CapabilityRoute{
				"demo": {Executors: []string{"node-a"}, DefaultExecutor: "node-a"},
			}},
		},
	})
	if err != nil {
		t.Fatalf("load replacement nodes: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %+v", warnings)
	}
	f.reg.Nodes = alt.Nodes
}

// waitRunForgotten waits until a run leaves the hub's map.
func waitRunForgotten(t *testing.T, f *fixture, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.hub.mu.Lock()
		_, active := f.hub.runs[id]
		f.hub.mu.Unlock()
		if !active {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %q is still in the hub", id)
		}
		time.Sleep(time.Millisecond)
	}
}

// dial opens one connection. A rejection returns the response with the
// error so the test can check its status.
func (f *fixture) dial(t *testing.T, credential string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	dialer := *websocket.DefaultDialer
	dialer.Subprotocols = []string{agentproto.SubprotocolV1}
	header := http.Header{"Authorization": []string{"Bearer " + credential}}
	ws, resp, err := dialer.Dial(f.wsURL, header)
	if err == nil {
		t.Cleanup(func() { _ = ws.Close() })
	}
	return ws, resp, err
}

// connect opens a connection and completes the hello exchange.
func (f *fixture) connect(t *testing.T, credential string, hello *agentproto.Hello) (*websocket.Conn, *agentproto.Welcome) {
	t.Helper()
	ws, resp, err := f.dial(t, credential)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial: %v (status %d)", err, status)
	}
	writeMessage(t, ws, hello)
	return ws, readWelcome(t, ws)
}

// hello returns a hello that matches node-a.
func hello(node string) *agentproto.Hello {
	switch node {
	case "node-b":
		return &agentproto.Hello{
			Type: agentproto.TypeHello, Protocol: agentproto.ProtocolV1, AgentVersion: "1.0.0",
			OS: "windows", Arch: "amd64", Contracts: []string{"hanse.handler/v1"},
			MaxConcurrentRuns: 4, Runs: []agentproto.RunState{},
		}
	default:
		return &agentproto.Hello{
			Type: agentproto.TypeHello, Protocol: agentproto.ProtocolV1, AgentVersion: "1.0.0",
			OS: "linux", Arch: "arm64", Contracts: []string{"hanse.handler/v1"},
			MaxConcurrentRuns: 4, Runs: []agentproto.RunState{},
		}
	}
}

// writeMessage sends one encoded message.
func writeMessage(t *testing.T, ws *websocket.Conn, msg agentproto.Message) {
	t.Helper()
	data, err := agentproto.Encode(msg)
	if err != nil {
		t.Fatalf("encode %q: %v", msg.MessageType(), err)
	}
	_ = ws.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if err := ws.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write %q: %v", msg.MessageType(), err)
	}
}

// readMessage reads one message, failing on a close or a timeout.
func readMessage(t *testing.T, ws *websocket.Conn) agentproto.Message {
	t.Helper()
	msg, err := readMessageMaybe(t, ws)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return msg
}

// readMessageMaybe reads one message and returns any error.
func readMessageMaybe(t *testing.T, ws *websocket.Conn) (agentproto.Message, error) {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, data, err := ws.ReadMessage()
	if err != nil {
		return nil, err
	}
	return agentproto.Decode(data)
}

// readWelcome reads the welcome message.
func readWelcome(t *testing.T, ws *websocket.Conn) *agentproto.Welcome {
	t.Helper()
	msg := readMessage(t, ws)
	welcome, ok := msg.(*agentproto.Welcome)
	if !ok {
		t.Fatalf("first message is %T, want welcome", msg)
	}
	return welcome
}

// readClose reads until the connection closes and returns its close code.
func readClose(t *testing.T, ws *websocket.Conn) int {
	t.Helper()
	return readCloseNoFail(ws)
}

// readCloseAsync reads a closing connection in the background, so the
// fake agent reads while the Gateway closes, as a real agent does.
func readCloseAsync(ws *websocket.Conn) <-chan int {
	code := make(chan int, 1)
	go func() { code <- readCloseNoFail(ws) }()
	return code
}

// readCloseNoFail reads until the connection closes and returns its close
// code without failing the test, so it can run in a goroutine.
func readCloseNoFail(ws *websocket.Conn) int {
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		if _, _, err := ws.ReadMessage(); err != nil {
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) {
				return closeErr.Code
			}
			return -1
		}
	}
}

// waitStatus waits for a node to reach a status.
func (f *fixture) waitStatus(t *testing.T, node string, want registry.NodeStatus) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		status, ok := f.reg.Nodes.Status(node)
		if ok && status == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("node %s status %q, want %q", node, status, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// testCall is one valid run the hub can send.
func testCall(runID string) *Call {
	return &Call{
		RunID:      runID,
		Node:       "node-a",
		Capability: "demo",
		Operation:  "run",
		Bundle:     "sha256:" + strings.Repeat("a", 64),
		Entry:      []string{"sh", "handler.sh"},
		Request:    json.RawMessage(`{"contract":"hanse.handler/v1","kind":"operation","id":"` + runID + `"}`),
	}
}

// resultMessage builds a successful result for a run.
func resultMessage(runID string, seq int64) *agentproto.Result {
	exit := 0
	return &agentproto.Result{
		Type:       agentproto.TypeResult,
		RunID:      runID,
		Seq:        seq,
		TS:         agentproto.Timestamp(time.Now()),
		ExitCode:   &exit,
		Response:   json.RawMessage(`{"status":"ok","result":{"counted":3},"summary":"done"}`),
		DurationMS: 5,
	}
}

// TestConnectAndPresence covers the hello exchange and the presence it sets.
func TestConnectAndPresence(t *testing.T) {
	f := newFixture(t, nil)
	credential := f.credential(t, "node-a")
	ws, welcome := f.connect(t, credential, hello("node-a"))
	if welcome.Node != "node-a" {
		t.Errorf("welcome node %q, want node-a", welcome.Node)
	}
	if welcome.GatewayVersion != "1.0.0" {
		t.Errorf("welcome gateway version %q, want 1.0.0", welcome.GatewayVersion)
	}
	if welcome.HeartbeatInterval < 1 || welcome.HeartbeatTimeout < 1 {
		t.Errorf("welcome heartbeat %d/%d, want positive", welcome.HeartbeatInterval, welcome.HeartbeatTimeout)
	}
	view, ok := f.reg.Nodes.Get("node-a")
	if !ok {
		t.Fatal("node-a is not in the registry")
	}
	if view.Status != registry.StatusOnline {
		t.Errorf("node-a status %q, want online", view.Status)
	}
	if view.AgentVersion != "1.0.0" {
		t.Errorf("node-a agent version %q, want 1.0.0", view.AgentVersion)
	}
	if !f.hub.Online("node-a") {
		t.Error("the hub reports node-a offline while it is connected")
	}
	_ = ws.Close()
	f.waitStatus(t, "node-a", registry.StatusOffline)
}

// TestRejectBadCredential answers 401 before the upgrade.
func TestRejectBadCredential(t *testing.T) {
	f := newFixture(t, nil)
	_, resp, err := f.dial(t, "hnc_not-a-credential")
	if err == nil {
		t.Fatal("a wrong credential opened a connection")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %v, want 401", resp)
	}
}

// TestRejectUnsupportedProtocol closes with 4003.
func TestRejectUnsupportedProtocol(t *testing.T) {
	f := newFixture(t, nil)
	credential := f.credential(t, "node-a")
	ws, _, err := f.dial(t, credential)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	bad := hello("node-a")
	bad.Protocol = "hanse.agent/v2"
	writeMessage(t, ws, bad)
	if code := readClose(t, ws); code != closeUnsupportedProtocol {
		t.Errorf("close code %d, want %d", code, closeUnsupportedProtocol)
	}
}

// TestRejectNodeMismatch closes with 4004 when the OS or architecture
// differs from nodes.yaml.
func TestRejectNodeMismatch(t *testing.T) {
	f := newFixture(t, nil)
	credential := f.credential(t, "node-a")
	ws, _, err := f.dial(t, credential)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	bad := hello("node-a")
	bad.OS, bad.Arch = "windows", "amd64"
	writeMessage(t, ws, bad)
	if code := readClose(t, ws); code != closeNodeMismatch {
		t.Errorf("close code %d, want %d", code, closeNodeMismatch)
	}
	if status, _ := f.reg.Nodes.Status("node-a"); status != registry.StatusOffline {
		t.Errorf("node-a status %q, want offline", status)
	}
}

// TestCredentialCannotActAsAnotherNode proves that the node ID comes from
// the credential: a credential for node-b cannot present node-a's OS and
// architecture.
func TestCredentialCannotActAsAnotherNode(t *testing.T) {
	f := newFixture(t, nil)
	credentialB := f.credential(t, "node-b")
	ws, _, err := f.dial(t, credentialB)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	claimA := hello("node-a") // linux/arm64, the node-b entry says windows/amd64
	writeMessage(t, ws, claimA)
	if code := readClose(t, ws); code != closeNodeMismatch {
		t.Fatalf("close code %d, want %d", code, closeNodeMismatch)
	}

	// The same credential with its own node's values is accepted, and the
	// Gateway names node-b in welcome even though the agent never sends a
	// node ID.
	ws2, welcome := f.connect(t, credentialB, hello("node-b"))
	if welcome.Node != "node-b" {
		t.Errorf("welcome node %q, want node-b", welcome.Node)
	}
	_ = ws2
}

// TestReplaceConnection closes the older connection with 4002.
func TestReplaceConnection(t *testing.T) {
	f := newFixture(t, nil)
	credential := f.credential(t, "node-a")
	first, _ := f.connect(t, credential, hello("node-a"))
	closed := readCloseAsync(first)
	second, _ := f.connect(t, credential, hello("node-a"))
	if code := <-closed; code != closeReplaced {
		t.Errorf("first connection close code %d, want %d", code, closeReplaced)
	}
	if !f.hub.Online("node-a") {
		t.Error("the node went offline after a replacement")
	}
	_ = second.Close()
	f.waitStatus(t, "node-a", registry.StatusOffline)
}

// TestHeartbeatTimeout closes a silent connection.
func TestHeartbeatTimeout(t *testing.T) {
	f := newFixture(t, func(cfg *Config) {
		cfg.HeartbeatInterval = 20 * time.Millisecond
		cfg.HeartbeatTimeout = 60 * time.Millisecond
	})
	credential := f.credential(t, "node-a")
	ws, _ := f.connect(t, credential, hello("node-a"))
	if code := readClose(t, ws); code != websocket.CloseNormalClosure {
		t.Errorf("close code %d, want %d", code, websocket.CloseNormalClosure)
	}
	f.waitStatus(t, "node-a", registry.StatusOffline)
}

// TestRevokeClosesConnection proves that enroll's Disconnecter closes the
// live connection when the credential is revoked.
func TestRevokeClosesConnection(t *testing.T) {
	f := newFixture(t, nil)
	credential := f.credential(t, "node-a")
	ws, _ := f.connect(t, credential, hello("node-a"))
	closed := readCloseAsync(ws)
	if err := f.service.Revoke("node-a"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if code := <-closed; code != closeRevoked {
		t.Errorf("close code %d, want %d", code, closeRevoked)
	}
	f.waitStatus(t, "node-a", registry.StatusOffline)
}

// TestReconnectResumesRun drops the connection during a run, reconnects,
// and lets the run finish with the messages above the acknowledged
// sequence.
func TestReconnectResumesRun(t *testing.T) {
	f := newFixture(t, nil)
	credential := f.credential(t, "node-a")
	ws, _ := f.connect(t, credential, hello("node-a"))

	done := make(chan dispatchResult, 1)
	go func() {
		response, err := f.hub.Execute(context.Background(), testCall("op_resume"))
		done <- dispatchResult{response: response, err: err}
	}()
	run := readMessage(t, ws)
	if _, ok := run.(*agentproto.Run); !ok {
		t.Fatalf("first message is %T, want run", run)
	}
	_ = ws.Close()
	f.waitStatus(t, "node-a", registry.StatusOffline)

	reconnect := hello("node-a")
	reconnect.Runs = []agentproto.RunState{{RunID: "op_resume", State: agentproto.RunStateRunning, LastSeq: 4}}
	ws2, welcome := f.connect(t, credential, reconnect)
	if len(welcome.Runs) != 1 {
		t.Fatalf("welcome runs %+v, want one resume", welcome.Runs)
	}
	if welcome.Runs[0].Action != agentproto.ActionResume {
		t.Fatalf("action %q, want resume", welcome.Runs[0].Action)
	}
	if welcome.Runs[0].AckedSeq == nil || *welcome.Runs[0].AckedSeq != 0 {
		t.Errorf("acked_seq %v, want 0", welcome.Runs[0].AckedSeq)
	}
	writeMessage(t, ws2, &agentproto.Log{
		Type: agentproto.TypeLog, RunID: "op_resume", Seq: 1,
		TS: agentproto.Timestamp(time.Now()), Stream: agentproto.StreamStderr, Line: "one",
	})
	if ack := readMessage(t, ws2); ack.(*agentproto.Ack).Seq != 1 {
		t.Errorf("first ack %+v, want seq 1", ack)
	}
	writeMessage(t, ws2, resultMessage("op_resume", 2))
	if ack := readMessage(t, ws2); ack.(*agentproto.Ack).Seq != 2 {
		t.Errorf("result ack %+v, want seq 2", ack)
	}
	result := <-done
	if result.err != nil {
		t.Fatalf("execute: %v", result.err)
	}
	if result.response.Status != runtime.StatusOK {
		t.Errorf("response status %q, want ok", result.response.Status)
	}
	if len(f.logs.find("one")) == 0 {
		t.Error("the log line from the reconnect was not stored")
	}
}

// TestCancelWhileDisconnectedAbandons drops the connection, cancels the
// run, and answers abandon on the reconnect.
func TestCancelWhileDisconnectedAbandons(t *testing.T) {
	f := newFixture(t, nil)
	credential := f.credential(t, "node-a")
	ws, _ := f.connect(t, credential, hello("node-a"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan dispatchResult, 1)
	go func() {
		response, err := f.hub.Execute(ctx, testCall("op_cancel"))
		done <- dispatchResult{response: response, err: err}
	}()
	run := readMessage(t, ws)
	if _, ok := run.(*agentproto.Run); !ok {
		t.Fatalf("first message is %T, want run", run)
	}
	_ = ws.Close()
	f.waitStatus(t, "node-a", registry.StatusOffline)
	cancel()
	result := <-done
	if !errors.Is(result.err, context.Canceled) {
		t.Fatalf("execute error %v, want context.Canceled", result.err)
	}

	reconnect := hello("node-a")
	reconnect.Runs = []agentproto.RunState{{RunID: "op_cancel", State: agentproto.RunStateRunning, LastSeq: 0}}
	_, welcome := f.connect(t, credential, reconnect)
	if len(welcome.Runs) != 1 || welcome.Runs[0].Action != agentproto.ActionAbandon {
		t.Fatalf("welcome runs %+v, want abandon", welcome.Runs)
	}
	waitRunForgotten(t, f, "op_cancel")
}

// TestReconnectResendsMissingRun sends a run again when hello does not
// list it, because the agent never accepted it.
func TestReconnectResendsMissingRun(t *testing.T) {
	f := newFixture(t, nil)
	credential := f.credential(t, "node-a")
	ws, _ := f.connect(t, credential, hello("node-a"))

	done := make(chan dispatchResult, 1)
	go func() {
		response, err := f.hub.Execute(context.Background(), testCall("op_resend"))
		done <- dispatchResult{response: response, err: err}
	}()
	first := readMessage(t, ws)
	original, ok := first.(*agentproto.Run)
	if !ok {
		t.Fatalf("first message is %T, want run", first)
	}
	_ = ws.Close()
	f.waitStatus(t, "node-a", registry.StatusOffline)

	ws2, welcome := f.connect(t, credential, hello("node-a"))
	if len(welcome.Runs) != 0 {
		t.Fatalf("welcome runs %+v, want none", welcome.Runs)
	}
	resent := readMessage(t, ws2)
	again, ok := resent.(*agentproto.Run)
	if !ok {
		t.Fatalf("second message is %T, want the run again", resent)
	}
	if again.RunID != original.RunID {
		t.Errorf("resent run %q, want %q", again.RunID, original.RunID)
	}
	writeMessage(t, ws2, resultMessage("op_resend", 1))
	_ = readMessage(t, ws2) // ack
	result := <-done
	if result.err != nil {
		t.Fatalf("execute: %v", result.err)
	}
	if result.response.Status != runtime.StatusOK {
		t.Errorf("response status %q, want ok", result.response.Status)
	}
}

// TestReconnectWindowExpires fails a run that waits too long, and answers
// abandon when the agent returns afterwards.
func TestReconnectWindowExpires(t *testing.T) {
	f := newFixture(t, func(cfg *Config) {
		cfg.ReconnectWindow = 80 * time.Millisecond
	})
	credential := f.credential(t, "node-a")
	ws, _ := f.connect(t, credential, hello("node-a"))

	done := make(chan dispatchResult, 1)
	go func() {
		response, err := f.hub.Execute(context.Background(), testCall("op_lost"))
		done <- dispatchResult{response: response, err: err}
	}()
	run := readMessage(t, ws)
	if _, ok := run.(*agentproto.Run); !ok {
		t.Fatalf("first message is %T, want run", run)
	}
	_ = ws.Close()

	select {
	case result := <-done:
		if !errors.Is(result.err, herrors.ExecutorConnectionLost) {
			t.Fatalf("execute error %v, want executor_connection_lost", result.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the run did not fail after the reconnect window")
	}

	reconnect := hello("node-a")
	reconnect.Runs = []agentproto.RunState{{RunID: "op_lost", State: agentproto.RunStateRunning, LastSeq: 0}}
	_, welcome := f.connect(t, credential, reconnect)
	if len(welcome.Runs) != 1 || welcome.Runs[0].Action != agentproto.ActionAbandon {
		t.Fatalf("welcome runs %+v, want abandon", welcome.Runs)
	}
	if len(f.logs.find("executor_connection_lost")) == 0 {
		t.Error("the failure has no system log line")
	}
}

// TestUnknownMessageKeepsConnection answers unknown_message and keeps the
// connection open (docs/agent-protocol.md section 10).
func TestUnknownMessageKeepsConnection(t *testing.T) {
	f := newFixture(t, nil)
	credential := f.credential(t, "node-a")
	ws, _ := f.connect(t, credential, hello("node-a"))
	_ = ws.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if err := ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"progress"}`)); err != nil {
		t.Fatalf("write unknown message: %v", err)
	}
	msg := readMessage(t, ws)
	protocolError, ok := msg.(*agentproto.Error)
	if !ok || protocolError.Code != "unknown_message" {
		t.Fatalf("message %+v, want unknown_message error", msg)
	}

	// The connection still works: a run starts and finishes on it.
	done := make(chan dispatchResult, 1)
	go func() {
		response, err := f.hub.Execute(context.Background(), testCall("op_after_unknown"))
		done <- dispatchResult{response: response, err: err}
	}()
	if run := readMessage(t, ws); run.MessageType() != agentproto.TypeRun {
		t.Fatalf("next message is %q, want run", run.MessageType())
	}
	writeMessage(t, ws, resultMessage("op_after_unknown", 1))
	_ = readMessage(t, ws) // ack
	if result := <-done; result.err != nil {
		t.Fatalf("execute after unknown message: %v", result.err)
	}
}

// TestRevokedCredentialClosesConnection proves that a revoke made in
// another process reaches the live connection: the hub re-checks the
// credential on the heartbeat tick, because the operator command's
// Disconnecter cannot see this process.
func TestRevokedCredentialClosesConnection(t *testing.T) {
	f := newFixture(t, func(cfg *Config) {
		cfg.HeartbeatInterval = 20 * time.Millisecond
		cfg.HeartbeatTimeout = 5 * time.Second
	})
	credential := f.credential(t, "node-a")
	ws, _ := f.connect(t, credential, hello("node-a"))

	// A second store handle with no Disconnecter is the separate
	// `hanse-gateway node revoke` process.
	other, err := enroll.Open(enroll.Options{Path: f.service.Path()})
	if err != nil {
		t.Fatalf("open second enrollment store: %v", err)
	}
	t.Cleanup(func() { _ = other.Close() })
	if err := other.Revoke("node-a"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if code := readClose(t, ws); code != closeRevoked {
		t.Errorf("close code %d, want %d", code, closeRevoked)
	}
	f.waitStatus(t, "node-a", registry.StatusOffline)
}

// TestVerifyStoreFailureDoesNotLookRevoked keeps a transient enroll
// store failure recoverable: the agent gets 4000 and reconnects with
// backoff, not 4001 and an hour of silence.
func TestVerifyStoreFailureDoesNotLookRevoked(t *testing.T) {
	var failing atomic.Bool
	f := newFixture(t, func(cfg *Config) {
		cfg.HeartbeatInterval = 20 * time.Millisecond
		cfg.HeartbeatTimeout = 5 * time.Second
		base := cfg.Verify
		cfg.Verify = func(credential string) (string, error) {
			if failing.Load() {
				return "", herrors.New(herrors.Internal, "the enrollment store is unavailable", nil)
			}
			return base(credential)
		}
	})
	credential := f.credential(t, "node-a")
	ws, _ := f.connect(t, credential, hello("node-a"))
	failing.Store(true)
	if code := readClose(t, ws); code != closeProtocolError {
		t.Errorf("close code %d, want %d", code, closeProtocolError)
	}
}

// TestDisabledNodeClosesConnection proves that the re-check closes a
// connection whose node entry became disabled.
func TestDisabledNodeClosesConnection(t *testing.T) {
	ticks := make(chan time.Time, 4)
	f := newFixture(t, func(cfg *Config) {
		cfg.HeartbeatInterval = time.Hour
		cfg.HeartbeatTimeout = time.Hour
		cfg.After = func(time.Duration) <-chan time.Time { return ticks }
	})
	credential := f.credential(t, "node-a")
	ws, _ := f.connect(t, credential, hello("node-a"))
	f.replaceNodes(t, false)
	ticks <- time.Now()
	if code := readClose(t, ws); code != closeNodeDisabled {
		t.Errorf("close code %d, want %d", code, closeNodeDisabled)
	}
	f.waitStatus(t, "node-a", registry.StatusOffline)
}

// TestNodeRemovedClosesConnection requires a configuration reload that
// removed the node to close its agent connection with the node-mismatch
// close code, because the node is no longer in nodes.yaml.
func TestNodeRemovedClosesConnection(t *testing.T) {
	f := newFixture(t, nil)
	credential := f.credential(t, "node-a")
	ws, _ := f.connect(t, credential, hello("node-a"))
	closed := readCloseAsync(ws)
	f.hub.NodeRemoved("node-a")
	if code := <-closed; code != closeNodeMismatch {
		t.Errorf("close code %d, want %d", code, closeNodeMismatch)
	}
	f.waitStatus(t, "node-a", registry.StatusOffline)
}

// TestCancelledRunIsForgottenAfterResult drops a cancelled run when the
// agent sends the result it still owed.
func TestCancelledRunIsForgottenAfterResult(t *testing.T) {
	f := newFixture(t, nil)
	credential := f.credential(t, "node-a")
	ws, _ := f.connect(t, credential, hello("node-a"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan dispatchResult, 1)
	go func() {
		response, err := f.hub.Execute(ctx, testCall("op_cancel_result"))
		done <- dispatchResult{response: response, err: err}
	}()
	if run := readMessage(t, ws); run.MessageType() != agentproto.TypeRun {
		t.Fatalf("first message is %q, want run", run.MessageType())
	}
	cancel()
	if msg := readMessage(t, ws); msg.MessageType() != agentproto.TypeCancel {
		t.Fatalf("next message is %q, want cancel", msg.MessageType())
	}
	if result := <-done; !errors.Is(result.err, context.Canceled) {
		t.Fatalf("execute error %v, want context.Canceled", result.err)
	}
	writeMessage(t, ws, resultMessage("op_cancel_result", 1))
	if ack := readMessage(t, ws); ack.MessageType() != agentproto.TypeAck {
		t.Fatalf("next message is %q, want ack", ack.MessageType())
	}
	waitRunForgotten(t, f, "op_cancel_result")
}

// TestCancelledRunIsForgottenAfterWindow drops a cancelled run whose agent
// never answers once the reconnect window passes.
func TestCancelledRunIsForgottenAfterWindow(t *testing.T) {
	f := newFixture(t, func(cfg *Config) {
		cfg.ReconnectWindow = 50 * time.Millisecond
	})
	credential := f.credential(t, "node-a")
	ws, _ := f.connect(t, credential, hello("node-a"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan dispatchResult, 1)
	go func() {
		response, err := f.hub.Execute(ctx, testCall("op_cancel_window"))
		done <- dispatchResult{response: response, err: err}
	}()
	if run := readMessage(t, ws); run.MessageType() != agentproto.TypeRun {
		t.Fatalf("first message is %q, want run", run.MessageType())
	}
	cancel()
	if msg := readMessage(t, ws); msg.MessageType() != agentproto.TypeCancel {
		t.Fatalf("next message is %q, want cancel", msg.MessageType())
	}
	<-done
	_ = ws.Close()
	waitRunForgotten(t, f, "op_cancel_window")
}

// TestReconnectWindowUsesInjectedClock proves that the reconnect timer
// reads the hub's injected clock, so tests need no real five-minute wait.
func TestReconnectWindowUsesInjectedClock(t *testing.T) {
	ticks := make(chan time.Time, 4)
	f := newFixture(t, func(cfg *Config) {
		cfg.ReconnectWindow = time.Hour
		cfg.After = func(time.Duration) <-chan time.Time { return ticks }
	})
	credential := f.credential(t, "node-a")
	ws, _ := f.connect(t, credential, hello("node-a"))

	done := make(chan dispatchResult, 1)
	go func() {
		response, err := f.hub.Execute(context.Background(), testCall("op_clock"))
		done <- dispatchResult{response: response, err: err}
	}()
	if msg := readMessage(t, ws); msg.MessageType() != agentproto.TypeRun {
		t.Fatalf("first message is %q, want run", msg.MessageType())
	}
	_ = ws.Close()
	f.waitStatus(t, "node-a", registry.StatusOffline)

	select {
	case <-done:
		t.Fatal("the run ended before the injected clock fired")
	default:
	}
	ticks <- time.Now()
	select {
	case result := <-done:
		if !errors.Is(result.err, herrors.ExecutorConnectionLost) {
			t.Fatalf("execute error %v, want executor_connection_lost", result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the injected clock did not end the reconnect wait")
	}
}

// dispatchResult is one Execute outcome.
type dispatchResult struct {
	response runtime.Response
	err      error
}

// TestPurgelessRun is a guard for the stale-run bookkeeping: a run that
// finished is gone from the hub.
func TestFinishedRunIsForgotten(t *testing.T) {
	f := newFixture(t, nil)
	credential := f.credential(t, "node-a")
	ws, _ := f.connect(t, credential, hello("node-a"))
	done := make(chan dispatchResult, 1)
	go func() {
		response, err := f.hub.Execute(context.Background(), testCall("op_done"))
		done <- dispatchResult{response: response, err: err}
	}()
	if msg := readMessage(t, ws); msg.MessageType() != agentproto.TypeRun {
		t.Fatalf("first message is %q, want run", msg.MessageType())
	}
	writeMessage(t, ws, resultMessage("op_done", 1))
	_ = readMessage(t, ws)
	if result := <-done; result.err != nil {
		t.Fatalf("execute: %v", result.err)
	}
	waitRunForgotten(t, f, "op_done")
}

// TestOversizedResultReportsHandlerOutputTooLarge covers a node agent that
// reports stdout over the limit: the Gateway maps the result to the handler
// contract's handler_output_too_large with the run's limit, never to
// handler_output_invalid (docs/handler-contract.md section 5).
func TestOversizedResultReportsHandlerOutputTooLarge(t *testing.T) {
	f := newFixture(t, nil)
	credential := f.credential(t, "node-a")
	ws, _ := f.connect(t, credential, hello("node-a"))
	call := testCall("op_flood")
	call.ResponseBytes = 1024
	done := make(chan dispatchResult, 1)
	go func() {
		response, err := f.hub.Execute(context.Background(), call)
		done <- dispatchResult{response: response, err: err}
	}()
	msg := readMessage(t, ws)
	run, ok := msg.(*agentproto.Run)
	if !ok {
		t.Fatalf("message %T, want run", msg)
	}
	if run.Limits.ResponseBytes != 1024 {
		t.Errorf("run response limit %d, want 1024", run.Limits.ResponseBytes)
	}
	stopReason, outputError := agentproto.StopOutputTooLarge, agentproto.OutputTooLarge
	writeMessage(t, ws, &agentproto.Result{
		Type:        agentproto.TypeResult,
		RunID:       "op_flood",
		Seq:         1,
		TS:          agentproto.Timestamp(time.Now()),
		StopReason:  &stopReason,
		OutputError: &outputError,
		DurationMS:  5,
	})
	_ = readMessage(t, ws)
	result := <-done
	if !errors.Is(result.err, herrors.HandlerOutputTooLarge) {
		t.Fatalf("execute error %v, want handler_output_too_large", result.err)
	}
	var apiErr *herrors.Error
	if !errors.As(result.err, &apiErr) {
		t.Fatalf("execute error is %T, want *herrors.Error", result.err)
	}
	if got := apiErr.Details["limit"]; got != int64(1024) {
		t.Errorf("limit detail %v, want 1024", got)
	}
	if code := herrors.ExitCode(result.err); code != 49 {
		t.Errorf("exit code %d, want 49", code)
	}
}

// TestRejectedRunFails covers the rejected message mapping.
func TestRejectedRunFails(t *testing.T) {
	f := newFixture(t, nil)
	credential := f.credential(t, "node-a")
	ws, _ := f.connect(t, credential, hello("node-a"))
	done := make(chan dispatchResult, 1)
	go func() {
		response, err := f.hub.Execute(context.Background(), testCall("op_rejected"))
		done <- dispatchResult{response: response, err: err}
	}()
	if msg := readMessage(t, ws); msg.MessageType() != agentproto.TypeRun {
		t.Fatalf("first message is %q, want run", msg.MessageType())
	}
	writeMessage(t, ws, &agentproto.Rejected{
		Type: agentproto.TypeRejected, RunID: "op_rejected", Code: "agent_busy", Message: "busy",
	})
	result := <-done
	if !errors.Is(result.err, herrors.AgentBusy) {
		t.Fatalf("execute error %v, want agent_busy", result.err)
	}
}

// TestExecuteWithoutConnection reports ExecutorUnavailable.
func TestExecuteWithoutConnection(t *testing.T) {
	f := newFixture(t, nil)
	_, err := f.hub.Execute(context.Background(), testCall("op_offline"))
	if !errors.Is(err, herrors.ExecutorUnavailable) {
		t.Fatalf("execute error %v, want executor_unavailable", err)
	}
}

// TestRunMessageEnvelope checks the run message fields the hub builds.
func TestRunMessageEnvelope(t *testing.T) {
	f := newFixture(t, nil)
	credential := f.credential(t, "node-a")
	ws, _ := f.connect(t, credential, hello("node-a"))
	call := testCall("op_fields")
	call.GracePeriod = 3 * time.Second
	done := make(chan dispatchResult, 1)
	go func() {
		response, err := f.hub.Execute(context.Background(), call)
		done <- dispatchResult{response: response, err: err}
	}()
	msg := readMessage(t, ws)
	run, ok := msg.(*agentproto.Run)
	if !ok {
		t.Fatalf("message %T, want run", msg)
	}
	if run.RunID != "op_fields" || run.Bundle != call.Bundle {
		t.Errorf("run %+v, want run_id and bundle of the call", run)
	}
	if run.GracePeriod != 3000 {
		t.Errorf("grace_period_ms %d, want 3000", run.GracePeriod)
	}
	if len(run.Entry) != 2 || run.Entry[0] != "sh" {
		t.Errorf("entry %v, want [sh handler.sh]", run.Entry)
	}
	if string(run.Request) != string(call.Request) {
		t.Errorf("request %s, want %s", run.Request, call.Request)
	}
	writeMessage(t, ws, resultMessage("op_fields", 1))
	_ = readMessage(t, ws)
	<-done
}

// TestCallWithoutEntry reports ExecutorNotEligible.
func TestCallWithoutEntry(t *testing.T) {
	f := newFixture(t, nil)
	credential := f.credential(t, "node-a")
	f.connect(t, credential, hello("node-a"))
	call := testCall("op_no_entry")
	call.Entry = nil
	_, err := f.hub.Execute(context.Background(), call)
	if !errors.Is(err, herrors.ExecutorNotEligible) {
		t.Fatalf("execute error %v, want executor_not_eligible", err)
	}
}

// TestNewRequiresParts rejects an incomplete hub.
func TestNewRequiresParts(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("New accepted a config without a registry")
	}
	if _, err := New(Config{Registry: &registry.Registry{}}); err == nil {
		t.Error("New accepted a config without a credential check")
	}
}

// TestTrimLine keeps the byte limit and a complete UTF-8 prefix.
func TestTrimLine(t *testing.T) {
	got := trimLine("héllo", 2)
	if got != "h" {
		t.Errorf("trimLine %q, want h", got)
	}
	if trimLine("ok", 10) != "ok" {
		t.Error("trimLine shortened a short line")
	}
}

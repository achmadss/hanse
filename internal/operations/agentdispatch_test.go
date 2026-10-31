package operations

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/achmadss/hanse/internal/agentconn"
	"github.com/achmadss/hanse/internal/agentproto"
	"github.com/achmadss/hanse/internal/config"
	"github.com/achmadss/hanse/internal/enroll"
	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/runtime"
)

// dispatchManifest is the fixture capability of the agent dispatcher
// tests: one ordinary operation and one privileged operation.
const dispatchManifest = `version: 1
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
  admin:
    description: Run the demonstration handler with administrator rights
    mode: sync
    privileged: true
    arguments:
      type: object
`

// dispatchFixture is one dispatcher with its hub, registry, and fake agent
// connection.
type dispatchFixture struct {
	dispatcher *AgentDispatcher
	hub        *agentconn.Hub
	reg        *registry.Registry
	service    *enroll.Service
	server     *httptest.Server
	wsURL      string
}

// newDispatchFixture builds the dispatcher and its hub.
func newDispatchFixture(t *testing.T) *dispatchFixture {
	t.Helper()
	capsDir := filepath.Join(t.TempDir(), "capabilities")
	demoDir := filepath.Join(capsDir, "demo")
	if err := os.MkdirAll(demoDir, 0o755); err != nil {
		t.Fatalf("create capability directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(demoDir, "manifest.yaml"), []byte(dispatchManifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(demoDir, "handler.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatalf("write handler: %v", err)
	}
	reg, warnings, err := registry.Load(registry.Options{
		Dirs: []string{capsDir},
		Input: registry.Input{
			Nodes: []registry.Node{
				{ID: "node-a", OS: "linux", Arch: "arm64", Enabled: true},
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
	var service *enroll.Service
	hub, err := agentconn.New(agentconn.Config{
		Registry:       reg,
		GatewayVersion: "1.0.0",
		Verify: func(credential string) (string, error) {
			if service == nil {
				return "", herrors.New(herrors.AuthenticationFailed, "", nil)
			}
			return service.Verify(credential)
		},
	})
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
	return &dispatchFixture{
		dispatcher: NewAgentDispatcher(hub, reg, config.DefaultTimeouts()),
		hub:        hub,
		reg:        reg,
		service:    service,
		server:     server,
		wsURL:      "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/agent/connect",
	}
}

// connect enrolls node-a, opens a connection, and completes the hello
// exchange.
func (f *dispatchFixture) connect(t *testing.T) *websocket.Conn {
	t.Helper()
	token, err := f.service.IssueJoinToken("node-a")
	if err != nil {
		t.Fatalf("issue join token: %v", err)
	}
	outcome, err := f.service.Join(token.Plain, nil)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	dialer := *websocket.DefaultDialer
	dialer.Subprotocols = []string{agentproto.SubprotocolV1}
	header := http.Header{"Authorization": []string{"Bearer " + outcome.Credential}}
	ws, _, err := dialer.Dial(f.wsURL, header)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	writeDispatchMessage(t, ws, &agentproto.Hello{
		Type: agentproto.TypeHello, Protocol: agentproto.ProtocolV1, AgentVersion: "1.0.0",
		OS: "linux", Arch: "arm64", Contracts: []string{"hanse.handler/v1"},
		MaxConcurrentRuns: 2,
	})
	if msg := readDispatchMessage(t, ws); msg.MessageType() != agentproto.TypeWelcome {
		t.Fatalf("first message is %q, want welcome", msg.MessageType())
	}
	return ws
}

// request builds one validated request envelope for demo.run.
func dispatchRequest(id string) *runtime.Request {
	return &runtime.Request{
		Contract:   runtime.ContractV1,
		Kind:       runtime.KindOperation,
		ID:         id,
		Capability: "demo",
		Operation:  "run",
		Arguments:  json.RawMessage(`{}`),
		Nodes:      map[string]runtime.Node{},
		Executor: runtime.Node{
			ID: "node-a", Name: "Home server", OS: "linux", Arch: "arm64", Address: "192.0.2.10",
			Attributes: map[string]any{"role": "server"},
		},
		Deadline: time.Date(2026, 10, 30, 8, 15, 30, 0, time.UTC),
	}
}

// TestAgentDispatcherExecute sends one run and receives its result.
func TestAgentDispatcherExecute(t *testing.T) {
	f := newDispatchFixture(t)
	ws := f.connect(t)
	done := make(chan dispatchOutcome, 1)
	go func() {
		response, err := f.dispatcher.Execute(context.Background(), &Call{Request: dispatchRequest("op_agent")})
		done <- dispatchOutcome{response: response, err: err}
	}()

	msg := readDispatchMessage(t, ws)
	run, ok := msg.(*agentproto.Run)
	if !ok {
		t.Fatalf("message %T, want run", msg)
	}
	capability, _ := f.reg.Capability("demo")
	if run.Bundle != capability.BundleHash {
		t.Errorf("bundle %q, want %q", run.Bundle, capability.BundleHash)
	}
	if len(run.Entry) != 2 || run.Entry[0] != "sh" {
		t.Errorf("entry %v, want [sh handler.sh]", run.Entry)
	}
	if run.Privileged {
		t.Error("demo.run must not be privileged")
	}
	var envelope struct {
		Contract string `json:"contract"`
		ID       string `json:"id"`
		Executor struct {
			ID         string         `json:"id"`
			Address    string         `json:"address"`
			OS         string         `json:"os"`
			Arch       string         `json:"arch"`
			Attributes map[string]any `json:"attributes"`
		} `json:"executor"`
		Deadline string `json:"deadline"`
	}
	if err := json.Unmarshal(run.Request, &envelope); err != nil {
		t.Fatalf("decode request envelope %s: %v", run.Request, err)
	}
	if envelope.Contract != runtime.ContractV1 || envelope.ID != "op_agent" {
		t.Errorf("envelope %+v, want contract and ID", envelope)
	}
	if envelope.Executor.ID != "node-a" || envelope.Executor.Address != "192.0.2.10" {
		t.Errorf("executor %+v, want node-a and its address", envelope.Executor)
	}
	if envelope.Deadline != "2026-10-30T08:15:30.000Z" {
		t.Errorf("deadline %q, want the envelope's UTC deadline", envelope.Deadline)
	}

	exit := 0
	writeDispatchMessage(t, ws, &agentproto.Result{
		Type: agentproto.TypeResult, RunID: "op_agent", Seq: 1,
		TS: agentproto.Timestamp(time.Now()), ExitCode: &exit,
		Response:   json.RawMessage(`{"status":"ok","result":{"counted":3},"summary":"done"}`),
		DurationMS: 5,
	})
	_ = readDispatchMessage(t, ws) // ack

	outcome := <-done
	if outcome.err != nil {
		t.Fatalf("execute: %v", outcome.err)
	}
	if outcome.response.Status != runtime.StatusOK {
		t.Errorf("response status %q, want ok", outcome.response.Status)
	}
	if string(outcome.response.Result) != `{"counted":3}` {
		t.Errorf("result %s, want the handler result", outcome.response.Result)
	}
}

// TestAgentDispatcherPrivileged sends the elevation password in the run
// message only.
func TestAgentDispatcherPrivileged(t *testing.T) {
	f := newDispatchFixture(t)
	ws := f.connect(t)
	request := dispatchRequest("op_admin")
	request.Operation = "admin"
	done := make(chan dispatchOutcome, 1)
	go func() {
		response, err := f.dispatcher.Execute(context.Background(), &Call{
			Request:   request,
			Elevation: &Elevation{Password: "placeholder-password"},
		})
		done <- dispatchOutcome{response: response, err: err}
	}()
	msg := readDispatchMessage(t, ws)
	run, ok := msg.(*agentproto.Run)
	if !ok {
		t.Fatalf("message %T, want run", msg)
	}
	if !run.Privileged || run.Elevation == nil || run.Elevation.Password != "placeholder-password" {
		t.Errorf("run %+v, want privileged with its password", run)
	}
	if strings.Contains(string(run.Request), "placeholder-password") {
		t.Error("the request envelope holds the elevation password")
	}
	exit := 0
	writeDispatchMessage(t, ws, &agentproto.Result{
		Type: agentproto.TypeResult, RunID: "op_admin", Seq: 1,
		TS: agentproto.Timestamp(time.Now()), ExitCode: &exit,
		Response: json.RawMessage(`{"status":"ok","result":{}}`),
	})
	_ = readDispatchMessage(t, ws)
	<-done
}

// TestAgentDispatcherCancel sends a cancel message when the run's context
// is cancelled.
func TestAgentDispatcherCancel(t *testing.T) {
	f := newDispatchFixture(t)
	ws := f.connect(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan dispatchOutcome, 1)
	go func() {
		response, err := f.dispatcher.Execute(ctx, &Call{Request: dispatchRequest("op_cancel")})
		done <- dispatchOutcome{response: response, err: err}
	}()
	if msg := readDispatchMessage(t, ws); msg.MessageType() != agentproto.TypeRun {
		t.Fatalf("first message is %q, want run", msg.MessageType())
	}
	cancel()
	msg := readDispatchMessage(t, ws)
	if _, ok := msg.(*agentproto.Cancel); !ok {
		t.Fatalf("message %T, want cancel", msg)
	}
	outcome := <-done
	if !errors.Is(outcome.err, context.Canceled) {
		t.Fatalf("execute error %v, want context.Canceled", outcome.err)
	}
}

// TestAgentDispatcherOffline reports ExecutorUnavailable without a
// connection.
func TestAgentDispatcherOffline(t *testing.T) {
	f := newDispatchFixture(t)
	_, err := f.dispatcher.Execute(context.Background(), &Call{Request: dispatchRequest("op_offline")})
	if !errors.Is(err, herrors.ExecutorUnavailable) {
		t.Fatalf("execute error %v, want executor_unavailable", err)
	}
}

// TestAgentDispatcherUnknownExecutor reports InvalidExecutor.
func TestAgentDispatcherUnknownExecutor(t *testing.T) {
	f := newDispatchFixture(t)
	request := dispatchRequest("op_unknown")
	request.Executor.ID = "node-z"
	_, err := f.dispatcher.Execute(context.Background(), &Call{Request: request})
	if !errors.Is(err, herrors.InvalidExecutor) {
		t.Fatalf("execute error %v, want invalid_executor", err)
	}
}

// TestAgentDispatcherNonExecCapabilityRefused refuses to send a capability
// that has no exec handler to a node agent.
func TestAgentDispatcherNonExecCapabilityRefused(t *testing.T) {
	f := newDispatchFixture(t)
	capability, _ := f.reg.Capability("demo")
	original := capability.Manifest.Handler.Kind
	capability.Manifest.Handler.Kind = registry.KindHTTP
	defer func() { capability.Manifest.Handler.Kind = original }()
	_, err := f.dispatcher.Execute(context.Background(), &Call{Request: dispatchRequest("op_http")})
	if !errors.Is(err, herrors.Internal) {
		t.Fatalf("execute error %v, want internal", err)
	}
}

// dispatchOutcome is one Execute result.
type dispatchOutcome struct {
	response runtime.Response
	err      error
}

// writeDispatchMessage sends one protocol message.
func writeDispatchMessage(t *testing.T, ws *websocket.Conn, msg agentproto.Message) {
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

// readDispatchMessage reads one protocol message.
func readDispatchMessage(t *testing.T, ws *websocket.Conn) agentproto.Message {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, data, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	msg, err := agentproto.Decode(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return msg
}

// TestAgentDispatcherOptionProviderIsNeverElevated covers the rule that an
// option provider never runs with administrator rights, even for a
// privileged operation (docs/handler-contract.md section 4.5).
func TestAgentDispatcherOptionProviderIsNeverElevated(t *testing.T) {
	f := newDispatchFixture(t)
	ws := f.connect(t)
	request := dispatchRequest("opt_admin")
	request.Operation = "admin"
	request.Kind = runtime.KindOptions
	request.Field = "name"
	done := make(chan dispatchOutcome, 1)
	go func() {
		response, err := f.dispatcher.Execute(context.Background(), &Call{
			Request:   request,
			Elevation: &Elevation{Password: "placeholder-password"},
		})
		done <- dispatchOutcome{response: response, err: err}
	}()
	msg := readDispatchMessage(t, ws)
	run, ok := msg.(*agentproto.Run)
	if !ok {
		t.Fatalf("message %T, want run", msg)
	}
	if run.Privileged {
		t.Error("an option provider run was privileged")
	}
	if run.Elevation != nil {
		t.Error("an option provider run carried an elevation password")
	}
	exit := 0
	writeDispatchMessage(t, ws, &agentproto.Result{
		Type: agentproto.TypeResult, RunID: "opt_admin", Seq: 1,
		TS: agentproto.Timestamp(time.Now()), ExitCode: &exit,
		Response: json.RawMessage(`{"status":"ok","result":{}}`),
	})
	_ = readDispatchMessage(t, ws)
	<-done
}

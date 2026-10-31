package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/achmadss/hanse/internal/agentconn"
	"github.com/achmadss/hanse/internal/api"
	"github.com/achmadss/hanse/internal/config"
	"github.com/achmadss/hanse/internal/enroll"
	"github.com/achmadss/hanse/internal/nodeagent"
	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/runner"
	hanruntime "github.com/achmadss/hanse/internal/runtime"
)

// testElevationPassword is a placeholder password of a privileged run. It
// must never appear in the agent's log.
const testElevationPassword = "placeholder-elevation-password"

// agentLog is a log destination that a test can read while the agent
// writes to it.
type agentLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write appends to the buffer.
func (l *agentLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// String returns the lines so far.
func (l *agentLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// TestAgentLogHoldsNoSecret runs the agent against a Gateway in the same
// process and proves that its log never holds the node credential or the
// elevation password of a privileged run.
//
// The agent logs four things: the Gateway URL it connects to, why a
// connection ended, a Gateway error code with its message, and a journal
// write failure. None of them carries request data: the password is used by
// the elevation check and dropped, and the credential never leaves the
// connection header. This test is the guard for that, so a later change that
// logs a request cannot pass unnoticed.
func TestAgentLogHoldsNoSecret(t *testing.T) {
	const password = testElevationPassword
	stateDir := filepath.Join(t.TempDir(), "state")
	configPath := filepath.Join(t.TempDir(), "agent.yaml")

	// The Gateway's side: one node, the echo capability, the enrollment
	// store, and the connection hub.
	registryInput := registry.Input{
		Nodes: []registry.Node{{
			ID: "node-a", OS: goruntime.GOOS, Arch: goruntime.GOARCH,
			Roles: []string{"server"}, Enabled: true,
		}},
		Routes: registry.Routes{Capabilities: map[string]registry.CapabilityRoute{
			"echo": {Executors: []string{"node-a"}, DefaultExecutor: "node-a"},
		}},
	}
	reg, warnings, err := registry.Load(registry.Options{
		Dirs:  []string{filepath.Join("..", "..", "capabilities")},
		Input: registryInput,
	})
	if err != nil {
		t.Fatalf("load the registry: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings: %+v", warnings)
	}
	var enrollment *enroll.Service
	hub, err := agentconn.New(agentconn.Config{
		Registry:       reg,
		GatewayVersion: "1.0.0",
		Verify: func(credential string) (string, error) {
			if enrollment == nil {
				return "", io.ErrUnexpectedEOF
			}
			return enrollment.Verify(credential)
		},
		HeartbeatInterval: time.Second,
		HeartbeatTimeout:  5 * time.Second,
		ReconnectWindow:   time.Minute,
	})
	if err != nil {
		t.Fatalf("build the hub: %v", err)
	}
	enrollment, err = enroll.Open(enroll.Options{StateDir: t.TempDir(), Disconnect: hub})
	if err != nil {
		t.Fatalf("open the enrollment store: %v", err)
	}
	t.Cleanup(func() { _ = enrollment.Close() })
	server := httptest.NewServer(api.New(reg, nil, enrollment, api.WithAgentHub(hub)))
	t.Cleanup(server.Close)

	// Enroll this node the way an operator does, with the real join
	// command, so the credential is the real thing.
	if err := os.WriteFile(configPath, []byte("version: 1\nstate_dir: "+stateDir+"\n"), 0o600); err != nil {
		t.Fatalf("write agent.yaml: %v", err)
	}
	token, err := enrollment.IssueJoinToken("node-a")
	if err != nil {
		t.Fatalf("issue a join token: %v", err)
	}
	var joinOut, joinErr bytes.Buffer
	if code := runJoin([]string{server.URL, token.Plain, "--state-dir", stateDir,
		"--config", configPath}, &joinOut, &joinErr, stateDir); code != exitOK {
		t.Fatalf("join: exit %d: %s", code, joinErr.String())
	}
	cfg, problems, err := config.LoadAgent(configPath)
	if err != nil {
		t.Fatalf("load agent.yaml: %v", err)
	}
	if len(problems) > 0 {
		t.Fatalf("problems: %v", problems)
	}
	credential, err := readCredential(stateDir)
	if err != nil {
		t.Fatalf("read the credential: %v", err)
	}

	// Run the real agent in this process, with the same logger that
	// `hanse-node run` builds.
	logs := &agentLog{}
	agent, err := nodeagent.New(nodeagent.Config{
		GatewayURL:        cfg.GatewayURL,
		Credential:        credential,
		StateDir:          cfg.StateDir,
		MaxConcurrentRuns: *cfg.MaxConcurrentRuns,
		BufferLimit:       cfg.BufferLimit.Value,
		// Elevation is on and the password check is a fake, so the run
		// reaches the real gate with the real password. The gate rejects
		// it, and neither the refusal nor anything else may echo it.
		Elevation: nodeagent.Elevation{Enabled: true},
		VerifyCredential: func(string, string) error {
			return runner.ErrPasswordRejected
		},
		ReconnectBase: 10 * time.Millisecond,
		ReconnectMax:  20 * time.Millisecond,
		Logf:          newAgentLogger(logs, cfg.Log.Level, cfg.Log.Format),
	})
	if err != nil {
		t.Fatalf("build the agent: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agentDone := make(chan error, 1)
	go func() { agentDone <- agent.Run(ctx) }()
	waitForOnline(t, hub, "node-a", logs)

	// A privileged run carries the password. The fake password check
	// rejects it, and the run fails with the node's reason.
	_, runErr := hub.Execute(ctx, &agentconn.Call{
		RunID:       "op_secret",
		Node:        "node-a",
		Capability:  "echo",
		Operation:   "say",
		Bundle:      "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		Entry:       []string{"sh", "handler.sh"},
		Request:     sayRequest(t),
		Privileged:  true,
		Password:    password,
		GracePeriod: time.Second,
	})
	if runErr == nil {
		t.Fatal("a privileged run with a wrong password was accepted")
	}
	if strings.Contains(runErr.Error(), password) {
		t.Errorf("the failure reason holds the password: %v", runErr)
	}
	cancel()
	select {
	case err := <-agentDone:
		if err != nil {
			t.Fatalf("the agent stopped with an error: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the agent did not stop")
	}

	logged := logs.String()
	if !strings.Contains(logged, "hanse-node: the connection ended") {
		t.Errorf("the log lacks the agent's own lines:\n%s", logged)
	}
	for _, secret := range []string{password, credential, "hnc_"} {
		if strings.Contains(logged, secret) {
			t.Errorf("the log holds %q:\n%s", secret, logged)
		}
	}
}

// waitForOnline waits until the Gateway sees the node's connection, and
// reports the agent's log when it never does.
func waitForOnline(t *testing.T, hub *agentconn.Hub, node string, logs *agentLog) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !hub.Online(node) {
		if time.Now().After(deadline) {
			t.Fatalf("the agent did not connect; log:\n%s", logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// sayRequest is the handler request envelope of one echo.say run.
func sayRequest(t *testing.T) json.RawMessage {
	t.Helper()
	envelope, err := json.Marshal(map[string]any{
		"contract":   hanruntime.ContractV1,
		"kind":       hanruntime.KindOperation,
		"id":         "op_secret",
		"capability": "echo",
		"operation":  "say",
		"arguments":  map[string]any{"message": "hello"},
		"nodes":      map[string]any{},
		"executor": map[string]any{
			"id": "node-a", "name": "Node A", "os": goruntime.GOOS, "arch": goruntime.GOARCH,
			"address": "192.0.2.11", "attributes": map[string]any{},
		},
		"secrets":  map[string]string{},
		"deadline": time.Now().Add(5 * time.Minute).UTC().Format("2006-01-02T15:04:05.000Z"),
	})
	if err != nil {
		t.Fatalf("encode the request: %v", err)
	}
	return envelope
}

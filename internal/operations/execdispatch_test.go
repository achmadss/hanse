package operations

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/achmadss/hanse/internal/agent"
	"github.com/achmadss/hanse/internal/config"
	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/runtime"
)

// echoCapabilityDirs points at the echo capability package in the
// repository.
func echoCapabilityDirs() []string {
	return []string{"../../capabilities"}
}

// echoInterpreterAvailable reports whether the current OS has the
// interpreter that capabilities/echo uses.
func echoInterpreterAvailable() bool {
	if goruntime.GOOS == "windows" {
		if _, err := exec.LookPath("python"); err == nil {
			return true
		}
		_, err := exec.LookPath("python3")
		return err == nil
	}
	_, err := exec.LookPath("sh")
	return err == nil
}

// loadEchoRegistry loads capabilities/echo with a Gateway host node that
// can run it.
func loadEchoRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg, _, err := registry.Load(registry.Options{
		Dirs: echoCapabilityDirs(),
		Input: registry.Input{
			Nodes: []registry.Node{{
				ID:      "gateway",
				Name:    "Gateway host",
				OS:      goruntime.GOOS,
				Arch:    goruntime.GOARCH,
				Enabled: true,
			}},
			GatewayHost: "gateway",
			Routes: registry.Routes{Capabilities: map[string]registry.CapabilityRoute{
				"echo": {Wildcard: true, DefaultExecutor: "gateway"},
			}},
		},
	})
	if err != nil {
		t.Fatalf("load capabilities/echo: %v", err)
	}
	return reg
}

// echoDispatcher builds the embedded agent and the exec dispatcher over one
// loaded registry.
func echoDispatcher(t *testing.T, reg *registry.Registry) *ExecDispatcher {
	t.Helper()
	host, err := agent.New(agent.Config{
		Node: runtime.Node{
			ID:   "gateway",
			Name: "Gateway host",
			OS:   goruntime.GOOS,
			Arch: goruntime.GOARCH,
		},
		Registry:      reg,
		ResponseLimit: 1 << 20,
		GracePeriod:   time.Second,
		WorkRoot:      t.TempDir(),
	})
	if err != nil {
		t.Fatalf("build the embedded agent: %v", err)
	}
	dispatcher, err := NewExecDispatcher(ExecConfig{Registry: reg, Agent: host})
	if err != nil {
		t.Fatalf("build the exec dispatcher: %v", err)
	}
	return dispatcher
}

// TestExecDispatcherEchoEndToEnd runs capabilities/echo through the exec
// runtime, the embedded agent, and the operations manager.
func TestExecDispatcherEchoEndToEnd(t *testing.T) {
	if !echoInterpreterAvailable() {
		t.Skip("the interpreter of capabilities/echo is not installed")
	}
	reg := loadEchoRegistry(t)
	manager, err := New(Config{
		Registry:   reg,
		Dispatcher: echoDispatcher(t, reg),
		Timeouts: config.Timeouts{
			Operation:   10 * time.Second,
			Options:     10 * time.Second,
			GracePeriod: time.Second,
		},
	})
	if err != nil {
		t.Fatalf("build the manager: %v", err)
	}

	said, err := manager.Submit(Request{
		Capability: "echo",
		Operation:  "say",
		Arguments:  json.RawMessage(`{"message":"hello"}`),
		Requester:  "test",
	})
	if err != nil {
		t.Fatalf("submit echo.say: %v", err)
	}
	if said.Status != StateCompleted {
		t.Fatalf("echo.say status = %q, want completed (failure %+v, logs %v)", said.Status, said.Failure, logLines(t, manager, said.ID))
	}
	if string(said.Result) != `{"said":"hello"}` {
		t.Errorf("echo.say result = %s, want {\"said\":\"hello\"}", said.Result)
	}
	if !logContains(t, manager, said.ID, "say:") {
		t.Errorf("echo.say logs do not hold the handler's stderr line")
	}

	slept, err := manager.Submit(Request{
		Capability: "echo",
		Operation:  "sleep",
		Arguments:  json.RawMessage(`{"seconds":1}`),
		Requester:  "test",
	})
	if err != nil {
		t.Fatalf("submit echo.sleep: %v", err)
	}
	if slept.Status != StateQueued {
		t.Fatalf("echo.sleep status = %q, want queued", slept.Status)
	}
	select {
	case <-manager.Done(slept.ID):
	case <-time.After(15 * time.Second):
		t.Fatalf("echo.sleep did not finish")
	}
	done, err := manager.Get(slept.ID)
	if err != nil {
		t.Fatalf("read echo.sleep: %v", err)
	}
	if done.Status != StateCompleted {
		t.Fatalf("echo.sleep status = %q, want completed (failure %+v)", done.Status, done.Failure)
	}
	if string(done.Result) != `{"slept":1}` {
		t.Errorf("echo.sleep result = %s, want {\"slept\":1}", done.Result)
	}
	if !logContains(t, manager, done.ID, "sleep 1 of 1") {
		t.Errorf("echo.sleep logs do not hold the countdown line")
	}
}

// logContains reports whether one operation log holds text.
func logContains(t *testing.T, manager *Manager, id, text string) bool {
	t.Helper()
	for _, line := range logLines(t, manager, id) {
		if strings.Contains(line, text) {
			return true
		}
	}
	return false
}

// logLines returns the text of one operation log.
func logLines(t *testing.T, manager *Manager, id string) []string {
	t.Helper()
	entries, err := manager.Logs(id, 0)
	if err != nil {
		t.Fatalf("read logs of %s: %v", id, err)
	}
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		lines = append(lines, entry.Line)
	}
	return lines
}

// TestExecDispatcherRejectsRemoteExecutor covers a call for a node agent
// executor: the embedded agent runs only the Gateway host.
func TestExecDispatcherRejectsRemoteExecutor(t *testing.T) {
	reg := loadEchoRegistry(t)
	dispatcher := echoDispatcher(t, reg)
	_, err := dispatcher.Execute(context.Background(), &Call{Request: &runtime.Request{
		Contract:   runtime.ContractV1,
		Kind:       runtime.KindOperation,
		ID:         "op_test",
		Capability: "echo",
		Operation:  "say",
		Arguments:  json.RawMessage(`{"message":"hello"}`),
		Executor:   runtime.Node{ID: "node-b", OS: goruntime.GOOS, Arch: goruntime.GOARCH},
		Deadline:   time.Now().Add(5 * time.Second),
	}})
	if !errors.Is(err, herrors.ExecutorUnavailable) {
		t.Fatalf("error = %v, want executor_unavailable", err)
	}
}

// TestNewExecDispatcherRequiresInputs covers the construction checks.
func TestNewExecDispatcherRequiresInputs(t *testing.T) {
	if _, err := NewExecDispatcher(ExecConfig{}); !errors.Is(err, herrors.Internal) {
		t.Errorf("error = %v, want internal", err)
	}
	reg := registry.New()
	if _, err := NewExecDispatcher(ExecConfig{Registry: reg}); !errors.Is(err, herrors.Internal) {
		t.Errorf("error = %v, want internal", err)
	}
}

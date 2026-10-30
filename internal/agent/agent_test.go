package agent

import (
	"context"
	"encoding/json"
	"errors"
	goruntime "runtime"
	"testing"
	"time"

	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/runtime"
)

// loadTestRegistry loads capabilities/echo with a Gateway host node.
func loadTestRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg, _, err := registry.Load(registry.Options{
		Dirs: []string{"../../capabilities"},
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

// newTestAgent builds an agent for one OS. A test uses another OS to reach
// the missing-entry path without running a handler.
func newTestAgent(t *testing.T, reg *registry.Registry, osName string) *Agent {
	t.Helper()
	host, err := New(Config{
		Node: runtime.Node{
			ID:   "gateway",
			Name: "Gateway host",
			OS:   osName,
			Arch: goruntime.GOARCH,
		},
		Registry: reg,
		WorkRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("build the agent: %v", err)
	}
	return host
}

// TestNewRequiresNodeAndRegistry covers the construction checks.
func TestNewRequiresNodeAndRegistry(t *testing.T) {
	if _, err := New(Config{Registry: registry.New()}); !errors.Is(err, herrors.Internal) {
		t.Errorf("error = %v, want internal", err)
	}
	if _, err := New(Config{Node: runtime.Node{ID: "gateway"}}); !errors.Is(err, herrors.Internal) {
		t.Errorf("error = %v, want internal", err)
	}
}

// TestExecuteUnknownCapability covers an envelope for a capability that is
// not loaded.
func TestExecuteUnknownCapability(t *testing.T) {
	host := newTestAgent(t, loadTestRegistry(t), goruntime.GOOS)
	_, err := host.Execute(context.Background(), &runtime.Request{
		Contract:   runtime.ContractV1,
		Kind:       runtime.KindOperation,
		ID:         "op_test",
		Capability: "nope",
		Operation:  "say",
		Arguments:  json.RawMessage(`{}`),
		Deadline:   time.Now().Add(5 * time.Second),
	}, nil)
	if !errors.Is(err, herrors.UnknownCapability) {
		t.Fatalf("error = %v, want unknown_capability", err)
	}
}

// TestExecuteMissingEntryForOS covers an OS that the manifest does not
// support.
func TestExecuteMissingEntryForOS(t *testing.T) {
	host := newTestAgent(t, loadTestRegistry(t), "plan9")
	_, err := host.Execute(context.Background(), &runtime.Request{
		Contract:   runtime.ContractV1,
		Kind:       runtime.KindOperation,
		ID:         "op_test",
		Capability: "echo",
		Operation:  "say",
		Arguments:  json.RawMessage(`{"message":"hello"}`),
		Deadline:   time.Now().Add(5 * time.Second),
	}, nil)
	if !errors.Is(err, herrors.HandlerStartFailed) {
		t.Fatalf("error = %v, want handler_start_failed", err)
	}
}

// TestExecuteRejectsRemoteExecutor covers an envelope whose executor is a
// node agent, not the Gateway host.
func TestExecuteRejectsRemoteExecutor(t *testing.T) {
	host := newTestAgent(t, loadTestRegistry(t), goruntime.GOOS)
	_, err := host.Execute(context.Background(), &runtime.Request{
		Contract:   runtime.ContractV1,
		Kind:       runtime.KindOperation,
		ID:         "op_test",
		Capability: "echo",
		Operation:  "say",
		Arguments:  json.RawMessage(`{"message":"hello"}`),
		Executor:   runtime.Node{ID: "node-b", OS: goruntime.GOOS},
		Deadline:   time.Now().Add(5 * time.Second),
	}, nil)
	if !errors.Is(err, herrors.ExecutorUnavailable) {
		t.Fatalf("error = %v, want executor_unavailable", err)
	}
}

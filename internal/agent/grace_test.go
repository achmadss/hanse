package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"testing"
	"time"

	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/runner"
	"github.com/achmadss/hanse/internal/runtime"
)

// stubbornManifest is a capability whose operation times out quickly and
// allows a short grace period. The handler ignores the stop signal, so only
// the grace period decides when the runner kills it.
const stubbornManifest = `version: 1
id: stubborn
name: Stubborn
description: Ignore the stop request past a short manifest grace period
contract: hanse.handler/v1

handler:
  kind: exec
  entry:
    linux: [sh, handler.sh]
    darwin: [sh, handler.sh]

operations:
  ignore:
    description: Ignore the stop request
    mode: async
    timeout: 200ms
    grace_period: 100ms
    arguments:
      type: object
      additionalProperties: false
      properties: {}
`

// stubbornHandler ignores every stop signal.
const stubbornHandler = `#!/bin/sh
trap '' TERM
trap '' INT
while :; do
  sleep 1
done
`

// loadStubbornRegistry writes the capability and loads it for the Gateway
// host node.
func loadStubbornRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "capabilities", "stubborn")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create capability directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(stubbornManifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "handler.sh"), []byte(stubbornHandler), 0o755); err != nil {
		t.Fatalf("write handler: %v", err)
	}
	reg, warnings, err := registry.Load(registry.Options{
		Dirs: []string{filepath.Dir(dir)},
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
				"stubborn": {Wildcard: true, DefaultExecutor: "gateway"},
			}},
		},
	})
	if err != nil {
		t.Fatalf("load the stubborn capability: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %+v", warnings)
	}
	return reg
}

// TestExecuteHonoursTheManifestGracePeriod requires the embedded agent to
// kill a stopped handler after the operation's own grace period, not after
// the Gateway's default of ten seconds.
func TestExecuteHonoursTheManifestGracePeriod(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("the fixture handler is a shell script")
	}
	host, err := New(Config{
		Node: runtime.Node{
			ID:   "gateway",
			Name: "Gateway host",
			OS:   goruntime.GOOS,
			Arch: goruntime.GOARCH,
		},
		Registry: loadStubbornRegistry(t),
		// The Gateway default; the manifest's 100ms must win.
		GracePeriod: 10 * time.Second,
		WorkRoot:    t.TempDir(),
	})
	if err != nil {
		t.Fatalf("build the agent: %v", err)
	}

	start := time.Now()
	var system []string
	_, err = host.Execute(context.Background(), &runtime.Request{
		Contract:   runtime.ContractV1,
		Kind:       runtime.KindOperation,
		ID:         "op_grace",
		Capability: "stubborn",
		Operation:  "ignore",
		Arguments:  json.RawMessage(`{}`),
		Deadline:   start.Add(200 * time.Millisecond),
	}, nil, func(line string) { system = append(system, line) })
	elapsed := time.Since(start)
	if !errors.Is(err, herrors.Timeout) {
		t.Fatalf("error = %v, want timeout", err)
	}
	if elapsed < 250*time.Millisecond {
		t.Errorf("execute returned after %s, before the deadline and the grace period", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Errorf("execute returned after %s, want the 200ms timeout plus the 100ms manifest grace", elapsed)
	}
	if !slices.Contains(system, runner.KilledLine) {
		t.Errorf("system lines %v do not record the kill", system)
	}
}

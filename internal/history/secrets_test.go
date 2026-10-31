package history_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/achmadss/hanse/internal/config"
	"github.com/achmadss/hanse/internal/history"
	"github.com/achmadss/hanse/internal/operations"
	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/runtime"
)

// secretManifest declares one argument-level secret, one argument whose
// name is a declared secret, and an ordinary argument.
const secretManifest = `version: 1
id: demo
name: Demo
description: Demonstrate secret redaction
contract: hanse.handler/v1

handler:
  kind: exec
  entry:
    linux: [sh, handler.sh]

secrets:
  ssh_key: Private key for target nodes

operations:
  secret:
    description: Run with a secret argument
    mode: sync
    secrets: [ssh_key]
    arguments:
      type: object
      required: [target, token]
      additionalProperties: false
      properties:
        target:
          type: string
        token:
          type: string
          writeOnly: true
        ssh_key:
          type: string
        note:
          type: string

  elevated:
    description: Run with a secret argument and an elevation password
    mode: sync
    privileged: true
    secrets: [ssh_key]
    arguments:
      type: object
      required: [target, token]
      additionalProperties: false
      properties:
        target:
          type: string
        token:
          type: string
          writeOnly: true
        ssh_key:
          type: string
        note:
          type: string
`

// The values the test proves absent from the database file.
const (
	secretToken    = "S3CR3T-token-value-123"
	secretKey      = "S3CR3T-ssh-key-value-456"
	secretPassword = "S3CR3T-elevpass-789"
)

// leakyDispatcher returns the elevation password in its result and summary,
// so the test also covers text a handler echoes.
type leakyDispatcher struct{}

func (leakyDispatcher) Execute(_ context.Context, call *operations.Call) (runtime.Response, error) {
	if !bytes.Contains(call.Request.Arguments, []byte(secretToken)) {
		return runtime.Response{}, context.Canceled
	}
	return runtime.OK(map[string]any{
		"state": "online",
		"echo":  secretPassword,
	}, "finished with "+secretPassword)
}

// secretRegistry loads the fixture capability and one executor node.
func secretRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "capabilities")
	if err := os.MkdirAll(filepath.Join(dir, "demo"), 0o755); err != nil {
		t.Fatalf("create capability directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "demo", "manifest.yaml"), []byte(secretManifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "demo", "handler.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatalf("write handler: %v", err)
	}
	reg, warnings, err := registry.Load(registry.Options{
		Dirs: []string{dir},
		Input: registry.Input{
			GatewayHost: "node-a",
			Nodes:       []registry.Node{{ID: "node-a", OS: "linux", Arch: "amd64", Enabled: true}},
			Routes: registry.Routes{Capabilities: map[string]registry.CapabilityRoute{
				"demo": {Executors: []string{"node-a"}, DefaultExecutor: "node-a"},
			}},
		},
	})
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("load fixture: unexpected warnings: %+v", warnings)
	}
	return reg
}

// TestSecretsNeverReachTheDatabase submits an operation with secret
// arguments and an elevation password, then scans every database file for
// the secret bytes (acceptance criterion: secrets never reach storage).
func TestSecretsNeverReachTheDatabase(t *testing.T) {
	stateDir := t.TempDir()
	store := open(t, history.Options{StateDir: stateDir})

	manager, err := operations.New(operations.Config{
		Registry:   secretRegistry(t),
		Dispatcher: leakyDispatcher{},
		Store:      store,
		Timeouts:   config.Timeouts{},
	})
	if err != nil {
		t.Fatalf("build manager: %v", err)
	}

	arguments, err := json.Marshal(map[string]any{
		"target":  "alpha",
		"token":   secretToken,
		"ssh_key": secretKey,
		"note":    "password is " + secretPassword,
	})
	if err != nil {
		t.Fatalf("marshal arguments: %v", err)
	}
	rec, err := manager.Submit(operations.Request{
		Capability: "demo",
		Operation:  "elevated",
		Arguments:  arguments,
		Requester:  "tester",
		Elevation:  &operations.Elevation{Password: secretPassword},
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if rec.Status != operations.StateCompleted {
		t.Fatalf("status %q, want completed", rec.Status)
	}

	var stored map[string]any
	if err := json.Unmarshal(rec.Arguments, &stored); err != nil {
		t.Fatalf("decode stored arguments %s: %v", rec.Arguments, err)
	}
	if stored["target"] != "alpha" {
		t.Errorf("stored target = %v, want alpha", stored["target"])
	}
	for _, field := range []string{"token", "ssh_key"} {
		if stored[field] != "[redacted]" {
			t.Errorf("stored %s = %v, want [redacted]", field, stored[field])
		}
	}
	if note, _ := stored["note"].(string); strings.Contains(note, secretPassword) {
		t.Errorf("stored note %q contains the elevation password", note)
	}
	if rec.Summary != "" && strings.Contains(rec.Summary, secretPassword) {
		t.Errorf("stored summary %q contains the elevation password", rec.Summary)
	}
	if bytes.Contains(rec.Result, []byte(secretPassword)) {
		t.Errorf("stored result %s contains the elevation password", rec.Result)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		data, err := os.ReadFile(filepath.Join(stateDir, history.DatabaseName) + suffix)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("read database file: %v", err)
		}
		for name, secret := range map[string]string{
			"token argument":     secretToken,
			"secret argument":    secretKey,
			"elevation password": secretPassword,
		} {
			if bytes.Contains(data, []byte(secret)) {
				t.Errorf("the database file holds the %s", name)
			}
		}
	}
}

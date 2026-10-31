package api

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/achmadss/hanse/internal/auth"
	"github.com/achmadss/hanse/internal/config"
	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/operations"
	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/runtime"
)

// elevationPassword is the obviously fake password of these tests.
const elevationPassword = "placeholder-elevation-password"

// privilegedManifest is a capability with one privileged operation.
const privilegedManifest = `version: 1
id: demo
name: Demo
description: Restart one service
contract: hanse.handler/v1

handler:
  kind: exec
  entry:
    linux: [sh, handler.sh]

operations:
  restart:
    description: Restart one service with administrator rights
    mode: sync
    privileged: true
    arguments:
      type: object
      required: [service]
      additionalProperties: false
      properties:
        service:
          type: string
`

// recordingDispatcher answers every call and remembers the elevation it
// received, so the API tests can prove the password reached dispatch and
// nothing else.
type recordingDispatcher struct {
	mu        sync.Mutex
	passwords []string
	err       error
}

func (d *recordingDispatcher) Execute(_ context.Context, call *operations.Call) (runtime.Response, error) {
	d.mu.Lock()
	password := ""
	if call.Elevation != nil {
		password = call.Elevation.Password
	}
	d.passwords = append(d.passwords, password)
	err := d.err
	d.mu.Unlock()
	if err != nil {
		return runtime.Response{}, err
	}
	return runtime.OK(map[string]any{"restarted": true}, "restarted")
}

func (d *recordingDispatcher) received() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.passwords...)
}

// privilegedFixture is one Gateway with a privileged capability.
type privilegedFixture struct {
	handler    http.Handler
	dispatcher *recordingDispatcher
	token      string
}

// newPrivilegedFixture builds the Gateway over a manifest with one
// privileged operation and one dispatcher that records the elevation.
func newPrivilegedFixture(t *testing.T) *privilegedFixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "capabilities")
	if err := os.MkdirAll(filepath.Join(dir, "demo"), 0o755); err != nil {
		t.Fatalf("create capability directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "demo", "manifest.yaml"), []byte(privilegedManifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "demo", "handler.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatalf("write handler: %v", err)
	}
	reg, warnings, err := registry.Load(registry.Options{
		Dirs: []string{dir},
		Input: registry.Input{
			GatewayHost: "gateway",
			Nodes:       []registry.Node{{ID: "gateway", OS: "linux", Arch: "amd64", Enabled: true}},
			Routes: registry.Routes{Capabilities: map[string]registry.CapabilityRoute{
				"demo": {Executors: []string{"gateway"}, DefaultExecutor: "gateway"},
			}},
		},
	})
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("load fixture: unexpected warnings: %+v", warnings)
	}
	dispatcher := &recordingDispatcher{}
	manager, err := operations.New(operations.Config{
		Registry:   reg,
		Dispatcher: dispatcher,
		Timeouts:   config.Timeouts{Operation: 10 * time.Second, GracePeriod: time.Second},
	})
	if err != nil {
		t.Fatalf("build manager: %v", err)
	}
	store, err := auth.LoadStore(filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatalf("load token store: %v", err)
	}
	token, err := store.Add("tester", nil)
	if err != nil {
		t.Fatalf("add token: %v", err)
	}
	return &privilegedFixture{
		handler:    store.Middleware(New(reg, manager, nil)),
		dispatcher: dispatcher,
		token:      token,
	}
}

// TestElevationRequired covers a privileged operation submitted without a
// password (docs/architecture.md section 8.5).
func TestElevationRequired(t *testing.T) {
	fixture := newPrivilegedFixture(t)
	body := `{"capability":"demo","operation":"restart","arguments":{"service":"demo"}}`
	resp, payload := call(t, fixture.handler, fixture.token, http.MethodPost, "/v1/operations", body, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d, want 403: %s", resp.StatusCode, payload)
	}
	if got := decodeWireError(t, payload).Code; got != "elevation_required" {
		t.Errorf("code %q, want elevation_required", got)
	}
	if got := fixture.dispatcher.received(); len(got) != 0 {
		t.Errorf("dispatch ran %d times, want none", len(got))
	}
}

// TestElevationDeniedSurfacesAsItsCode proves that a refusal from the node
// is the documented API error and not a generic handler failure.
func TestElevationDeniedSurfacesAsItsCode(t *testing.T) {
	fixture := newPrivilegedFixture(t)
	fixture.dispatcher.err = herrors.New(herrors.ElevationDenied,
		"the elevation password is wrong", nil)
	body := `{"capability":"demo","operation":"restart","arguments":{"service":"demo"},` +
		`"elevation":{"password":"` + elevationPassword + `"}}`
	resp, payload := call(t, fixture.handler, fixture.token, http.MethodPost, "/v1/operations", body, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d, want 403: %s", resp.StatusCode, payload)
	}
	if got := decodeWireError(t, payload).Code; got != "elevation_denied" {
		t.Errorf("code %q, want elevation_denied", got)
	}
	if bytes.Contains(payload, []byte(elevationPassword)) {
		t.Errorf("the error holds the elevation password: %s", payload)
	}
}

// TestPrivilegedElevationNeverReturned sends the password of a privileged
// operation and requires it to stay out of every response.
func TestPrivilegedElevationNeverReturned(t *testing.T) {
	fixture := newPrivilegedFixture(t)
	body := `{"capability":"demo","operation":"restart","arguments":{"service":"demo"},` +
		`"elevation":{"password":"` + elevationPassword + `"}}`
	resp, payload := call(t, fixture.handler, fixture.token, http.MethodPost, "/v1/operations", body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("submit: status %d, body %s", resp.StatusCode, payload)
	}
	if bytes.Contains(payload, []byte(elevationPassword)) {
		t.Errorf("the submit response holds the elevation password: %s", payload)
	}
	if got := fixture.dispatcher.received(); len(got) != 1 || got[0] != elevationPassword {
		t.Errorf("dispatch passwords %v, want the one supplied password", got)
	}
	view := decodeOperation(t, payload)
	resp, payload = call(t, fixture.handler, fixture.token, http.MethodGet, "/v1/operations/"+view.ID, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET operation: status %d, body %s", resp.StatusCode, payload)
	}
	if bytes.Contains(payload, []byte(elevationPassword)) {
		t.Errorf("the operation response holds the elevation password: %s", payload)
	}
}

// TestPasswordForNonPrivilegedOperationIsRejected covers a password sent
// with an operation that is not privileged (docs/architecture.md section
// 8.5).
func TestPasswordForNonPrivilegedOperationIsRejected(t *testing.T) {
	fixture := newExecutionFixture(t)
	body := `{"capability":"echo","operation":"say","arguments":{"message":"hello"},` +
		`"elevation":{"password":"` + elevationPassword + `"}}`
	resp, payload := call(t, fixture.handler, fixture.token, http.MethodPost, "/v1/operations", body, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", resp.StatusCode, payload)
	}
	if got := decodeWireError(t, payload).Code; got != "invalid_arguments" {
		t.Errorf("code %q, want invalid_arguments", got)
	}
	if bytes.Contains(payload, []byte(elevationPassword)) {
		t.Errorf("the error holds the elevation password: %s", payload)
	}
}

// TestValidateElevationRules covers the dry-run endpoint: it never needs
// the password of a privileged operation, and it rejects a password for an
// operation that is not privileged.
func TestValidateElevationRules(t *testing.T) {
	fixture := newPrivilegedFixture(t)
	privileged := `{"capability":"demo","operation":"restart","arguments":{"service":"demo"}}`
	resp, payload := call(t, fixture.handler, fixture.token, http.MethodPost, "/v1/operations/validate", privileged, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("validate without a password: status %d, body %s", resp.StatusCode, payload)
	}

	ordinary := newExecutionFixture(t)
	withPassword := `{"capability":"echo","operation":"say","arguments":{"message":"hello"},` +
		`"elevation":{"password":"` + elevationPassword + `"}}`
	resp, payload = call(t, ordinary.handler, ordinary.token, http.MethodPost, "/v1/operations/validate", withPassword, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("validate with a password for an ordinary operation: status %d, body %s", resp.StatusCode, payload)
	}
}

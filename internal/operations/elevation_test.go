package operations_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/operations"
	"github.com/achmadss/hanse/internal/runtime"
)

// elevationPassword is the obviously fake password of these tests. It
// must never appear in a record, a log line, a result, or an error.
const elevationPassword = "placeholder-elevation-password"

// privilegedRequest is the submit of the fixture's privileged operation.
func privilegedRequest(password string) operations.Request {
	args, _ := json.Marshal(map[string]any{"target": "alpha"})
	req := operations.Request{
		Capability: "demo",
		Operation:  "privileged",
		Arguments:  args,
		Requester:  "tester",
	}
	if password != "" {
		req.Elevation = &operations.Elevation{Password: password}
	}
	return req
}

// TestSubmitPrivilegedNeedsPassword covers the missing password of a
// privileged operation (docs/architecture.md section 8.5).
func TestSubmitPrivilegedNeedsPassword(t *testing.T) {
	d := newDispatcher()
	manager := newManager(t, d)

	_, err := manager.Submit(privilegedRequest(""))
	if err == nil {
		t.Fatal("a privileged operation without a password was accepted")
	}
	if !errors.Is(err, herrors.ElevationRequired) {
		t.Fatalf("err = %v, want the elevation_required code", err)
	}
	if got := herrors.HTTPStatus(err); got != 403 {
		t.Errorf("HTTP status %d, want 403", got)
	}
	if got := herrors.ExitCode(err); got != 30 {
		t.Errorf("CLI exit code %d, want 30", got)
	}
	if d.callCount() != 0 {
		t.Errorf("dispatch ran %d times, want none", d.callCount())
	}
}

// TestSubmitEmptyPrivilegedPasswordIsMissing covers an elevation object
// whose password is empty: it is as missing as no object at all.
func TestSubmitEmptyPrivilegedPasswordIsMissing(t *testing.T) {
	d := newDispatcher()
	manager := newManager(t, d)

	req := privilegedRequest("")
	req.Elevation = &operations.Elevation{}
	_, err := manager.Submit(req)
	if !errors.Is(err, herrors.ElevationRequired) {
		t.Fatalf("err = %v, want the elevation_required code", err)
	}
	if d.callCount() != 0 {
		t.Errorf("dispatch ran %d times, want none", d.callCount())
	}
}

// TestSubmitRejectsPasswordForNonPrivilegedOperation covers a password
// sent with an operation that is not privileged: nothing may run with
// administrator rights by accident.
func TestSubmitRejectsPasswordForNonPrivilegedOperation(t *testing.T) {
	d := newDispatcher()
	manager := newManager(t, d)

	args, _ := json.Marshal(map[string]any{"target": "alpha"})
	_, err := manager.Submit(operations.Request{
		Capability: "demo",
		Operation:  "quick",
		Arguments:  args,
		Requester:  "tester",
		Elevation:  &operations.Elevation{Password: elevationPassword},
	})
	if !errors.Is(err, herrors.InvalidArguments) {
		t.Fatalf("err = %v, want the invalid_arguments code", err)
	}
	if got := herrors.HTTPStatus(err); got != 422 {
		t.Errorf("HTTP status %d, want 422", got)
	}
	if strings.Contains(err.Error(), elevationPassword) {
		t.Error("the error message holds the elevation password")
	}
	if d.callCount() != 0 {
		t.Errorf("dispatch ran %d times, want none", d.callCount())
	}
}

// leakyElevationDispatcher behaves like a careless handler: it logs the
// elevation password and echoes it in its result and summary, so the test
// proves the manager removes it from every stored text.
type leakyElevationDispatcher struct{}

func (leakyElevationDispatcher) Execute(_ context.Context, call *operations.Call) (runtime.Response, error) {
	if call.Elevation == nil || call.Elevation.Password != elevationPassword {
		return runtime.Response{}, errors.New("the dispatcher received no elevation password")
	}
	call.Log(operations.StreamStderr, "the password is "+elevationPassword)
	return runtime.OK(map[string]any{"echo": elevationPassword}, "finished with "+elevationPassword)
}

// TestElevationPasswordNeverReachesARecordOrLog covers the acceptance
// criterion that the password appears in no log line, history record, or
// result. The record itself, its arguments, its summary, its result, and
// every log entry are scanned.
func TestElevationPasswordNeverReachesARecordOrLog(t *testing.T) {
	logStore := operations.NewMemoryLogStore()
	manager, err := operations.New(operations.Config{
		Registry:   newFixture(t),
		Dispatcher: leakyElevationDispatcher{},
		LogStore:   logStore,
	})
	if err != nil {
		t.Fatalf("build manager: %v", err)
	}

	rec, err := manager.Submit(privilegedRequest(elevationPassword))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if rec.Status != operations.StateCompleted {
		t.Fatalf("status %q, want completed", rec.Status)
	}

	stored, err := manager.Get(rec.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	for name, text := range map[string]string{
		"arguments": string(stored.Arguments),
		"result":    string(stored.Result),
		"summary":   stored.Summary,
	} {
		if strings.Contains(text, elevationPassword) {
			t.Errorf("the stored %s holds the elevation password: %s", name, text)
		}
	}
	if !strings.Contains(string(stored.Result), "[redacted]") {
		t.Errorf("result %s, want the echoed password redacted", stored.Result)
	}
	if strings.Contains(stored.Summary, elevationPassword) {
		t.Errorf("summary %q holds the elevation password", stored.Summary)
	}

	entries, err := logStore.Logs(rec.ID, 0)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the operation stored no log entries")
	}
	for _, entry := range entries {
		if strings.Contains(entry.Line, elevationPassword) {
			t.Errorf("log entry %q holds the elevation password", entry.Line)
		}
	}
	// A replay under the same Idempotency-Key returns the stored operation
	// and runs nothing, so it needs no password.
	first := privilegedRequest(elevationPassword)
	first.IdempotencyKey = "idem-1"
	original, err := manager.Submit(first)
	if err != nil {
		t.Fatalf("first keyed submit: %v", err)
	}
	retry := privilegedRequest("")
	retry.IdempotencyKey = "idem-1"
	retried, err := manager.Submit(retry)
	if err != nil {
		t.Fatalf("keyed retry: %v", err)
	}
	if retried.ID != original.ID {
		t.Errorf("retry returned %s, want the stored operation %s", retried.ID, original.ID)
	}
}

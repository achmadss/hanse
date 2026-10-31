package operations_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/achmadss/hanse/internal/config"
	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/operations"
	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/runtime"
)

// manifest is the fixture capability: one asynchronous operation that
// allows one run per `target` value, and one synchronous operation.
const manifest = `version: 1
id: demo
name: Demo
description: Demonstrate the operation lifecycle
contract: hanse.handler/v1

handler:
  kind: exec
  entry:
    linux: [sh, handler.sh]

operations:
  run:
    description: Run a job for one target
    mode: async
    concurrency:
      limit: 1
      key: [target]
    arguments:
      type: object
      required: [target]
      additionalProperties: false
      properties:
        target:
          type: string

  quick:
    description: Finish at once
    mode: sync
    arguments:
      type: object
      additionalProperties: false
      properties:
        target:
          type: string

  privileged:
    description: Finish at once with administrator rights
    mode: sync
    privileged: true
    arguments:
      type: object
      additionalProperties: false
      properties:
        target:
          type: string

  probe:
    description: Probe one target node
    mode: sync
    node_filters:
      node:
        roles: [target]
    arguments:
      type: object
      required: [node]
      additionalProperties: false
      properties:
        node:
          type: string
          format: hanse:node
`

// fixtureNode is the only routed node and the Gateway host, so the resolver
// can pick it as the default executor.
const fixtureNode = "node-a"

// fixtureTarget is a node that is a valid `hanse:node` target but is not an
// eligible executor.
const fixtureTarget = "node-b"

// newFixture loads the capability and marks the node online, so the
// resolver accepts it.
func newFixture(t *testing.T) *registry.Registry {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "capabilities")
	if err := os.MkdirAll(filepath.Join(dir, "demo"), 0o755); err != nil {
		t.Fatalf("create capability directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "demo", "manifest.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "demo", "handler.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatalf("write handler: %v", err)
	}
	reg, warnings, err := registry.Load(registry.Options{
		Dirs: []string{dir},
		Input: registry.Input{
			GatewayHost: fixtureNode,
			Nodes: []registry.Node{
				{
					ID:      fixtureNode,
					Name:    "Gateway host",
					OS:      "linux",
					Arch:    "amd64",
					Address: "192.0.2.10",
					Enabled: true,
					Attributes: map[string]string{
						"site": "lab",
					},
				},
				{
					ID:      fixtureTarget,
					Name:    "Target machine",
					OS:      "linux",
					Arch:    "arm64",
					Address: "192.0.2.11",
					Roles:   []string{"target"},
					Enabled: true,
					Attributes: map[string]string{
						"mac": "00:00:5e:00:53:02",
					},
				},
			},
			Routes: registry.Routes{Capabilities: map[string]registry.CapabilityRoute{
				"demo": {
					Executors:       []string{fixtureNode},
					DefaultExecutor: fixtureNode,
				},
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

// newManager builds a manager over the fixture with short timeouts.
func newManager(t *testing.T, dispatcher operations.Dispatcher) *operations.Manager {
	t.Helper()
	manager, err := operations.New(operations.Config{
		Registry:   newFixture(t),
		Dispatcher: dispatcher,
		Timeouts: config.Timeouts{
			Operation:   5 * time.Second,
			GracePeriod: time.Second,
		},
	})
	if err != nil {
		t.Fatalf("build manager: %v", err)
	}
	return manager
}

// dispatcher is a fake runtime. When release is set, Execute reports
// Cancelled on context cancellation, like a cooperative handler.
type dispatcher struct {
	release chan struct{}
	started chan *operations.Call
	mu      sync.Mutex
	calls   int
}

func newDispatcher() *dispatcher {
	return &dispatcher{started: make(chan *operations.Call, 8)}
}

func (d *dispatcher) Execute(ctx context.Context, call *operations.Call) (runtime.Response, error) {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	d.started <- call
	if d.release != nil {
		select {
		case <-d.release:
		case <-ctx.Done():
			return runtime.Cancelled(map[string]any{"stopped": true}, "stopped")
		}
	}
	return runtime.OK(map[string]any{"ok": true}, "done")
}

func (d *dispatcher) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// submit is the common request of the tests.
func submit(target, key, requester string) operations.Request {
	args, _ := json.Marshal(map[string]any{"target": target})
	return operations.Request{
		Capability:     "demo",
		Operation:      "run",
		Arguments:      args,
		IdempotencyKey: key,
		Requester:      requester,
	}
}

// waitFor polls until the operation reaches want or the deadline passes.
func waitFor(t *testing.T, manager *operations.Manager, id string, want operations.State) operations.Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec, err := manager.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if rec.Status == want {
			return rec
		}
		time.Sleep(2 * time.Millisecond)
	}
	rec, _ := manager.Get(id)
	t.Fatalf("operation %s stayed %s, want %s", id, rec.Status, want)
	return operations.Record{}
}

// TestConcurrencyConflict runs one job per key and rejects the second job
// while the first one runs, with the running operation's ID in the details.
func TestConcurrencyConflict(t *testing.T) {
	d := newDispatcher()
	d.release = make(chan struct{})
	manager := newManager(t, d)

	first, err := manager.Submit(submit("alpha", "", "tester"))
	if err != nil {
		t.Fatalf("submit first: %v", err)
	}
	if first.Status != operations.StateQueued {
		t.Fatalf("first status %q, want queued", first.Status)
	}
	<-d.started

	_, err = manager.Submit(submit("alpha", "", "tester"))
	if !errors.Is(err, herrors.OperationConflict) {
		t.Fatalf("second submit error %v, want operation_conflict", err)
	}
	var e *herrors.Error
	if !errors.As(err, &e) {
		t.Fatalf("second submit error %v is not a herrors.Error", err)
	}
	if got := e.Details["operation_id"]; got != first.ID {
		t.Errorf("conflict details operation_id %v, want %s", got, first.ID)
	}

	// Another key value has its own limit, so it is accepted.
	other, err := manager.Submit(submit("beta", "", "tester"))
	if err != nil {
		t.Fatalf("submit other key: %v", err)
	}
	<-d.started

	close(d.release)
	waitFor(t, manager, first.ID, operations.StateCompleted)
	waitFor(t, manager, other.ID, operations.StateCompleted)

	// Once the run finished, the same key value may run again.
	again, err := manager.Submit(submit("alpha", "", "tester"))
	if err != nil {
		t.Fatalf("submit after completion: %v", err)
	}
	waitFor(t, manager, again.ID, operations.StateCompleted)
}

// TestIdempotency covers the repeated key, the reused key with other
// arguments, and the requester scoping.
func TestIdempotency(t *testing.T) {
	d := newDispatcher()
	manager := newManager(t, d)

	first, err := manager.Submit(submit("alpha", "key-1", "tester"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitFor(t, manager, first.ID, operations.StateCompleted)

	retry, err := manager.Submit(submit("alpha", "key-1", "tester"))
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if retry.ID != first.ID {
		t.Errorf("retry returned operation %s, want %s", retry.ID, first.ID)
	}
	if retry.Status != operations.StateCompleted {
		t.Errorf("retry status %q, want completed", retry.Status)
	}
	if got := d.callCount(); got != 1 {
		t.Errorf("dispatcher ran %d times, want once", got)
	}

	_, err = manager.Submit(submit("beta", "key-1", "tester"))
	if !errors.Is(err, herrors.InvalidArguments) {
		t.Fatalf("same key with other arguments: error %v, want invalid_arguments", err)
	}

	// The same key from another requester is a different request.
	other, err := manager.Submit(submit("alpha", "key-1", "other"))
	if err != nil {
		t.Fatalf("submit for another requester: %v", err)
	}
	if other.ID == first.ID {
		t.Errorf("another requester reused operation %s", first.ID)
	}
	waitFor(t, manager, other.ID, operations.StateCompleted)
}

// TestCancelRunning asks a running operation to stop and sees it move to
// cancelled, with the partial result kept.
func TestCancelRunning(t *testing.T) {
	d := newDispatcher()
	d.release = make(chan struct{})
	manager := newManager(t, d)

	started, err := manager.Submit(submit("alpha", "", "tester"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	<-d.started
	waitFor(t, manager, started.ID, operations.StateRunning)

	cancelled, err := manager.Cancel(started.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cancelled.Status != operations.StateCancelled {
		t.Fatalf("cancel status %q, want cancelled", cancelled.Status)
	}
	if cancelled.FinishedAt.IsZero() {
		t.Error("cancelled operation has no finish time")
	}
	if string(cancelled.Result) != `{"stopped":true}` {
		t.Errorf("cancelled result %s, want the partial result", cancelled.Result)
	}

	// Cancelling a finished operation is safe and changes nothing.
	again, err := manager.Cancel(started.ID)
	if err != nil {
		t.Fatalf("cancel again: %v", err)
	}
	if again.Status != operations.StateCancelled {
		t.Errorf("repeated cancel status %q, want cancelled", again.Status)
	}
}

// lateOKDispatcher ignores the stop request briefly and then reports
// success, like a handler that finishes its work during the grace period.
type lateOKDispatcher struct {
	started chan struct{}
}

// Execute waits for the stop request, finishes the work, and reports ok.
func (d *lateOKDispatcher) Execute(ctx context.Context, _ *operations.Call) (runtime.Response, error) {
	d.started <- struct{}{}
	<-ctx.Done()
	time.Sleep(20 * time.Millisecond)
	return runtime.OK(map[string]any{"finished": true}, "finished during the grace period")
}

// TestCancelRunningHandlerFinishesOK requires a handler that reports ok
// during the grace period to end completed, per docs/handler-contract.md
// section 5, with the handler exit line last in the log.
func TestCancelRunningHandlerFinishesOK(t *testing.T) {
	d := &lateOKDispatcher{started: make(chan struct{})}
	manager := newManager(t, d)

	started, err := manager.Submit(submit("alpha", "", "tester"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	<-d.started
	waitFor(t, manager, started.ID, operations.StateRunning)

	final, err := manager.Cancel(started.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if final.Status != operations.StateCompleted {
		t.Fatalf("status %q, want completed", final.Status)
	}
	if string(final.Result) != `{"finished":true}` {
		t.Errorf("result %s, want the handler's result", final.Result)
	}
	entries, err := manager.Logs(started.ID, 0)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	if last := entries[len(entries)-1].Line; last != "handler exited: completed" {
		t.Errorf("last log line %q, want the handler exit entry", last)
	}
}

// TestEnvelope checks what dispatch receives: the executor, the deadline,
// and the arguments.
func TestEnvelope(t *testing.T) {
	d := newDispatcher()
	manager := newManager(t, d)

	rec, err := manager.Submit(operations.Request{
		Capability: "demo",
		Operation:  "quick",
		Arguments:  json.RawMessage(`{"target":"alpha"}`),
		Requester:  "tester",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if rec.Status != operations.StateCompleted {
		t.Fatalf("status %q, want completed", rec.Status)
	}
	if rec.Executor != fixtureNode || rec.ExecutorSource != "default" {
		t.Errorf("executor %q source %q, want %s default", rec.Executor, rec.ExecutorSource, fixtureNode)
	}
	call := <-d.started
	if call.Request == nil {
		t.Fatal("dispatch received no request envelope")
	}
	if call.Request.Contract != runtime.ContractV1 || call.Request.Kind != runtime.KindOperation {
		t.Errorf("envelope contract %q kind %q", call.Request.Contract, call.Request.Kind)
	}
	if call.Request.ID != rec.ID || call.Request.Capability != "demo" || call.Request.Operation != "quick" {
		t.Errorf("envelope identity %+v, want operation %s", call.Request, rec.ID)
	}
	if call.Request.Executor.ID != fixtureNode {
		t.Errorf("envelope executor %q, want %s", call.Request.Executor.ID, fixtureNode)
	}
	if string(call.Request.Arguments) != `{"target":"alpha"}` {
		t.Errorf("envelope arguments %s", call.Request.Arguments)
	}
	if call.Request.Deadline.IsZero() {
		t.Error("envelope has no deadline")
	}
	if call.Elevation != nil {
		t.Errorf("envelope elevation %v, want none", call.Elevation)
	}
}

// TestEnvelopeNodeObjects checks the full node object of the request
// envelope, handler contract section 2.1: the executor and every
// `hanse:node` target carry id, name, os, arch, address, and attributes.
func TestEnvelopeNodeObjects(t *testing.T) {
	d := newDispatcher()
	manager := newManager(t, d)

	rec, err := manager.Submit(operations.Request{
		Capability: "demo",
		Operation:  "probe",
		Arguments:  json.RawMessage(`{"node":"` + fixtureTarget + `"}`),
		Requester:  "tester",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if rec.Status != operations.StateCompleted {
		t.Fatalf("status %q, want completed", rec.Status)
	}
	call := <-d.started
	if call.Request == nil {
		t.Fatal("dispatch received no request envelope")
	}

	executor := call.Request.Executor
	wantExecutor := runtime.Node{
		ID:         fixtureNode,
		Name:       "Gateway host",
		OS:         "linux",
		Arch:       "amd64",
		Address:    "192.0.2.10",
		Attributes: map[string]any{"site": "lab"},
	}
	if !reflect.DeepEqual(executor, wantExecutor) {
		t.Errorf("executor node %+v, want %+v", executor, wantExecutor)
	}

	target, ok := call.Request.Nodes[fixtureTarget]
	if !ok {
		t.Fatalf("envelope nodes %v have no target %s", call.Request.Nodes, fixtureTarget)
	}
	wantTarget := runtime.Node{
		ID:         fixtureTarget,
		Name:       "Target machine",
		OS:         "linux",
		Arch:       "arm64",
		Address:    "192.0.2.11",
		Attributes: map[string]any{"mac": "00:00:5e:00:53:02"},
	}
	if !reflect.DeepEqual(target, wantTarget) {
		t.Errorf("target node %+v, want %+v", target, wantTarget)
	}
}

// TestValidate checks the dry-run checks without creating an operation.
func TestValidate(t *testing.T) {
	d := newDispatcher()
	manager := newManager(t, d)

	check, err := manager.Validate(submit("alpha", "", "tester"))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if check.Executor != fixtureNode || check.ExecutorSource != "default" {
		t.Errorf("check executor %q source %q", check.Executor, check.ExecutorSource)
	}
	if check.Mode != registry.ModeAsync {
		t.Errorf("check mode %q, want async", check.Mode)
	}
	if got, err := manager.List(operations.Filter{}); err != nil {
		t.Fatalf("list: %v", err)
	} else if len(got) != 0 {
		t.Errorf("validate stored %d operations, want none", len(got))
	}

	bad := submit("alpha", "", "tester")
	bad.Arguments = json.RawMessage(`{}`)
	if _, err := manager.Validate(bad); !errors.Is(err, herrors.InvalidArguments) {
		t.Fatalf("validate without the required argument: %v, want invalid_arguments", err)
	}
}

// TestListFilters checks the filter and the newest-first order.
func TestListFilters(t *testing.T) {
	d := newDispatcher()
	manager := newManager(t, d)

	first, err := manager.Submit(submit("alpha", "", "tester"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitFor(t, manager, first.ID, operations.StateCompleted)
	second, err := manager.Submit(submit("beta", "", "other"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitFor(t, manager, second.ID, operations.StateCompleted)

	all, err := manager.List(operations.Filter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("list returned %d operations, want 2", len(all))
	}
	if all[0].ID != second.ID || all[1].ID != first.ID {
		t.Errorf("list is not newest first: %s then %s, want %s then %s",
			all[0].ID, all[1].ID, second.ID, first.ID)
	}
	if all[0].CreatedAt.Before(all[1].CreatedAt) {
		t.Errorf("list is not sorted by creation time: %s then %s",
			all[0].CreatedAt, all[1].CreatedAt)
	}
	newest, err := manager.List(operations.Filter{Limit: 1})
	if err != nil {
		t.Fatalf("list with a limit: %v", err)
	}
	if len(newest) != 1 || newest[0].ID != second.ID {
		t.Errorf("list with a limit returned %v, want only %s", newest, second.ID)
	}
	byRequester, err := manager.List(operations.Filter{Requester: "other"})
	if err != nil {
		t.Fatalf("list by requester: %v", err)
	}
	if len(byRequester) != 1 || byRequester[0].ID != second.ID {
		t.Errorf("requester filter returned %+v, want %s", byRequester, second.ID)
	}
}

// failingReadStore wraps the memory store and fails every read, like a
// closed or corrupted database.
type failingReadStore struct {
	*operations.MemoryStore
	err error
}

func (s *failingReadStore) Get(string) (operations.Record, bool, error) {
	return operations.Record{}, false, s.err
}

func (s *failingReadStore) List(operations.Filter) ([]operations.Record, error) {
	return nil, s.err
}

func (s *failingReadStore) ByIdempotency(string, string) (operations.Record, bool, error) {
	return operations.Record{}, false, s.err
}

// TestReadFailureDoesNotRunTheOperation proves that a failed idempotency
// read is the Internal code and never looks like a new key, so a retry
// cannot run the state-changing operation twice.
func TestReadFailureDoesNotRunTheOperation(t *testing.T) {
	d := newDispatcher()
	backing := operations.NewMemoryStore()
	store := &failingReadStore{MemoryStore: backing, err: errors.New("the database is closed")}
	manager, err := operations.New(operations.Config{
		Registry:   newFixture(t),
		Dispatcher: d,
		Store:      store,
		Timeouts:   config.Timeouts{Operation: 5 * time.Second, GracePeriod: time.Second},
	})
	if err != nil {
		t.Fatalf("build manager: %v", err)
	}

	_, err = manager.Submit(submit("alpha", "key-1", "tester"))
	if !errors.Is(err, herrors.Internal) {
		t.Fatalf("submit with a failed idempotency read: %v, want internal", err)
	}
	if got := d.callCount(); got != 0 {
		t.Errorf("dispatcher ran %d times, want none", got)
	}
	if records, err := backing.List(operations.Filter{}); err != nil {
		t.Fatalf("list backing store: %v", err)
	} else if len(records) != 0 {
		t.Errorf("the failed read stored %d operations, want none", len(records))
	}

	// A failed read must not turn a known operation into a 404.
	if _, err := manager.Get("op_missing"); !errors.Is(err, herrors.Internal) {
		t.Errorf("get with a failed read: %v, want internal", err)
	}
}

// filterStore records the filters the manager passes to List.
type filterStore struct {
	*operations.MemoryStore
	mu      sync.Mutex
	filters []operations.Filter
}

func (s *filterStore) List(f operations.Filter) ([]operations.Record, error) {
	s.mu.Lock()
	s.filters = append(s.filters, f)
	s.mu.Unlock()
	return s.MemoryStore.List(f)
}

func (s *filterStore) lastFilter() operations.Filter {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.filters) == 0 {
		return operations.Filter{}
	}
	return s.filters[len(s.filters)-1]
}

// TestConflictReadsOnlyActiveRecords checks that the concurrency check asks
// the store for active records, and that a finished record with the same
// key does not block a new run.
func TestConflictReadsOnlyActiveRecords(t *testing.T) {
	d := newDispatcher()
	backing := operations.NewMemoryStore()
	finished := operations.Record{
		ID:             "op_finished",
		Capability:     "demo",
		Operation:      "run",
		Requester:      "tester",
		Status:         operations.StateCompleted,
		ConcurrencyKey: `["alpha"]`,
		CreatedAt:      time.Now().UTC().Add(-time.Hour),
		FinishedAt:     time.Now().UTC().Add(-time.Minute),
	}
	if err := backing.Insert(finished); err != nil {
		t.Fatalf("insert finished record: %v", err)
	}
	store := &filterStore{MemoryStore: backing}
	manager, err := operations.New(operations.Config{
		Registry:   newFixture(t),
		Dispatcher: d,
		Store:      store,
		Timeouts:   config.Timeouts{Operation: 5 * time.Second, GracePeriod: time.Second},
	})
	if err != nil {
		t.Fatalf("build manager: %v", err)
	}

	rec, err := manager.Submit(submit("alpha", "", "tester"))
	if err != nil {
		t.Fatalf("submit with only a finished record: %v", err)
	}
	if filter := store.lastFilter(); !filter.Active {
		t.Errorf("conflict list filter %+v, want Active", filter)
	}
	waitFor(t, manager, rec.ID, operations.StateCompleted)

	active, err := backing.List(operations.Filter{Capability: "demo", Operation: "run", Active: true})
	if err != nil {
		t.Fatalf("list active records: %v", err)
	}
	for _, record := range active {
		if record.ID == finished.ID {
			t.Errorf("active filter returned the finished record %s", finished.ID)
		}
	}
}

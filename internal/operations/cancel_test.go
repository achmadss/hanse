package operations

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/achmadss/hanse/internal/config"
	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/runtime"
)

// newTestManager builds the minimal manager the cancellation tests need:
// a store and a clock, with no registry or dispatcher.
func newTestManager() *Manager {
	return &Manager{
		store: NewMemoryStore(),
		now:   time.Now,
		live:  map[string]*execution{},
	}
}

// TestCancelQueued moves a queued operation to cancelled at once.
func TestCancelQueued(t *testing.T) {
	m := newTestManager()
	rec := Record{ID: "op_queued", Status: StateQueued, CreatedAt: time.Now().UTC()}
	if err := m.store.Insert(rec); err != nil {
		t.Fatalf("insert: %v", err)
	}
	exec := &execution{done: make(chan struct{})}
	m.live[rec.ID] = exec

	cancelled, err := m.Cancel(rec.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cancelled.Status != StateCancelled {
		t.Errorf("status %q, want cancelled", cancelled.Status)
	}
	if cancelled.FinishedAt.IsZero() {
		t.Error("cancelled operation has no finish time")
	}
	select {
	case <-exec.done:
	default:
		t.Error("the queued run's done channel is still open")
	}
	if _, ok := m.live[rec.ID]; ok {
		t.Error("the cancelled run is still live")
	}
}

// TestCancelFinalIsUnchanged leaves a finished operation as it is.
func TestCancelFinalIsUnchanged(t *testing.T) {
	m := newTestManager()
	rec := Record{ID: "op_done", Status: StateCompleted, FinishedAt: time.Now().UTC()}
	if err := m.store.Insert(rec); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := m.Cancel(rec.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got.Status != StateCompleted {
		t.Errorf("status %q, want completed", got.Status)
	}
}

// TestCancelUnknown reports an operation that does not exist.
func TestCancelUnknown(t *testing.T) {
	m := newTestManager()
	if _, err := m.Cancel("op_missing"); !errors.Is(err, herrors.UnknownOperation) {
		t.Fatalf("cancel unknown: %v, want unknown_operation", err)
	}
}

// blockingDispatcher waits for the run's context and reports a clean
// cancellation, like a cooperative handler.
type blockingDispatcher struct {
	started chan struct{}
}

// Execute signals that it started, then waits for the context.
func (d *blockingDispatcher) Execute(ctx context.Context, _ *Call) (runtime.Response, error) {
	d.started <- struct{}{}
	<-ctx.Done()
	return runtime.Cancelled(map[string]any{"stopped": true}, "stopped")
}

// updateHookStore runs a hook before every Update.
type updateHookStore struct {
	Store
	onUpdate func(Record)
}

// Update calls the hook, then stores the record.
func (s *updateHookStore) Update(rec Record) error {
	if s.onUpdate != nil {
		s.onUpdate(rec)
	}
	return s.Store.Update(rec)
}

// hasCancel reports whether the run's cancel function is registered.
func hasCancel(e *execution) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cancel != nil
}

// TestCancelRegisteredBeforeRunning is the regression test for a cancel
// that arrives as the run starts: the cancel function must already exist
// when the record becomes running, or the stop request is lost and the
// handler runs to completion.
func TestCancelRegisteredBeforeRunning(t *testing.T) {
	store := NewMemoryStore()
	rec := Record{ID: "op_start", Capability: "demo", Operation: "run", Status: StateQueued, CreatedAt: time.Now().UTC()}
	if err := store.Insert(rec); err != nil {
		t.Fatalf("insert: %v", err)
	}
	exec := &execution{args: []byte("{}"), done: make(chan struct{})}
	checked := make(chan bool, 1)
	hook := &updateHookStore{Store: store, onUpdate: func(updated Record) {
		if updated.Status == StateRunning {
			checked <- hasCancel(exec)
		}
	}}
	d := &blockingDispatcher{started: make(chan struct{})}
	m := &Manager{
		registry:   &registry.Registry{},
		dispatcher: d,
		timeouts:   config.Timeouts{Operation: time.Second, Options: time.Second, GracePeriod: time.Second},
		store:      hook,
		now:        time.Now,
		newID:      newOperationID,
		live:       map[string]*execution{rec.ID: exec},
	}
	op := &registry.Operation{ID: "run", Mode: registry.ModeAsync}
	go m.execute(exec, rec, op)
	<-d.started
	if registered := <-checked; !registered {
		t.Fatal("the record became running before its cancel function was registered")
	}

	cancelled, err := m.Cancel(rec.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cancelled.Status != StateCancelled {
		t.Fatalf("status %q, want cancelled", cancelled.Status)
	}
	if string(cancelled.Result) != `{"stopped":true}` {
		t.Errorf("result %s, want the partial result", cancelled.Result)
	}
}

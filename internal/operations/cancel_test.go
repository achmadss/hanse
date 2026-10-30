package operations

import (
	"errors"
	"testing"
	"time"

	herrors "github.com/achmadss/hanse/internal/errors"
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

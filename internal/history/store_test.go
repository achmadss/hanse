package history_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/achmadss/hanse/internal/history"
	"github.com/achmadss/hanse/internal/operations"
)

// fixedNow is the injected clock of the tests.
var fixedNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

// open opens a store over a fresh state directory.
func open(t *testing.T, opts history.Options) *history.Store {
	t.Helper()
	if opts.StateDir == "" {
		opts.StateDir = t.TempDir()
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return fixedNow }
	}
	store, err := history.Open(opts)
	if err != nil {
		t.Fatalf("open history store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// sample is a complete record of one run.
func sample(id string, status operations.State) operations.Record {
	rec := operations.Record{
		ID:                  id,
		Capability:          "demo",
		Operation:           "run",
		Requester:           "tester",
		Executor:            "node-a",
		ExecutorSource:      "default",
		Mode:                "async",
		Status:              status,
		CreatedAt:           fixedNow.Add(-time.Hour),
		StartedAt:           fixedNow.Add(-59 * time.Minute),
		IdempotencyKey:      "key-" + id,
		ArgumentFingerprint: "fingerprint-" + id,
		Arguments:           json.RawMessage(`{"target":"alpha"}`),
		ConcurrencyKey:      `["alpha"]`,
	}
	switch status {
	case operations.StateCompleted:
		rec.FinishedAt = fixedNow.Add(-time.Minute)
		rec.Result = json.RawMessage(`{"state":"online"}`)
		rec.Summary = "node-c is online"
	case operations.StateFailed:
		rec.FinishedAt = fixedNow.Add(-time.Minute)
		rec.Failure = &operations.Failure{
			Code:    "handler_failed",
			Message: "the handler failed",
			Details: map[string]any{"target": "alpha", "attempts": float64(2)},
		}
	case operations.StateCancelled:
		rec.FinishedAt = fixedNow.Add(-time.Minute)
		rec.Summary = "stopped"
	}
	return rec
}

// TestPersistenceAcrossReopen stores a record, closes the database, opens it
// again, and reads the same record back.
func TestPersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	first := open(t, history.Options{StateDir: dir})
	for _, status := range []operations.State{
		operations.StateCompleted, operations.StateFailed, operations.StateCancelled,
	} {
		rec := sample("op_"+string(status), status)
		if err := first.Insert(rec); err != nil {
			t.Fatalf("insert %s: %v", status, err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second := open(t, history.Options{StateDir: dir})
	for _, status := range []operations.State{
		operations.StateCompleted, operations.StateFailed, operations.StateCancelled,
	} {
		want := sample("op_"+string(status), status)
		got, ok := second.Get(want.ID)
		if !ok {
			t.Fatalf("operation %s did not survive the reopen", want.ID)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("operation %s = %+v, want %+v", want.ID, got, want)
		}
	}
}

// TestRestartRecovery marks the operations a previous process left queued
// or running as failed with gateway_restart, and leaves final records alone.
func TestRestartRecovery(t *testing.T) {
	dir := t.TempDir()
	first := open(t, history.Options{StateDir: dir})
	queued := sample("op_queued", operations.StateQueued)
	queued.StartedAt = time.Time{}
	queued.FinishedAt = time.Time{}
	running := sample("op_running", operations.StateRunning)
	running.FinishedAt = time.Time{}
	done := sample("op_done", operations.StateCompleted)
	for _, rec := range []operations.Record{queued, running, done} {
		if err := first.Insert(rec); err != nil {
			t.Fatalf("insert %s: %v", rec.ID, err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened := open(t, history.Options{StateDir: dir})
	for _, id := range []string{"op_queued", "op_running"} {
		rec, ok := reopened.Get(id)
		if !ok {
			t.Fatalf("operation %s is missing after the reopen", id)
		}
		if rec.Status != operations.StateFailed {
			t.Errorf("operation %s status %q, want failed", id, rec.Status)
		}
		if rec.Failure == nil || rec.Failure.Code != "gateway_restart" {
			t.Errorf("operation %s failure %+v, want gateway_restart", id, rec.Failure)
		}
		if !rec.FinishedAt.Equal(fixedNow) {
			t.Errorf("operation %s finished at %v, want %v", id, rec.FinishedAt, fixedNow)
		}
	}
	finished, _ := reopened.Get("op_done")
	if !reflect.DeepEqual(finished, done) {
		t.Errorf("a final record changed: %+v, want %+v", finished, done)
	}

	// A second recovery finds nothing left to change.
	if changed, err := reopened.RecoverInterrupted(fixedNow); err != nil || changed != 0 {
		t.Errorf("second recovery = %d, %v, want 0, nil", changed, err)
	}
}

// TestRetentionStartup removes finished records older than the retention
// when the store opens, and never removes an active one.
func TestRetentionStartup(t *testing.T) {
	dir := t.TempDir()
	old := fixedNow.Add(-72 * time.Hour)
	recent := fixedNow.Add(-time.Hour)

	first := open(t, history.Options{StateDir: dir})
	records := []operations.Record{
		sample("op_old_done", operations.StateCompleted),
		sample("op_recent_done", operations.StateCompleted),
		sample("op_old_active", operations.StateQueued),
	}
	records[0].FinishedAt = old
	records[1].FinishedAt = recent
	records[2].CreatedAt = old
	records[2].StartedAt = time.Time{}
	records[2].FinishedAt = time.Time{}
	for _, rec := range records {
		if err := first.Insert(rec); err != nil {
			t.Fatalf("insert %s: %v", rec.ID, err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second := open(t, history.Options{StateDir: dir, Retention: 24 * time.Hour})
	if _, ok := second.Get("op_old_done"); ok {
		t.Error("the old finished operation survived the startup cleanup")
	}
	if _, ok := second.Get("op_recent_done"); !ok {
		t.Error("the recent finished operation was removed")
	}
	// Open also recovers the active record, so it fails but stays stored.
	active, ok := second.Get("op_old_active")
	if !ok {
		t.Fatal("the active operation was removed by retention")
	}
	if active.Status != operations.StateFailed {
		t.Errorf("active operation status %q, want failed after recovery", active.Status)
	}
}

// TestRetentionPeriodic removes an old finished record while the store is
// open.
func TestRetentionPeriodic(t *testing.T) {
	store := open(t, history.Options{
		Retention:       time.Hour,
		CleanupInterval: 5 * time.Millisecond,
		Now:             func() time.Time { return fixedNow },
	})
	rec := sample("op_old_done", operations.StateCompleted)
	rec.FinishedAt = fixedNow.Add(-48 * time.Hour)
	if err := store.Insert(rec); err != nil {
		t.Fatalf("insert: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := store.Get(rec.ID); !ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the retention timer did not remove the old record")
}

// TestIdempotencyAfterReopen checks that a stored key still resolves after
// the database is closed and opened again.
func TestIdempotencyAfterReopen(t *testing.T) {
	dir := t.TempDir()
	first := open(t, history.Options{StateDir: dir})
	rec := sample("op_key", operations.StateCompleted)
	if err := first.Insert(rec); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second := open(t, history.Options{StateDir: dir})
	got, ok := second.ByIdempotency("tester", "key-op_key")
	if !ok || got.ID != rec.ID {
		t.Fatalf("idempotency lookup = %+v, %v, want %s", got, ok, rec.ID)
	}
	if _, ok := second.ByIdempotency("other", "key-op_key"); ok {
		t.Error("another requester resolved the idempotency key")
	}
	if _, ok := second.ByIdempotency("tester", ""); ok {
		t.Error("an empty idempotency key matched")
	}
}

// TestConcurrentUse exercises the store from many goroutines. Run it with
// -race to check the connection pool and the scans.
func TestConcurrentUse(t *testing.T) {
	store := open(t, history.Options{})
	const workers = 8
	const perWorker = 20
	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				id := "op_" + string(rune('a'+w)) + "_" + string(rune('a'+i))
				rec := sample(id, operations.StateQueued)
				rec.IdempotencyKey = "key-" + id
				rec.Arguments = json.RawMessage(`{"target":"alpha"}`)
				if err := store.Insert(rec); err != nil {
					errs <- err
					return
				}
				if _, ok := store.Get(id); !ok {
					t.Errorf("operation %s is missing after insert", id)
					return
				}
				if _, ok := store.ByIdempotency(rec.Requester, rec.IdempotencyKey); !ok {
					t.Errorf("idempotency key of %s is missing", id)
					return
				}
				rec.Status = operations.StateCompleted
				rec.FinishedAt = fixedNow
				if err := store.Update(rec); err != nil {
					errs <- err
					return
				}
				if got := store.List(operations.Filter{Requester: rec.Requester}); len(got) == 0 {
					t.Error("the list is empty while records exist")
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent store use: %v", err)
	}
}

// TestFilePermissions checks the restrictive modes of the database and the
// state directory on Unix.
func TestFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no POSIX mode bits")
	}
	dir := filepath.Join(t.TempDir(), "state")
	store := open(t, history.Options{StateDir: dir})
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat state directory: %v", err)
	}
	if mode := dirInfo.Mode().Perm(); mode != 0o700 {
		t.Errorf("state directory mode %o, want 700", mode)
	}
	fileInfo, err := os.Stat(store.Path())
	if err != nil {
		t.Fatalf("stat database: %v", err)
	}
	if mode := fileInfo.Mode().Perm(); mode != 0o600 {
		t.Errorf("database mode %o, want 600", mode)
	}
}

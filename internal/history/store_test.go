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
		got, ok, err := second.Get(want.ID)
		if err != nil {
			t.Fatalf("get %s: %v", want.ID, err)
		}
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
		rec, ok, err := reopened.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
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
	finished, found, err := reopened.Get("op_done")
	if err != nil || !found {
		t.Fatalf("get op_done: %v, found %v", err, found)
	}
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
	if _, ok, err := second.Get("op_old_done"); err != nil {
		t.Fatalf("get op_old_done: %v", err)
	} else if ok {
		t.Error("the old finished operation survived the startup cleanup")
	}
	if _, ok, err := second.Get("op_recent_done"); err != nil {
		t.Fatalf("get op_recent_done: %v", err)
	} else if !ok {
		t.Error("the recent finished operation was removed")
	}
	// Open also recovers the active record, so it fails but stays stored.
	active, ok, err := second.Get("op_old_active")
	if err != nil {
		t.Fatalf("get op_old_active: %v", err)
	}
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
		if _, ok, err := store.Get(rec.ID); err != nil {
			t.Fatalf("get %s: %v", rec.ID, err)
		} else if !ok {
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
	got, ok, err := second.ByIdempotency("tester", "key-op_key")
	if err != nil {
		t.Fatalf("idempotency lookup: %v", err)
	}
	if !ok || got.ID != rec.ID {
		t.Fatalf("idempotency lookup = %+v, %v, want %s", got, ok, rec.ID)
	}
	if _, ok, err := second.ByIdempotency("other", "key-op_key"); err != nil {
		t.Fatalf("idempotency lookup for another requester: %v", err)
	} else if ok {
		t.Error("another requester resolved the idempotency key")
	}
	if _, ok, err := second.ByIdempotency("tester", ""); err != nil {
		t.Fatalf("empty idempotency lookup: %v", err)
	} else if ok {
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
				if _, ok, err := store.Get(id); err != nil {
					errs <- err
					return
				} else if !ok {
					t.Errorf("operation %s is missing after insert", id)
					return
				}
				if _, ok, err := store.ByIdempotency(rec.Requester, rec.IdempotencyKey); err != nil {
					errs <- err
					return
				} else if !ok {
					t.Errorf("idempotency key of %s is missing", id)
					return
				}
				rec.Status = operations.StateCompleted
				rec.FinishedAt = fixedNow
				if err := store.Update(rec); err != nil {
					errs <- err
					return
				}
				got, err := store.List(operations.Filter{Requester: rec.Requester})
				if err != nil {
					errs <- err
					return
				}
				if len(got) == 0 {
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

// TestActiveFilterExcludesFinished proves the concurrency check can load
// only queued and running records, never finished history.
func TestActiveFilterExcludesFinished(t *testing.T) {
	store := open(t, history.Options{})
	finished := sample("op_finished", operations.StateCompleted)
	queued := sample("op_queued", operations.StateQueued)
	queued.StartedAt = time.Time{}
	running := sample("op_running", operations.StateRunning)
	running.FinishedAt = time.Time{}
	for _, rec := range []operations.Record{finished, queued, running} {
		if err := store.Insert(rec); err != nil {
			t.Fatalf("insert %s: %v", rec.ID, err)
		}
	}

	active, err := store.List(operations.Filter{Capability: "demo", Operation: "run", Active: true})
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	ids := make(map[string]bool, len(active))
	for _, rec := range active {
		ids[rec.ID] = true
	}
	if ids[finished.ID] {
		t.Error("the active filter returned a finished record")
	}
	if !ids[queued.ID] || !ids[running.ID] {
		t.Errorf("active filter returned %v, want the queued and running records", ids)
	}
}

// TestListOrderAndLimit checks that the store orders records newest first,
// by creation time and then ID, and that a limit keeps the newest ones.
func TestListOrderAndLimit(t *testing.T) {
	store := open(t, history.Options{})
	older := sample("op_aaa", operations.StateCompleted)
	older.CreatedAt = fixedNow.Add(-2 * time.Hour)
	middle := sample("op_mmm", operations.StateCompleted)
	middle.CreatedAt = fixedNow.Add(-time.Hour)
	newest := sample("op_zzz", operations.StateCompleted)
	newest.CreatedAt = fixedNow.Add(-time.Minute)
	for _, rec := range []operations.Record{older, middle, newest} {
		if err := store.Insert(rec); err != nil {
			t.Fatalf("insert %s: %v", rec.ID, err)
		}
	}

	all, err := store.List(operations.Filter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{newest.ID, middle.ID, older.ID}
	if len(all) != len(want) {
		t.Fatalf("list returned %d records, want %d", len(all), len(want))
	}
	for i, id := range want {
		if all[i].ID != id {
			t.Errorf("record %d is %s, want %s", i, all[i].ID, id)
		}
	}

	limited, err := store.List(operations.Filter{Limit: 2})
	if err != nil {
		t.Fatalf("list with a limit: %v", err)
	}
	if len(limited) != 2 || limited[0].ID != newest.ID || limited[1].ID != middle.ID {
		t.Errorf("list with a limit returned %v, want the two newest records", limited)
	}
}

// TestTimestampOrdering checks that a whole-second timestamp and a
// fractional one sort by time, so retention deletes the older record.
func TestTimestampOrdering(t *testing.T) {
	start := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	cutoff := start.Add(-2 * time.Hour)
	store := open(t, history.Options{
		Retention: 2 * time.Hour,
		Now:       func() time.Time { return start },
	})

	older := sample("op_older", operations.StateCompleted)
	older.FinishedAt = cutoff.Add(-time.Second) // whole second, before the cutoff
	newer := sample("op_newer", operations.StateCompleted)
	newer.FinishedAt = cutoff.Add(500 * time.Millisecond) // fractional, after the cutoff
	for _, rec := range []operations.Record{older, newer} {
		if err := store.Insert(rec); err != nil {
			t.Fatalf("insert %s: %v", rec.ID, err)
		}
	}

	removed, err := store.PurgeOld(start)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if removed != 1 {
		t.Errorf("purge removed %d records, want 1", removed)
	}
	if _, ok, err := store.Get(older.ID); err != nil {
		t.Fatalf("get older: %v", err)
	} else if ok {
		t.Error("the record before the cutoff survived")
	}
	if _, ok, err := store.Get(newer.ID); err != nil {
		t.Fatalf("get newer: %v", err)
	} else if !ok {
		t.Error("the record after the cutoff was removed")
	}
}

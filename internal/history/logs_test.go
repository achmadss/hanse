package history_test

import (
	"testing"
	"time"

	"github.com/achmadss/hanse/internal/history"
	"github.com/achmadss/hanse/internal/operations"
)

// TestLogSequenceAndPersistence stores log lines, reopens the database, and
// reads the same lines with the same sequence numbers.
func TestLogSequenceAndPersistence(t *testing.T) {
	dir := t.TempDir()
	store := open(t, history.Options{StateDir: dir})

	const id = "op_logs"
	lines := []string{"executor resolved", "handler started", "line 1"}
	for i, line := range lines {
		stream := operations.StreamSystem
		if i == len(lines)-1 {
			stream = operations.StreamStderr
		}
		entry, err := store.AppendLog(id, operations.LogEntry{
			Time:   fixedNow.Add(time.Duration(i) * time.Second),
			Stream: stream,
			Line:   line,
		})
		if err != nil {
			t.Fatalf("append log line %d: %v", i, err)
		}
		if entry.Seq != int64(i+1) {
			t.Fatalf("line %d has sequence %d, want %d", i, entry.Seq, i+1)
		}
	}

	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened := open(t, history.Options{StateDir: dir})

	all, err := reopened.Logs(id, 0)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	if len(all) != len(lines) {
		t.Fatalf("read %d lines, want %d", len(all), len(lines))
	}
	for i, entry := range all {
		if entry.Seq != int64(i+1) || entry.Line != lines[i] {
			t.Errorf("line %d is %d/%q, want %d/%q", i, entry.Seq, entry.Line, i+1, lines[i])
		}
		if entry.Time.IsZero() {
			t.Errorf("line %d has no timestamp", i)
		}
	}
	if all[2].Stream != operations.StreamStderr {
		t.Errorf("last line has stream %q, want stderr", all[2].Stream)
	}

	after, err := reopened.Logs(id, 2)
	if err != nil {
		t.Fatalf("read logs after 2: %v", err)
	}
	if len(after) != 1 || after[0].Seq != 3 {
		t.Errorf("resume after 2 returned %v, want the third line only", after)
	}

	none, err := reopened.Logs(id, 99)
	if err != nil {
		t.Fatalf("read logs after 99: %v", err)
	}
	if none == nil {
		t.Error("an empty read returned nil, want an empty list")
	}
	if len(none) != 0 {
		t.Errorf("read %d lines after the last one, want none", len(none))
	}
}

// TestRetentionRemovesLogs removes the log of an operation with the
// operation itself, at the configured retention.
func TestRetentionRemovesLogs(t *testing.T) {
	store := open(t, history.Options{
		StateDir:  t.TempDir(),
		Retention: time.Hour,
	})

	old := sample("op_old", operations.StateCompleted)
	old.FinishedAt = fixedNow.Add(-48 * time.Hour)
	if err := store.Insert(old); err != nil {
		t.Fatalf("insert old operation: %v", err)
	}
	if _, err := store.AppendLog(old.ID, operations.LogEntry{Line: "an old line"}); err != nil {
		t.Fatalf("append old log line: %v", err)
	}

	fresh := sample("op_fresh", operations.StateCompleted)
	if err := store.Insert(fresh); err != nil {
		t.Fatalf("insert fresh operation: %v", err)
	}
	if _, err := store.AppendLog(fresh.ID, operations.LogEntry{Line: "a fresh line"}); err != nil {
		t.Fatalf("append fresh log line: %v", err)
	}

	removed, err := store.PurgeOld(fixedNow)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if removed != 1 {
		t.Errorf("purged %d operations, want 1", removed)
	}
	oldLines, err := store.Logs(old.ID, 0)
	if err != nil {
		t.Fatalf("read old logs: %v", err)
	}
	if len(oldLines) != 0 {
		t.Errorf("the purged operation kept %d log lines", len(oldLines))
	}
	freshLines, err := store.Logs(fresh.ID, 0)
	if err != nil {
		t.Fatalf("read fresh logs: %v", err)
	}
	if len(freshLines) != 1 {
		t.Errorf("the kept operation has %d log lines, want 1", len(freshLines))
	}
}

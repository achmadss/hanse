package operations_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/achmadss/hanse/internal/config"
	"github.com/achmadss/hanse/internal/operations"
	"github.com/achmadss/hanse/internal/runtime"
)

// logDispatcher writes lines through the call's log writer. When proceed is
// set it emits the first line, reports that through first, and then waits
// for proceed, so a follower can join the run between two lines.
type logDispatcher struct {
	lines   int
	first   chan struct{}
	proceed chan struct{}
}

func newLogDispatcher(lines int) *logDispatcher {
	return &logDispatcher{
		lines:   lines,
		first:   make(chan struct{}),
		proceed: make(chan struct{}),
	}
}

// newImmediateLogDispatcher logs every line without waiting, for tests that
// only read a finished run.
func newImmediateLogDispatcher(lines int) *logDispatcher {
	return &logDispatcher{lines: lines}
}

// Execute logs its lines and returns a successful response.
func (d *logDispatcher) Execute(_ context.Context, call *operations.Call) (runtime.Response, error) {
	for i := 1; i <= d.lines; i++ {
		if call.Log != nil {
			call.Log(fmt.Sprintf("line %d", i))
		}
		if i == 1 && d.proceed != nil {
			close(d.first)
			<-d.proceed
		}
	}
	return runtime.OK(map[string]any{"lines": d.lines}, "logged")
}

// logManager builds a manager over the fixture with the given log limit.
func logManager(t *testing.T, dispatcher operations.Dispatcher, limit int) *operations.Manager {
	t.Helper()
	manager, err := operations.New(operations.Config{
		Registry:   newFixture(t),
		Dispatcher: dispatcher,
		LogLimit:   limit,
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

// TestFollowMidRunHasNoGapsOrDuplicates covers the first acceptance
// criterion: a follower that subscribes after the run started gets the
// earlier lines from the backlog and then the live lines, each exactly
// once, in sequence order.
func TestFollowMidRunHasNoGapsOrDuplicates(t *testing.T) {
	dispatcher := newLogDispatcher(5)
	manager := logManager(t, dispatcher, 0)

	rec, err := manager.Submit(submit("alpha", "", "tester"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	<-dispatcher.first

	sub, backlog, err := manager.Follow(rec.ID, 0)
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	defer sub.Close()
	if len(backlog) == 0 {
		t.Fatal("the follower joined mid-run but the backlog is empty")
	}

	close(dispatcher.proceed)
	<-manager.Done(rec.ID)
	live := sub.Drain()

	entries := append(append([]operations.LogEntry{}, backlog...), live...)
	if len(entries) == 0 {
		t.Fatal("the follower received no lines")
	}
	for i, entry := range entries {
		if entry.Seq != int64(i+1) {
			t.Fatalf("entry %d has sequence %d, want %d", i, entry.Seq, i+1)
		}
	}
	lines := make([]string, 0, len(entries))
	counts := map[string]int{}
	for _, entry := range entries {
		lines = append(lines, entry.Line)
		counts[entry.Line]++
	}
	for i := 1; i <= dispatcher.lines; i++ {
		line := fmt.Sprintf("line %d", i)
		if counts[line] != 1 {
			t.Errorf("line %q appears %d times, want once; lines: %v", line, counts[line], lines)
		}
	}
	if lines[0] != "executor resolved: node-a (default)" {
		t.Errorf("first line %q, want the executor resolution", lines[0])
	}
	if !contains(lines, "handler started") {
		t.Errorf("lines %v have no handler start", lines)
	}
	if last := lines[len(lines)-1]; last != "handler exited: completed" {
		t.Errorf("last line %q, want the handler exit", last)
	}
}

// TestFollowResumesAfterSequence covers the second acceptance criterion at
// the sink: a follower that names the last line it saw gets the next line
// and no earlier one.
func TestFollowResumesAfterSequence(t *testing.T) {
	manager := logManager(t, newImmediateLogDispatcher(3), 0)

	rec, err := manager.Submit(submit("alpha", "", "tester"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	<-manager.Done(rec.ID)

	all, err := manager.Logs(rec.ID, 0)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	if len(all) < 3 {
		t.Fatalf("stored %d lines, want at least 3", len(all))
	}
	const lastSeen = 2

	sub, backlog, err := manager.Follow(rec.ID, lastSeen)
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	defer sub.Close()
	entries := append(append([]operations.LogEntry{}, backlog...), sub.Drain()...)
	if len(entries) != len(all)-lastSeen {
		t.Fatalf("resume returned %d lines, want %d", len(entries), len(all)-lastSeen)
	}
	if entries[0].Seq != lastSeen+1 {
		t.Errorf("resume starts at sequence %d, want %d", entries[0].Seq, lastSeen+1)
	}
	for i, entry := range entries {
		want := all[lastSeen+i]
		if entry.Seq != want.Seq || entry.Line != want.Line {
			t.Errorf("resumed entry %d is %d/%q, want %d/%q",
				i, entry.Seq, entry.Line, want.Seq, want.Line)
		}
	}
}

// TestLogTotalTruncatesOnce covers limits.log_total: once the cap is
// reached the run stores one system entry that says so, and no later line.
func TestLogTotalTruncatesOnce(t *testing.T) {
	const limit = 40
	manager := logManager(t, newImmediateLogDispatcher(20), limit)

	rec, err := manager.Submit(submit("alpha", "", "tester"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	<-manager.Done(rec.ID)

	entries, err := manager.Logs(rec.ID, 0)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	truncations := 0
	truncationIndex := -1
	for i, entry := range entries {
		if strings.Contains(entry.Line, "log limit reached") {
			truncations++
			truncationIndex = i
			if entry.Stream != operations.StreamSystem {
				t.Errorf("the truncation entry has stream %q, want system", entry.Stream)
			}
		}
	}
	if truncations != 1 {
		t.Fatalf("stored %d truncation entries, want 1; lines: %v", truncations, entries)
	}
	if truncationIndex != len(entries)-1 {
		t.Errorf("a line follows the truncation entry: %v", entries)
	}
	total := 0
	for i, entry := range entries {
		if i == truncationIndex {
			continue
		}
		total += len(entry.Line)
	}
	if total > limit {
		t.Errorf("stored %d bytes before the truncation entry, want no more than %d", total, limit)
	}
}

// TestLogLinesKeepOrderAndStream checks that builtin handler lines travel as
// `system` entries and keep the order the handler wrote them in.
func TestLogLinesKeepOrderAndStream(t *testing.T) {
	manager := logManager(t, newImmediateLogDispatcher(3), 0)
	rec, err := manager.Submit(submit("alpha", "", "tester"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	<-manager.Done(rec.ID)

	entries, err := manager.Logs(rec.ID, 0)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	want := []string{"line 1", "line 2", "line 3"}
	var got []string
	for _, entry := range entries {
		if entry.Stream != operations.StreamSystem {
			t.Errorf("entry %d has stream %q, want system", entry.Seq, entry.Stream)
		}
		if strings.HasPrefix(entry.Line, "line ") {
			got = append(got, entry.Line)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("handler lines %v, want %v", got, want)
	}
}

// TestLogsOfUnknownOperationAreEmpty keeps a read of an operation that never
// logged anything an empty list, never an error.
func TestLogsOfUnknownOperationAreEmpty(t *testing.T) {
	manager := logManager(t, newImmediateLogDispatcher(1), 0)
	entries, err := manager.Logs("op_missing", 0)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("read %d lines of an unknown operation, want none", len(entries))
	}
}

// TestCancelLogsSystemEntry records the cancellation as one system entry.
func TestCancelLogsSystemEntry(t *testing.T) {
	dispatcher := newDispatcher()
	dispatcher.release = make(chan struct{})
	manager := logManager(t, dispatcher, 0)

	rec, err := manager.Submit(submit("alpha", "", "tester"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitFor(t, manager, rec.ID, operations.StateRunning)
	if _, err := manager.Cancel(rec.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	<-manager.Done(rec.ID)

	entries, err := manager.Logs(rec.ID, 0)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	if !contains(lineOf(entries), "cancelled: stop requested by client") {
		t.Errorf("lines %v have no cancellation entry", lineOf(entries))
	}
}

// timeoutDispatcher waits for the run's deadline and reports it, so the
// manager records the timeout state.
type timeoutDispatcher struct{}

// Execute waits for the context and returns its error.
func (timeoutDispatcher) Execute(ctx context.Context, _ *operations.Call) (runtime.Response, error) {
	<-ctx.Done()
	return runtime.Response{}, ctx.Err()
}

// TestTimeoutLogsSystemEntry records the timeout as one system entry.
func TestTimeoutLogsSystemEntry(t *testing.T) {
	manager, err := operations.New(operations.Config{
		Registry:   newFixture(t),
		Dispatcher: timeoutDispatcher{},
		Timeouts: config.Timeouts{
			Operation:   20 * time.Millisecond,
			GracePeriod: time.Second,
		},
	})
	if err != nil {
		t.Fatalf("build manager: %v", err)
	}
	rec, err := manager.Submit(submit("alpha", "", "tester"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	<-manager.Done(rec.ID)

	entries, err := manager.Logs(rec.ID, 0)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	if !contains(lineOf(entries), "timeout: the operation exceeded its deadline") {
		t.Errorf("lines %v have no timeout entry", lineOf(entries))
	}
}

// lineOf returns the text of every entry.
func lineOf(entries []operations.LogEntry) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.Line)
	}
	return out
}

// contains reports whether lines holds want.
func contains(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

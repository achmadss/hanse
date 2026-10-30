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
	"github.com/achmadss/hanse/internal/runtime"
)

// logLeakDispatcher logs the secret values it received, as a careless
// handler would, so the test can prove the Gateway redacts them.
type logLeakDispatcher struct{}

// Execute logs its lines and returns a successful response.
func (logLeakDispatcher) Execute(_ context.Context, call *operations.Call) (runtime.Response, error) {
	call.Log("token " + secretToken + " key " + secretKey)
	call.Log("elevation password " + secretPassword)
	return runtime.OK(map[string]any{"state": "online"}, "finished")
}

// TestLogsRedactBeforeSplitting covers a secret that crosses a
// limits.log_line boundary: redaction runs first, so no chunk holds any part
// of the secret, and every chunk still fits the limit.
func TestLogsRedactBeforeSplitting(t *testing.T) {
	const lineLimit = 40
	store := open(t, history.Options{StateDir: t.TempDir()})
	// The secret starts at byte 35 and the boundary is at byte 40, so a
	// split before redaction would cut the secret in two.
	prefix := strings.Repeat("x", 35)
	dispatcher := lineLeakDispatcher{line: prefix + secretToken}
	manager, err := operations.New(operations.Config{
		Registry:     secretRegistry(t),
		Dispatcher:   dispatcher,
		Store:        store,
		LogStore:     store,
		LogLineLimit: lineLimit,
	})
	if err != nil {
		t.Fatalf("build manager: %v", err)
	}

	arguments, err := json.Marshal(map[string]any{
		"target":  "alpha",
		"token":   secretToken,
		"ssh_key": secretKey,
		"note":    "nothing secret",
	})
	if err != nil {
		t.Fatalf("marshal arguments: %v", err)
	}
	rec, err := manager.Submit(operations.Request{
		Capability: "demo",
		Operation:  "secret",
		Arguments:  arguments,
		Requester:  "tester",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	entries, err := store.Logs(rec.ID, 0)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	var joined strings.Builder
	chunks := 0
	for _, entry := range entries {
		// The manager's own system lines fit the limit, so every other
		// entry belongs to the split handler line.
		if strings.HasPrefix(entry.Line, "executor resolved:") ||
			strings.HasPrefix(entry.Line, "handler started") ||
			strings.HasPrefix(entry.Line, "handler exited:") {
			continue
		}
		chunks++
		if len(entry.Line) > lineLimit {
			t.Errorf("chunk %q is %d bytes, want at most %d", entry.Line, len(entry.Line), lineLimit)
		}
		joined.WriteString(entry.Line)
	}
	if chunks < 2 {
		t.Fatalf("one long line produced %d chunks, want a split", chunks)
	}
	if got := joined.String(); got != prefix+"[redacted]" {
		t.Errorf("the chunks join to %q, want the redacted line", got)
	}
	if strings.Contains(joined.String(), secretToken) {
		t.Errorf("the split leak left the secret: %q", joined.String())
	}
}

// lineLeakDispatcher logs one fixed line, as a careless handler would.
type lineLeakDispatcher struct{ line string }

// Execute logs its line and returns a successful response.
func (d lineLeakDispatcher) Execute(_ context.Context, call *operations.Call) (runtime.Response, error) {
	call.Log(d.line)
	return runtime.OK(map[string]any{"state": "online"}, "finished")
}

// TestLogsRedactSecretsInTheDatabase covers the third acceptance
// criterion: a known secret value never appears in the stored or streamed
// log. The stored log is read back and every database file is scanned.
func TestLogsRedactSecretsInTheDatabase(t *testing.T) {
	stateDir := t.TempDir()
	store := open(t, history.Options{StateDir: stateDir})

	manager, err := operations.New(operations.Config{
		Registry:   secretRegistry(t),
		Dispatcher: logLeakDispatcher{},
		Store:      store,
		LogStore:   store,
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
		Operation:  "secret",
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

	entries, err := store.Logs(rec.ID, 0)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the operation stored no log lines")
	}
	var stored strings.Builder
	redacted := false
	for _, entry := range entries {
		stored.WriteString(entry.Line)
		stored.WriteString("\n")
		if strings.Contains(entry.Line, "[redacted]") {
			redacted = true
		}
	}
	for name, secret := range map[string]string{
		"token argument":     secretToken,
		"secret argument":    secretKey,
		"elevation password": secretPassword,
	} {
		if strings.Contains(stored.String(), secret) {
			t.Errorf("the stored log holds the %s: %s", name, stored.String())
		}
	}
	if !redacted {
		t.Errorf("the stored log has no redaction placeholder: %s", stored.String())
	}

	// The follower path hands the same entries to a stream, so a resume
	// cannot bring an unredacted line back.
	sub, backlog, err := manager.Follow(rec.ID, 0)
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	defer sub.Close()
	var streamed strings.Builder
	for _, entry := range append(append([]operations.LogEntry{}, backlog...), sub.Drain()...) {
		streamed.WriteString(entry.Line)
		streamed.WriteString("\n")
	}
	for name, secret := range map[string]string{
		"token argument":     secretToken,
		"secret argument":    secretKey,
		"elevation password": secretPassword,
	} {
		if strings.Contains(streamed.String(), secret) {
			t.Errorf("the followed log holds the %s: %s", name, streamed.String())
		}
	}

	// The database file itself must not hold them either.
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

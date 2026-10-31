package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fixtures below follow internal/api/logs.go. Every value is a
// placeholder: the addresses of the notes come from 192.0.2.0/24.

// fixtureLogsBody is the stored log of every fixture operation.
const fixtureLogsBody = `{"logs":[
  {"seq":1,"ts":"2026-10-31T08:00:01Z","stream":"system","line":"executor resolved: node-a"},
  {"seq":2,"ts":"2026-10-31T08:00:02Z","stream":"stderr","line":"waking node-c"}
]}`

// logsGateway serves the operations and the log endpoint for the logs
// command fixtures. It records the follow requests and the Last-Event-ID
// header of each, so a test can prove a resume happened and that the
// operation was never cancelled.
type logsGateway struct {
	mu             sync.Mutex
	followRequests int
	lastEventIDs   []string
	cancels        int
}

// newLogsGateway starts a Gateway fixture for the logs command.
func newLogsGateway(t *testing.T) (*httptest.Server, *logsGateway) {
	t.Helper()
	gw := &logsGateway{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"authentication_failed","message":"the token is wrong"}`))
			return
		}
		switch {
		case r.URL.Path == "/v1/version":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fixtureVersionBody))
		case strings.HasSuffix(r.URL.Path, "/cancel"):
			gw.mu.Lock()
			gw.cancels++
			gw.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"operation_conflict","message":"the fixture never cancels"}`))
		case strings.HasSuffix(r.URL.Path, "/logs") && r.Method == http.MethodGet:
			gw.serveLogs(w, r)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"unknown_operation","message":"operation does not exist"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, gw
}

// serveLogs answers the stored log, or one connection of a followed log.
func (g *logsGateway) serveLogs(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/operations/"), "/logs")
	query := r.URL.Query()
	if query.Get("follow") != "true" && query.Get("stream") == "true" {
		// The non-follow read of the CLI uses the event stream, so a log
		// larger than the JSON response cap still arrives whole.
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeFixtureSSE(w, 1, "log", `{"seq":1,"ts":"2026-10-31T08:00:01Z","stream":"system","line":"executor resolved: node-a"}`)
		writeFixtureSSE(w, 2, "log", `{"seq":2,"ts":"2026-10-31T08:00:02Z","stream":"stderr","line":"waking node-c"}`)
		writeFixtureSSE(w, 0, "status", fmt.Sprintf(`{"operation_id":%q,"status":"completed"}`, id))
		return
	}
	if query.Get("follow") != "true" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fixtureLogsBody))
		return
	}
	g.mu.Lock()
	g.followRequests++
	g.lastEventIDs = append(g.lastEventIDs, r.Header.Get("Last-Event-ID"))
	attempt := len(g.lastEventIDs)
	g.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	switch id {
	case "op_fail":
		writeFixtureSSE(w, 1, "log", fixtureLogLine(1, "the handler exited nonzero"))
		writeFixtureSSE(w, 0, "status", `{"operation_id":"op_fail","status":"failed",`+
			`"error":{"code":"handler_exit","message":"the handler exited nonzero","details":{"exit_code":1}}}`)
	case "op_cancelled":
		writeFixtureSSE(w, 1, "log", fixtureLogLine(1, "cancelling operation"))
		writeFixtureSSE(w, 0, "status", `{"operation_id":"op_cancelled","status":"cancelled",`+
			`"error":{"code":"cancelled","message":"cancelled by the client"}}`)
	case "op_handler_code":
		writeFixtureSSE(w, 1, "log", fixtureLogLine(1, "the audit log is unreadable"))
		writeFixtureSSE(w, 0, "status", `{"operation_id":"op_handler_code","status":"failed",`+
			`"error":{"code":"audit_unreadable","message":"the audit log is unreadable",`+
			`"details":{"path":"/var/log/audit.log"}}}`)
	case "op_drop":
		if attempt == 1 {
			// Drop the connection after one line, as a proxy timeout
			// would, and let the next attempt resume.
			writeFixtureSSE(w, 1, "log", fixtureLogLine(1, "first"))
			return
		}
		writeFixtureSSE(w, 1, "log", fixtureLogLine(1, "first"))
		writeFixtureSSE(w, 2, "log", fixtureLogLine(2, "second"))
		writeFixtureSSE(w, 0, "status", `{"operation_id":"op_drop","status":"completed"}`)
	case "op_quiet":
		writeFixtureSSE(w, 1, "log", fixtureLogLine(1, "first"))
		<-r.Context().Done()
	default:
		writeFixtureSSE(w, 1, "log", fixtureLogLine(1, "waking node-c"))
		writeFixtureSSE(w, 2, "log", fixtureLogLine(2, "node-c answered"))
		writeFixtureSSE(w, 0, "status", fmt.Sprintf(`{"operation_id":%q,"status":"completed"}`, id))
	}
}

// fixtureLogLine renders one log event body.
func fixtureLogLine(seq int64, line string) string {
	return fmt.Sprintf(`{"seq":%d,"ts":"2026-10-31T08:00:0%dZ","stream":"stderr","line":%q}`, seq, seq, line)
}

// writeFixtureSSE writes one event and flushes it.
func writeFixtureSSE(w http.ResponseWriter, id int64, event, data string) {
	if id > 0 {
		_, _ = fmt.Fprintf(w, "id: %d\n", id)
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// newLogsConfig starts the fixture and writes the CLI configuration.
func newLogsConfig(t *testing.T) (*logsGateway, string) {
	t.Helper()
	srv, gw := newLogsGateway(t)
	configPath := filepath.Join(t.TempDir(), "hanse", "config.json")
	writeTestConfig(t, configPath, srv.URL, testToken)
	return gw, configPath
}

// TestLogsStoredText prints the stored lines with a stream prefix.
func TestLogsStoredText(t *testing.T) {
	_, configPath := newLogsConfig(t)

	stdout, stderr, code := runCLI(t, []string{"logs", "op_done"}, configPath)
	if code != 0 {
		t.Fatalf("logs: exit code %d, stderr %s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("logs: unexpected stderr %q", stderr)
	}
	for _, want := range []string{
		"system: executor resolved: node-a\n",
		"stderr: waking node-c\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("logs: output does not contain %q:\n%s", want, stdout)
		}
	}
}

// TestLogsStoredJSON requires one JSON object per line with the stream, the
// line, the time, and the sequence number.
func TestLogsStoredJSON(t *testing.T) {
	_, configPath := newLogsConfig(t)

	stdout, stderr, code := runCLI(t, []string{"logs", "op_done", "--format", "json"}, configPath)
	if code != 0 {
		t.Fatalf("logs: exit code %d, stderr %s", code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("logs: got %d lines, want one object per line:\n%s", len(lines), stdout)
	}
	want := []struct {
		stream string
		line   string
		id     int64
	}{
		{"system", "executor resolved: node-a", 1},
		{"stderr", "waking node-c", 2},
	}
	for i, line := range lines {
		var object struct {
			Stream string    `json:"stream"`
			Line   string    `json:"line"`
			Time   time.Time `json:"time"`
			ID     int64     `json:"id"`
		}
		if err := json.Unmarshal([]byte(line), &object); err != nil {
			t.Fatalf("logs: line %d is not JSON: %v (%s)", i, err, line)
		}
		if object.Stream != want[i].stream || object.Line != want[i].line || object.ID != want[i].id {
			t.Errorf("logs: line %d %+v, want %+v", i, object, want[i])
		}
		if object.Time.IsZero() {
			t.Errorf("logs: line %d has no time", i)
		}
	}
}

// TestLogsFollowExitsZero requires a followed log to exit 0 for every final
// state: the command is a query, and only run maps the state to an exit code.
func TestLogsFollowExitsZero(t *testing.T) {
	for _, id := range []string{"op_done", "op_fail", "op_cancelled", "op_handler_code"} {
		t.Run(id, func(t *testing.T) {
			_, configPath := newLogsConfig(t)
			stdout, stderr, code := runCLI(t, []string{"logs", id, "-f"}, configPath)
			if code != 0 {
				t.Fatalf("logs -f: exit code %d, want 0; stderr %s", code, stderr)
			}
			if stderr != "" {
				t.Errorf("logs -f: unexpected stderr %q", stderr)
			}
			if !strings.Contains(stdout, "stderr: ") {
				t.Errorf("logs -f: output has no log line:\n%s", stdout)
			}
		})
	}
}

// TestLogsFollowJSON requires NDJSON with a final object for the terminal
// state.
func TestLogsFollowJSON(t *testing.T) {
	_, configPath := newLogsConfig(t)

	stdout, stderr, code := runCLI(t, []string{"logs", "op_fail", "-f", "--format", "json"}, configPath)
	if code != 0 {
		t.Fatalf("logs -f: exit code %d, want 0; stderr %s", code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("logs -f: got %d lines, want a line and a status:\n%s", len(lines), stdout)
	}
	var line struct {
		Stream string `json:"stream"`
		Line   string `json:"line"`
		ID     int64  `json:"id"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &line); err != nil {
		t.Fatalf("logs -f: first line is not JSON: %v", err)
	}
	if line.Stream != "stderr" || line.Line != "the handler exited nonzero" || line.ID != 1 {
		t.Errorf("logs -f: first line %+v", line)
	}
	var status struct {
		OperationID string `json:"operation_id"`
		Status      string `json:"status"`
		Error       *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &status); err != nil {
		t.Fatalf("logs -f: the last line is not JSON: %v", err)
	}
	if status.OperationID != "op_fail" || status.Status != "failed" {
		t.Errorf("logs -f: status %+v", status)
	}
	if status.Error == nil || status.Error.Code != "handler_exit" {
		t.Errorf("logs -f: status error %+v, want handler_exit", status.Error)
	}
}

// TestLogsFollowCapabilityErrorCode requires the final status object of a
// followed log to keep a capability-defined code, message, and details. The
// command exits 0 because the query succeeded.
func TestLogsFollowCapabilityErrorCode(t *testing.T) {
	_, configPath := newLogsConfig(t)

	stdout, stderr, code := runCLI(t, []string{"logs", "op_handler_code", "-f", "--format", "json"}, configPath)
	if code != 0 {
		t.Fatalf("logs -f: exit code %d, want 0; stderr %s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("logs -f: unexpected stderr %q", stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) == 0 {
		t.Fatal("logs -f: no output")
	}
	var status struct {
		Status string `json:"status"`
		Error  *struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &status); err != nil {
		t.Fatalf("logs -f: the last line is not JSON: %v", err)
	}
	if status.Status != "failed" || status.Error == nil {
		t.Fatalf("logs -f: status %+v", status)
	}
	if status.Error.Code != "audit_unreadable" || status.Error.Message != "the audit log is unreadable" {
		t.Errorf("logs -f: error %+v, want the handler's code and message", status.Error)
	}
	if status.Error.Details["path"] != "/var/log/audit.log" {
		t.Errorf("logs -f: details %v, want the handler's details", status.Error.Details)
	}
}

// TestLogsFollowReconnect drops the connection mid-stream and requires the
// resume to carry Last-Event-ID, with no duplicate and no gap.
func TestLogsFollowReconnect(t *testing.T) {
	gw, configPath := newLogsConfig(t)

	stdout, stderr, code := runCLI(t, []string{"logs", "op_drop", "-f"}, configPath)
	if code != 0 {
		t.Fatalf("logs -f: exit code %d, stderr %s", code, stderr)
	}
	if got := strings.Count(stdout, "first"); got != 1 {
		t.Errorf("logs -f: the first line appears %d times, want once:\n%s", got, stdout)
	}
	if !strings.Contains(stdout, "second") {
		t.Errorf("logs -f: the resumed line is missing:\n%s", stdout)
	}

	gw.mu.Lock()
	defer gw.mu.Unlock()
	if gw.followRequests != 2 {
		t.Errorf("logs -f: %d follow requests, want 2", gw.followRequests)
	}
	if len(gw.lastEventIDs) != 2 || gw.lastEventIDs[0] != "" || gw.lastEventIDs[1] != "1" {
		t.Errorf("logs -f: Last-Event-ID values %q, want [\"\" \"1\"]", gw.lastEventIDs)
	}
}

// TestLogsFollowInterrupted proves Ctrl+C stops the following only, exits
// 130, and leaves the operation running.
func TestLogsFollowInterrupted(t *testing.T) {
	gw, configPath := newLogsConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(50*time.Millisecond, cancel)

	var out, errOut bytes.Buffer
	code := Run(ctx, []string{"logs", "op_quiet", "-f"}, Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &out,
		Stderr:     &errOut,
		ConfigPath: configPath,
	})
	if code != 130 {
		t.Fatalf("logs -f: exit code %d, want 130; stderr %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "first") {
		t.Errorf("logs -f: the streamed line is missing:\n%s", out.String())
	}
	if !strings.Contains(errOut.String(), "op_quiet") || !strings.Contains(errOut.String(), "still running") {
		t.Errorf("logs -f: stderr %q, want the operation ID and its state", errOut.String())
	}
	gw.mu.Lock()
	cancels := gw.cancels
	gw.mu.Unlock()
	if cancels != 0 {
		t.Errorf("logs -f: %d cancel requests, want none", cancels)
	}
}

// TestLogsStoredLargerThanResponseCap prints a stored log larger than the
// client's 8 MiB response cap. The JSON form of the endpoint would exceed
// the cap, so the command reads the event stream instead
// (docs/architecture.md section 10).
func TestLogsStoredLargerThanResponseCap(t *testing.T) {
	const (
		lineCount = 600
		lineBytes = 16 << 10
		capBytes  = 8 << 20
	)
	line := strings.Repeat("x", lineBytes)
	var stored strings.Builder
	stored.WriteString(`{"logs":[`)
	for i := 1; i <= lineCount; i++ {
		if i > 1 {
			stored.WriteByte(',')
		}
		fmt.Fprintf(&stored,
			`{"seq":%d,"ts":"2026-10-31T08:00:01Z","stream":"stderr","line":%q}`, i, line)
	}
	stored.WriteString(`]}`)
	if stored.Len() <= capBytes {
		t.Fatalf("the fixture log has %d bytes, want more than the %d byte cap", stored.Len(), capBytes)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"authentication_failed","message":"the token is wrong"}`))
			return
		}
		switch {
		case r.URL.Path == "/v1/version":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fixtureVersionBody))
		case strings.HasSuffix(r.URL.Path, "/logs"):
			if r.URL.Query().Get("stream") != "true" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(stored.String()))
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			for i := 1; i <= lineCount; i++ {
				writeFixtureSSE(w, int64(i), "log", fmt.Sprintf(
					`{"seq":%d,"ts":"2026-10-31T08:00:01Z","stream":"stderr","line":%q}`, i, line))
			}
			writeFixtureSSE(w, 0, "status", `{"operation_id":"op_big","status":"completed"}`)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"unknown_operation","message":"operation does not exist"}`))
		}
	}))
	t.Cleanup(srv.Close)
	configPath := filepath.Join(t.TempDir(), "hanse", "config.json")
	writeTestConfig(t, configPath, srv.URL, testToken)

	stdout, stderr, code := runCLI(t, []string{"logs", "op_big"}, configPath)
	if code != 0 {
		t.Fatalf("logs: exit code %d, stderr %s", code, stderr)
	}
	if len(stdout) <= capBytes {
		t.Errorf("printed %d bytes, want the whole %d byte log", len(stdout), stored.Len())
	}
	if got := strings.Count(stdout, "stderr: "); got != lineCount {
		t.Errorf("printed %d lines, want %d", got, lineCount)
	}
}

// TestLogsErrors maps Gateway errors and usage mistakes to the documented
// exit codes.
func TestLogsErrors(t *testing.T) {
	_, configPath := newLogsConfig(t)

	cases := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"missing id", []string{"logs"}, 2, "usage: hanse logs"},
		{"two ids", []string{"logs", "op_done", "op_fail"}, 2, "usage: hanse logs"},
		{"hostile id", []string{"logs", "../version"}, 22, "not a valid identifier"},
		{"hostile follow id", []string{"logs", "../version", "-f"}, 22, "not a valid identifier"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := runCLI(t, tc.args, configPath)
			if code != tc.code {
				t.Fatalf("%v: exit code %d, want %d; stderr %s", tc.args, code, tc.code, stderr)
			}
			if stdout != "" {
				t.Errorf("%v: stdout %q, want no output", tc.args, stdout)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("%v: stderr %q, want %q", tc.args, stderr, tc.want)
			}
		})
	}
}

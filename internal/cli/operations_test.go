package cli

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// The fixtures below follow internal/api/execution.go. Every value is a
// placeholder.

const fixtureOperationRunning = `{
  "id": "op_01J9Z3",
  "capability": "pc",
  "operation": "up",
  "requester": "alice",
  "executor": "node-a",
  "executor_source": "route",
  "status": "running",
  "mode": "async",
  "created_at": "2026-10-30T08:00:00Z",
  "started_at": "2026-10-30T08:00:01Z",
  "summary": "waking node-c"
}`

const fixtureOperationCompleted = `{
  "id": "op_01J9Z4",
  "capability": "demo",
  "operation": "clean",
  "requester": "bob",
  "executor": "node-b",
  "executor_source": "default",
  "status": "completed",
  "mode": "sync",
  "created_at": "2026-10-30T07:00:00Z",
  "started_at": "2026-10-30T07:00:01Z",
  "finished_at": "2026-10-30T07:00:02Z",
  "result": {
    "removed": 2
  },
  "summary": "removed 2 files"
}`

const fixtureOperationFailed = `{
  "id": "op_01J9Z6",
  "capability": "service",
  "operation": "restart",
  "requester": "carol",
  "executor": "node-c",
  "executor_source": "explicit",
  "status": "failed",
  "mode": "async",
  "created_at": "2026-10-30T06:00:00Z",
  "started_at": "2026-10-30T06:00:01Z",
  "finished_at": "2026-10-30T06:00:03Z",
  "summary": "the handler exited nonzero",
  "error": {
    "code": "handler_exit",
    "message": "the handler exited nonzero",
    "details": {
      "exit_code": 1
    }
  }
}`

// fixtureOperationCapabilityCode is a failed operation whose final-state
// code belongs to the capability, not to the shared set of internal/errors.
const fixtureOperationCapabilityCode = `{
  "id": "op_01J9Z7",
  "capability": "audit",
  "operation": "sweep",
  "requester": "dave",
  "executor": "node-d",
  "executor_source": "default",
  "status": "failed",
  "mode": "sync",
  "created_at": "2026-10-30T05:00:00Z",
  "started_at": "2026-10-30T05:00:01Z",
  "finished_at": "2026-10-30T05:00:02Z",
  "summary": "the audit log is unreadable",
  "error": {
    "code": "audit_unreadable",
    "message": "the audit log is unreadable",
    "details": {
      "path": "/var/log/audit.log"
    }
  }
}`

const fixtureOperationCancelled = `{
  "id": "op_01J9Z3",
  "capability": "pc",
  "operation": "up",
  "requester": "alice",
  "executor": "node-a",
  "executor_source": "route",
  "status": "cancelled",
  "mode": "async",
  "created_at": "2026-10-30T08:00:00Z",
  "started_at": "2026-10-30T08:00:01Z",
  "finished_at": "2026-10-30T08:00:05Z",
  "summary": "cancelled by the client"
}`

// operationsGateway records the query of every list request, so a test can
// prove the CLI forwarded its filters.
type operationsGateway struct {
	mu      sync.Mutex
	queries []string
}

// remember stores one list query.
func (g *operationsGateway) remember(r *http.Request) {
	g.mu.Lock()
	g.queries = append(g.queries, r.URL.Query().Encode())
	g.mu.Unlock()
}

// reset forgets the recorded queries.
func (g *operationsGateway) reset() {
	g.mu.Lock()
	g.queries = nil
	g.mu.Unlock()
}

// last returns the last recorded list query.
func (g *operationsGateway) last() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.queries) == 0 {
		return ""
	}
	return g.queries[len(g.queries)-1]
}

// newOperationsGateway starts a Gateway fixture for the operations
// commands. The list endpoint answers a status filter only for the states
// the fixture holds.
func newOperationsGateway(t *testing.T) (*httptest.Server, *operationsGateway) {
	t.Helper()
	gw := &operationsGateway{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"authentication_failed","message":"the token is wrong"}`))
			return
		}
		switch {
		case r.URL.Path == "/v1/version":
			_, _ = w.Write([]byte(fixtureVersionBody))
		case r.URL.Path == "/v1/operations" && r.Method == http.MethodGet:
			gw.remember(r)
			switch r.URL.Query().Get("status") {
			case "":
				_, _ = w.Write([]byte(`{"operations":[` + fixtureOperationRunning + `,` + fixtureOperationCompleted + `]}`))
			case "completed":
				_, _ = w.Write([]byte(`{"operations":[` + fixtureOperationCompleted + `]}`))
			default:
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(`{"code":"invalid_arguments","message":"the status filter is not a known operation state"}`))
			}
		case r.URL.Path == "/v1/operations/op_01J9Z3":
			_, _ = w.Write([]byte(`{"operation":` + fixtureOperationRunning + `}`))
		case r.URL.Path == "/v1/operations/op_01J9Z4":
			_, _ = w.Write([]byte(`{"operation":` + fixtureOperationCompleted + `}`))
		case r.URL.Path == "/v1/operations/op_01J9Z6":
			_, _ = w.Write([]byte(`{"operation":` + fixtureOperationFailed + `}`))
		case r.URL.Path == "/v1/operations/op_01J9Z7":
			_, _ = w.Write([]byte(`{"operation":` + fixtureOperationCapabilityCode + `}`))
		case r.URL.Path == "/v1/operations/op_01J9Z3/cancel" && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(fixtureOperationCancelled))
		case r.URL.Path == "/v1/operations/op_01J9Z4/cancel" && r.Method == http.MethodPost:
			// A finished operation is returned unchanged.
			_, _ = w.Write([]byte(fixtureOperationCompleted))
		case r.URL.Path == "/v1/operations/op_01J9Z5/cancel" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"operation_conflict","message":"the operation finished before the cancel request"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"unknown_operation","message":"operation does not exist"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, gw
}

// TestOperationsGoldenJSON covers the JSON output of every operations
// command.
func TestOperationsGoldenJSON(t *testing.T) {
	srv, gw := newOperationsGateway(t)
	configPath := filepath.Join(t.TempDir(), "hanse", "config.json")
	writeTestConfig(t, configPath, srv.URL, testToken)

	cases := []struct {
		golden    string
		args      []string
		wantQuery string
		wantCode  int
	}{
		{
			golden:    "operations",
			args:      []string{"operations"},
			wantQuery: "",
		},
		{
			golden: "operations_filtered",
			args: []string{
				"operations", "--capability", "demo", "--operation", "clean",
				"--executor", "node-b", "--status", "completed", "--requester", "bob",
			},
			wantQuery: "capability=demo&executor=node-b&operation=clean&requester=bob&status=completed",
		},
		{
			// A known final-state code keeps its own exit code.
			golden:    "operation",
			args:      []string{"operations", "show", "op_01J9Z6"},
			wantQuery: "",
			wantCode:  38,
		},
		{
			golden:    "operation_cancel",
			args:      []string{"operations", "cancel", "op_01J9Z3"},
			wantQuery: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.golden, func(t *testing.T) {
			gw.reset()
			args := append(append([]string{}, tc.args...), "--format", "json")
			stdout, stderr, code := runCLI(t, args, configPath)
			if code != tc.wantCode {
				t.Fatalf("%v: exit code %d, want %d; stderr %s", args, code, tc.wantCode, stderr)
			}
			if tc.wantCode == 0 && stderr != "" {
				t.Errorf("%v: unexpected stderr %q", args, stderr)
			}
			if strings.Contains(stdout, "\x1b[") {
				t.Errorf("%v: JSON output contains a terminal escape", args)
			}
			if got := gw.last(); got != tc.wantQuery {
				t.Errorf("%v: list query %q, want %q", args, got, tc.wantQuery)
			}
			checkGolden(t, tc.golden, []byte(stdout))
		})
	}
}

// TestOperationsTextOutput covers the default format.
func TestOperationsTextOutput(t *testing.T) {
	srv, _ := newOperationsGateway(t)
	configPath := filepath.Join(t.TempDir(), "hanse", "config.json")
	writeTestConfig(t, configPath, srv.URL, testToken)

	cases := []struct {
		name     string
		args     []string
		want     []string
		wantCode int
	}{
		{
			name: "list",
			args: []string{"operations"},
			want: []string{"STATUS", "op_01J9Z3", "running", "node-a", "2026-10-30T08:00:00Z", "completed"},
		},
		{
			name: "show with error",
			args: []string{"operations", "show", "op_01J9Z6"},
			want: []string{"Status:", "failed", "Code:", "handler_exit", "Message:", "2026-10-30T06:00:03Z"},
			// The known code keeps its own exit code.
			wantCode: 38,
		},
		{
			name: "show with result",
			args: []string{"operations", "show", "op_01J9Z4"},
			want: []string{"Result:", `"removed": 2`, "Summary:", "removed 2 files"},
		},
		{
			name: "cancel",
			args: []string{"operations", "cancel", "op_01J9Z3"},
			want: []string{"Status:", "cancelled", "2026-10-30T08:00:05Z"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := runCLI(t, tc.args, configPath)
			if code != tc.wantCode {
				t.Fatalf("%v: exit code %d, want %d; stderr %s", tc.args, code, tc.wantCode, stderr)
			}
			if strings.Contains(stdout, "\x1b[") {
				t.Errorf("%v: text output contains a terminal escape", tc.args)
			}
			for _, want := range tc.want {
				if !strings.Contains(stdout, want) {
					t.Errorf("%v: output does not contain %q:\n%s", tc.args, want, stdout)
				}
			}
		})
	}
}

// TestOperationsShowCapabilityErrorCode requires a failed operation whose
// final-state code is not one of the shared codes to exit with
// handler_failed, while the record still shows the handler's own code,
// message, and details.
func TestOperationsShowCapabilityErrorCode(t *testing.T) {
	srv, _ := newOperationsGateway(t)
	configPath := filepath.Join(t.TempDir(), "hanse", "config.json")
	writeTestConfig(t, configPath, srv.URL, testToken)

	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			stdout, stderr, code := runCLI(t, []string{"operations", "show", "op_01J9Z7", "--format", format}, configPath)
			if code != 33 {
				t.Fatalf("show: exit code %d, want 33; stderr %s", code, stderr)
			}
			for _, want := range []string{"audit_unreadable", "the audit log is unreadable", "/var/log/audit.log"} {
				if !strings.Contains(stdout, want) {
					t.Errorf("show: output does not contain %q:\n%s", want, stdout)
				}
			}
			if !strings.Contains(stderr, "handler_failed") || !strings.Contains(stderr, "audit_unreadable") {
				t.Errorf("show: stderr %q, want handler_failed and the handler's code", stderr)
			}
		})
	}
}

// TestOperationsCancelFinished requires a cancel of a finished operation to
// report the record's final state without an error.
func TestOperationsCancelFinished(t *testing.T) {
	srv, _ := newOperationsGateway(t)
	configPath := filepath.Join(t.TempDir(), "hanse", "config.json")
	writeTestConfig(t, configPath, srv.URL, testToken)

	stdout, stderr, code := runCLI(t, []string{"operations", "cancel", "op_01J9Z4", "--format", "json"}, configPath)
	if code != 0 {
		t.Fatalf("cancel: exit code %d, stderr %s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("cancel: unexpected stderr %q", stderr)
	}
	if !strings.Contains(stdout, `"status": "completed"`) {
		t.Errorf("cancel: output\n%s\nwant the completed state", stdout)
	}
}

// TestOperationsErrors maps Gateway errors and usage mistakes to the
// documented exit codes.
func TestOperationsErrors(t *testing.T) {
	srv, _ := newOperationsGateway(t)
	configPath := filepath.Join(t.TempDir(), "hanse", "config.json")
	writeTestConfig(t, configPath, srv.URL, testToken)

	cases := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"unknown operation", []string{"operations", "show", "op_99ZZZZ"}, 21, "unknown_operation"},
		{"unknown cancel", []string{"operations", "cancel", "op_99ZZZZ"}, 21, "unknown_operation"},
		{"operation conflict", []string{"operations", "cancel", "op_01J9Z5"}, 32, "operation_conflict"},
		{"unknown status filter", []string{"operations", "--status", "bogus"}, 22, "invalid_arguments"},
		{"hostile id", []string{"operations", "show", "../version"}, 22, "not a valid identifier"},
		{"missing id", []string{"operations", "show"}, 2, "usage: hanse operations"},
		{"unknown subcommand", []string{"operations", "bogus"}, 2, "usage: hanse operations"},
		{"filter on show", []string{"operations", "show", "op_01J9Z3", "--status", "running"}, 2, "list only"},
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

// TestOperationsInvalidIDMakesNoRequest requires a hostile ID to fail
// before the CLI contacts the Gateway.
func TestOperationsInvalidIDMakesNoRequest(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/version") {
			requests.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fixtureVersionBody))
	}))
	t.Cleanup(srv.Close)
	configPath := filepath.Join(t.TempDir(), "hanse", "config.json")
	writeTestConfig(t, configPath, srv.URL, testToken)

	for _, id := range []string{"../version", "a/b", ".."} {
		_, stderr, code := runCLI(t, []string{"operations", "cancel", id}, configPath)
		if code != 22 {
			t.Fatalf("cancel %q: exit code %d, want 22; stderr %s", id, code, stderr)
		}
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("sent %d requests with a hostile identifier, want 0", got)
	}
}

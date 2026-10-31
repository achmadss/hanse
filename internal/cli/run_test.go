package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fixture below serves an operation of a capability the CLI has never
// heard of. Every value is a placeholder: addresses come from 192.0.2.0/24,
// MACs from 00:00:5e:00:53:xx, and the passwords are obviously fake.

const runTestPassword = "placeholder-elevation-password"

// runSchema is the schema of hvac.set: one argument of every type a flag can
// carry, one object that needs JSON input, and an enum.
const runSchema = `{
  "additionalProperties": false,
  "properties": {
    "force": {
      "description": "Override the zone lock",
      "type": "boolean"
    },
    "level": {
      "description": "The fan level",
      "type": "integer"
    },
    "mode": {
      "default": "auto",
      "description": "The mode",
      "enum": [
        "auto",
        "cool",
        "heat"
      ],
      "type": "string"
    },
    "retry": {
      "description": "The retry policy",
      "properties": {
        "count": {
          "type": "integer"
        }
      },
      "type": "object"
    },
    "tags": {
      "description": "Labels for this run",
      "items": {
        "type": "string"
      },
      "type": "array"
    },
    "threshold": {
      "description": "The target temperature",
      "type": "number"
    },
    "zone": {
      "description": "The zone to set",
      "format": "hanse:node",
      "type": "string"
    }
  },
  "required": [
    "zone"
  ],
  "type": "object"
}`

// runDescribe is the describe response of hvac.set.
func runDescribe(privileged bool) string {
	return fmt.Sprintf(`{
  "capability": "hvac",
  "operation": "set",
  "description": "Set one zone of the building",
  "mode": "async",
  "privileged": %t,
  "schema": %s,
  "required": [
    "zone"
  ],
  "options": [
    {
      "field": "zone",
      "provider": "nodes"
    }
  ],
  "node_filters": {},
  "executors": [
    {
      "id": "node-a",
      "status": "online",
      "default": true
    }
  ],
  "default_executor": "node-a",
  "executor_required": false
}`, privileged, runSchema)
}

// runFixture is a Gateway fixture for run. It records every submit and
// answers the status polls from a queue of records; the last queued record
// repeats.
type runFixture struct {
	mu        sync.Mutex
	submits   []map[string]any
	polls     []string
	pollCount int
	cancels   int
	afterRun  func()
}

// newRunFixture starts a Gateway for run. privileged marks hvac.set as
// privileged, and polls are the records the status endpoint answers in
// order.
func newRunFixture(t *testing.T, privileged bool, polls ...string) (*runFixture, string) {
	t.Helper()
	fixture := &runFixture{polls: polls}
	describeBody := runDescribe(privileged)
	nodesBody := `{
  "nodes": [
    {"id": "node-a", "os": "linux", "arch": "amd64", "roles": ["server"], "attributes": {}, "enabled": true, "status": "online"},
    {"id": "loft", "os": "linux", "arch": "arm64", "roles": ["zone"], "attributes": {}, "enabled": true, "status": "offline"}
  ]
}`
	optionsBody := `{"options": [{"value": "loft"}, {"value": "node-a"}]}`
	capabilitiesBody := `{
  "capabilities": [
    {
      "id": "hvac",
      "name": "HVAC",
      "description": "Set zones",
      "contract": "hanse.handler/v1",
      "handler": {"kind": "exec"},
      "operations": [
        {"id": "set", "description": "Set one zone of the building", "mode": "async", "privileged": false}
      ]
    }
  ]
}`
	capabilityBody := `{
  "capability": {
    "id": "hvac",
    "name": "HVAC",
    "description": "Set zones",
    "contract": "hanse.handler/v1",
    "handler": {"kind": "exec"},
    "operations": [
      {"id": "set", "description": "Set one zone of the building", "mode": "async", "privileged": false}
    ]
  }
}`
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
		case r.URL.Path == "/v1/capabilities":
			_, _ = w.Write([]byte(capabilitiesBody))
		case r.URL.Path == "/v1/capabilities/hvac":
			_, _ = w.Write([]byte(capabilityBody))
		case r.URL.Path == "/v1/nodes":
			_, _ = w.Write([]byte(nodesBody))
		case r.URL.Path == "/v1/operations/hvac/set/describe":
			_, _ = w.Write([]byte(describeBody))
		case r.URL.Path == "/v1/operations/hvac/set/options/zone":
			_, _ = w.Write([]byte(optionsBody))
		case r.URL.Path == "/v1/operations" && r.Method == http.MethodPost:
			fixture.recordSubmit(t, w, r)
		case strings.HasPrefix(r.URL.Path, "/v1/operations/") && strings.HasSuffix(r.URL.Path, "/cancel"):
			fixture.mu.Lock()
			fixture.cancels++
			fixture.mu.Unlock()
			_, _ = w.Write([]byte(runRecord("op_hvac_1", "cancelled", "")))
		case strings.HasPrefix(r.URL.Path, "/v1/operations/") && r.Method == http.MethodGet:
			fixture.answerPoll(t, w)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"unknown_capability","message":"capability does not exist"}`))
		}
	}))
	t.Cleanup(srv.Close)
	configPath := filepath.Join(t.TempDir(), "hanse", "config.json")
	writeTestConfig(t, configPath, srv.URL, testToken)
	return fixture, configPath
}

// recordSubmit stores the request body and answers the submit the way the
// Gateway answers an asynchronous operation.
func (f *runFixture) recordSubmit(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"invalid_request","message":"the request body is malformed"}`))
		return
	}
	f.mu.Lock()
	f.submits = append(f.submits, body)
	f.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(runRecord("op_hvac_1", "running", "")))
	if f.afterRun != nil {
		f.afterRun()
	}
}

// answerPoll answers one status read from the queue.
func (f *runFixture) answerPoll(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	f.mu.Lock()
	index := f.pollCount
	if index >= len(f.polls) {
		index = len(f.polls) - 1
	}
	f.pollCount++
	body := runRecord("op_hvac_1", "running", "")
	if len(f.polls) > 0 {
		body = f.polls[index]
	}
	f.mu.Unlock()
	// GET /v1/operations/{id} answers with the envelope of
	// docs/architecture.md section 11.1.
	_, _ = w.Write([]byte(`{"operation":` + body + `}`))
}

// submitted returns the arguments of the one request the fixture received.
func (f *runFixture) submitted(t *testing.T) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.submits) != 1 {
		t.Fatalf("the fixture received %d submit requests, want 1", len(f.submits))
	}
	return f.submits[0]
}

// submitCount returns how many submit requests the fixture received.
func (f *runFixture) submitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.submits)
}

// runRecord builds an operation record body of the given status.
func runRecord(id, status, extra string) string {
	return `{"id": "` + id + `", "capability": "hvac", "operation": "set", ` +
		`"requester": "alice", "executor": "node-a", "executor_source": "default", ` +
		`"status": "` + status + `", "mode": "async", "created_at": "2026-10-31T08:00:00Z", ` +
		`"started_at": "2026-10-31T08:00:01Z", "summary": "setting one zone"` + extra + `}`
}

// completedRecord is a finished record with a result.
const completedRunRecord = `{"id": "op_hvac_1", "capability": "hvac", "operation": "set", ` +
	`"requester": "alice", "executor": "node-a", "executor_source": "default", ` +
	`"status": "completed", "mode": "async", "created_at": "2026-10-31T08:00:00Z", ` +
	`"started_at": "2026-10-31T08:00:01Z", "finished_at": "2026-10-31T08:00:02Z", ` +
	`"result": {"temperature": 21.5}, "summary": "set zone loft"}`

// TestRunNewCapability proves the acceptance criterion of the issue: a
// capability the CLI has no code for runs from its fetched schema alone.
func TestRunNewCapability(t *testing.T) {
	fixture, configPath := newRunFixture(t, false, completedRunRecord)

	stdout, stderr, code := runCLI(t, []string{
		"run", "hvac", "set",
		"--zone", "loft",
		"--mode", "cool",
		"--level", "3",
		"--threshold", "21.5",
		"--force",
		"--tags", "morning",
		"--tags", "quiet",
		"--args-json", `{"retry":{"count":2},"level":9,"zone":"wrong"}`,
	}, configPath)
	if code != 0 {
		t.Fatalf("run: exit code %d, stderr %s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("run: unexpected stderr %q", stderr)
	}

	submit := fixture.submitted(t)
	if submit["capability"] != "hvac" || submit["operation"] != "set" {
		t.Errorf("run: submitted %v", submit)
	}
	if submit["executor"] != nil {
		t.Errorf("run: executor %v, want none", submit["executor"])
	}
	if _, ok := submit["elevation"]; ok {
		t.Errorf("run: the request carries elevation for a public operation")
	}
	wantArguments := map[string]any{
		"zone":      "loft",
		"mode":      "cool",
		"level":     float64(3),
		"threshold": 21.5,
		"force":     true,
		"tags":      []any{"morning", "quiet"},
		"retry":     map[string]any{"count": float64(2)},
	}
	if !sameJSON(submit["arguments"], wantArguments) {
		t.Errorf("run: arguments %#v, want %#v (an explicit flag overrides --args-json)",
			submit["arguments"], wantArguments)
	}

	// The command waits by default and prints the finished record.
	for _, want := range []string{"op_hvac_1", "completed", "temperature", "21.5"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("run: output does not contain %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "\x1b[") {
		t.Errorf("run: output contains a terminal escape")
	}
}

// TestRunJSONOutput covers the deterministic JSON result of a waited
// operation.
func TestRunJSONOutput(t *testing.T) {
	_, configPath := newRunFixture(t, false, completedRunRecord)

	stdout, stderr, code := runCLI(t, []string{
		"run", "hvac", "set", "--zone", "loft", "--format", "json",
	}, configPath)
	if code != 0 {
		t.Fatalf("run: exit code %d, stderr %s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("run: unexpected stderr %q", stderr)
	}
	var envelope struct {
		Operation struct {
			ID     string         `json:"id"`
			Status string         `json:"status"`
			Result map[string]any `json:"result"`
		} `json:"operation"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("run: output is not JSON: %v (%s)", err, stdout)
	}
	if envelope.Operation.ID != "op_hvac_1" || envelope.Operation.Status != "completed" {
		t.Errorf("run: operation %+v", envelope.Operation)
	}
	if envelope.Operation.Result["temperature"] != 21.5 {
		t.Errorf("run: result %v", envelope.Operation.Result)
	}
}

// TestRunDetach requires the operation ID at once, with no status read.
func TestRunDetach(t *testing.T) {
	fixture, configPath := newRunFixture(t, false, completedRunRecord)

	stdout, stderr, code := runCLI(t, []string{
		"run", "hvac", "set", "--zone", "loft", "--detach",
	}, configPath)
	if code != 0 {
		t.Fatalf("run: exit code %d, stderr %s", code, stderr)
	}
	if strings.TrimSpace(stdout) != "op_hvac_1" {
		t.Errorf("run --detach: stdout %q, want the operation ID", stdout)
	}
	fixture.mu.Lock()
	polls := fixture.pollCount
	fixture.mu.Unlock()
	if polls != 0 {
		t.Errorf("run --detach: %d status reads, want none", polls)
	}
}

// TestRunExecutorFlag requires --executor in the request.
func TestRunExecutorFlag(t *testing.T) {
	fixture, configPath := newRunFixture(t, false, completedRunRecord)

	_, stderr, code := runCLI(t, []string{
		"run", "hvac", "set", "--zone", "loft", "--executor", "node-b",
	}, configPath)
	if code != 0 {
		t.Fatalf("run: exit code %d, stderr %s", code, stderr)
	}
	if got := fixture.submitted(t)["executor"]; got != "node-b" {
		t.Errorf("run: executor %v, want node-b", got)
	}
}

// TestRunArgumentsFile covers --args-file, with "-" reading standard input.
func TestRunArgumentsFile(t *testing.T) {
	_, configPath := newRunFixture(t, false, completedRunRecord)

	path := filepath.Join(t.TempDir(), "arguments.json")
	if err := os.WriteFile(path, []byte(`{"zone":"loft","level":2}`), 0o600); err != nil {
		t.Fatalf("write arguments file: %v", err)
	}
	_, stderr, code := runCLI(t, []string{
		"run", "hvac", "set", "--args-file", path, "--detach",
	}, configPath)
	if code != 0 {
		t.Fatalf("run --args-file: exit code %d, stderr %s", code, stderr)
	}

	var out, errOut bytes.Buffer
	code = Run(context.Background(), []string{
		"run", "hvac", "set", "--args-file", "-", "--detach",
	}, Options{
		Stdin:      strings.NewReader(`{"zone":"loft"}`),
		Stdout:     &out,
		Stderr:     &errOut,
		ConfigPath: configPath,
	})
	if code != 0 {
		t.Fatalf("run --args-file -: exit code %d, stderr %s", code, errOut.String())
	}
	if strings.TrimSpace(out.String()) != "op_hvac_1" {
		t.Errorf("run --args-file -: stdout %q, want the operation ID", out.String())
	}
}

// TestRunRejectedArguments covers the checks that fail before a request.
func TestRunRejectedArguments(t *testing.T) {
	fixture, configPath := newRunFixture(t, false, completedRunRecord)

	cases := []struct {
		name   string
		args   []string
		code   int
		stderr string
	}{
		{
			name:   "missing required",
			args:   []string{"run", "hvac", "set", "--level", "3"},
			code:   23,
			stderr: `"zone" is missing`,
		},
		{
			name:   "enum",
			args:   []string{"run", "hvac", "set", "--zone", "loft", "--mode", "warm"},
			code:   2,
			stderr: "must be one of: auto, cool, heat",
		},
		{
			name:   "integer",
			args:   []string{"run", "hvac", "set", "--zone", "loft", "--level", "1.5"},
			code:   2,
			stderr: "must be an integer",
		},
		{
			name:   "number",
			args:   []string{"run", "hvac", "set", "--zone", "loft", "--threshold", "warm"},
			code:   2,
			stderr: "must be a number",
		},
		{
			name:   "boolean",
			args:   []string{"run", "hvac", "set", "--zone", "loft", "--force=yes"},
			code:   2,
			stderr: "must be true or false",
		},
		{
			name:   "unknown flag",
			args:   []string{"run", "hvac", "set", "--zone", "loft", "--znoe", "loft"},
			code:   2,
			stderr: "znoe",
		},
		{
			name:   "both argument sources",
			args:   []string{"run", "hvac", "set", "--zone", "loft", "--args-json", `{}`, "--args-file", "x"},
			code:   2,
			stderr: "not both",
		},
		{
			name:   "usage",
			args:   []string{"run"},
			code:   2,
			stderr: "usage: hanse run",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := runCLI(t, tc.args, configPath)
			if code != tc.code {
				t.Fatalf("%v: exit code %d, want %d; stderr %s", tc.args, code, tc.code, stderr)
			}
			if !strings.Contains(stderr, tc.stderr) {
				t.Errorf("%v: stderr %q, want %q", tc.args, stderr, tc.stderr)
			}
			if stdout != "" {
				t.Errorf("%v: stdout %q, want no output", tc.args, stdout)
			}
		})
	}
	if got := fixture.submitCount(); got != 0 {
		t.Errorf("%d submit requests reached the Gateway, want none", got)
	}
}

// TestRunHelp covers the schema-driven help: the argument flags with their
// types, requirement, enum, and default, plus the JSON input of a nested
// object.
func TestRunHelp(t *testing.T) {
	_, configPath := newRunFixture(t, false, completedRunRecord)

	stdout, stderr, code := runCLI(t, []string{"run", "hvac", "set", "--help"}, configPath)
	if code != 0 {
		t.Fatalf("run --help: exit code %d, stderr %s", code, stderr)
	}
	for _, want := range []string{
		"usage: hanse run hvac set [flags]",
		"Set one zone of the building",
		"Mode:", "async",
		"Flags:",
		"--zone string", "required",
		"--mode string", "one of: auto, cool, heat", "default: auto",
		"--level integer",
		"--threshold number",
		"--force",
		"--tags string", "repeat the flag for each value",
		"--executor id",
		"--detach",
		"--args-json object",
		"--args-file path",
		"overrides the key of the same name",
		"Pass retry with --args-json or --args-file.",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("run --help: output does not contain %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "--password") {
		t.Errorf("run --help: a public operation lists --password:\n%s", stdout)
	}
	if strings.Contains(stdout, "\x1b[") {
		t.Errorf("run --help: output contains a terminal escape")
	}
}

// TestRunPrivileged covers the password rules of
// docs/architecture.md section 12.
func TestRunPrivileged(t *testing.T) {
	t.Run("no terminal", func(t *testing.T) {
		fixture, configPath := newRunFixture(t, true, completedRunRecord)
		_, stderr, code := runCLI(t, []string{"run", "hvac", "set", "--zone", "loft"}, configPath)
		if code != 30 {
			t.Fatalf("run: exit code %d, want 30; stderr %s", code, stderr)
		}
		if !strings.Contains(stderr, "elevation_required") {
			t.Errorf("run: stderr %q, want elevation_required", stderr)
		}
		if got := fixture.submitCount(); got != 0 {
			t.Errorf("run: %d submit requests, want none", got)
		}
	})

	t.Run("password flag", func(t *testing.T) {
		fixture, configPath := newRunFixture(t, true, completedRunRecord)
		stdout, stderr, code := runCLI(t, []string{
			"run", "hvac", "set", "--zone", "loft", "--password", runTestPassword,
		}, configPath)
		if code != 0 {
			t.Fatalf("run: exit code %d, stderr %s", code, stderr)
		}
		if !strings.Contains(stderr, "warning") || !strings.Contains(stderr, "history") {
			t.Errorf("run: stderr %q, want the command line warning", stderr)
		}
		if strings.Contains(stdout, runTestPassword) || strings.Contains(stderr, runTestPassword) {
			t.Error("run: the password was printed")
		}
		elevation, ok := fixture.submitted(t)["elevation"].(map[string]any)
		if !ok {
			t.Fatalf("run: the request carries no elevation")
		}
		if elevation["password"] != runTestPassword {
			t.Errorf("run: elevation %v, want the password", elevation)
		}
	})

	t.Run("terminal prompt", func(t *testing.T) {
		fixture, configPath := newRunFixture(t, true, completedRunRecord)
		var out, errOut bytes.Buffer
		code := Run(context.Background(), []string{
			"run", "hvac", "set", "--zone", "loft",
		}, Options{
			Stdin:           strings.NewReader(""),
			StdinIsTerminal: true,
			ReadPassword: func(int) ([]byte, error) {
				return []byte(runTestPassword), nil
			},
			Stdout:     &out,
			Stderr:     &errOut,
			ConfigPath: configPath,
		})
		if code != 0 {
			t.Fatalf("run: exit code %d, want 0; stderr %s", code, errOut.String())
		}
		if !strings.Contains(errOut.String(), "Password:") {
			t.Errorf("run: stderr %q, want a password prompt", errOut.String())
		}
		if strings.Contains(out.String(), runTestPassword) || strings.Contains(errOut.String(), runTestPassword) {
			t.Error("run: the password was printed")
		}
		elevation, ok := fixture.submitted(t)["elevation"].(map[string]any)
		if !ok || elevation["password"] != runTestPassword {
			t.Errorf("run: elevation %v, want the prompted password", elevation)
		}
	})

	t.Run("password on a public operation", func(t *testing.T) {
		fixture, configPath := newRunFixture(t, false, completedRunRecord)
		_, stderr, code := runCLI(t, []string{
			"run", "hvac", "set", "--zone", "loft", "--password", runTestPassword,
		}, configPath)
		if code != 0 {
			t.Fatalf("run: exit code %d, stderr %s", code, stderr)
		}
		if !strings.Contains(stderr, "not privileged") {
			t.Errorf("run: stderr %q, want a note that the password is ignored", stderr)
		}
		if _, ok := fixture.submitted(t)["elevation"]; ok {
			t.Errorf("run: the request carries elevation for a public operation")
		}
	})
}

// TestRunConflict requires the running operation's ID from the error
// details.
func TestRunConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/version":
			_, _ = w.Write([]byte(fixtureVersionBody))
		case r.URL.Path == "/v1/operations/hvac/set/describe":
			_, _ = w.Write([]byte(runDescribe(false)))
		case r.URL.Path == "/v1/operations" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"operation_conflict","message":"operation \"op_running\" is already running",` +
				`"details":{"capability":"hvac","operation":"set","operation_id":"op_running"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"unknown_capability","message":"capability does not exist"}`))
		}
	}))
	t.Cleanup(srv.Close)
	configPath := filepath.Join(t.TempDir(), "hanse", "config.json")
	writeTestConfig(t, configPath, srv.URL, testToken)

	stdout, stderr, code := runCLI(t, []string{"run", "hvac", "set", "--zone", "loft"}, configPath)
	if code != 32 {
		t.Fatalf("run: exit code %d, want 32; stderr %s", code, stderr)
	}
	if !strings.Contains(stderr, "op_running") || !strings.Contains(stderr, "already running") {
		t.Errorf("run: stderr %q, want the running operation's ID", stderr)
	}
	if stdout != "" {
		t.Errorf("run: stdout %q, want no output", stdout)
	}
}

// TestRunFailedOperation requires the recorded failure to drive the exit
// code.
func TestRunFailedOperation(t *testing.T) {
	failed := `{"id": "op_hvac_1", "capability": "hvac", "operation": "set", ` +
		`"requester": "alice", "executor": "node-a", "executor_source": "default", ` +
		`"status": "failed", "mode": "async", "created_at": "2026-10-31T08:00:00Z", ` +
		`"started_at": "2026-10-31T08:00:01Z", "finished_at": "2026-10-31T08:00:02Z", ` +
		`"summary": "the handler exited nonzero", ` +
		`"error": {"code": "handler_exit", "message": "the handler exited nonzero", "details": {"exit_code": 1}}}`
	_, configPath := newRunFixture(t, false, failed)

	stdout, stderr, code := runCLI(t, []string{"run", "hvac", "set", "--zone", "loft"}, configPath)
	if code != 38 {
		t.Fatalf("run: exit code %d, want 38; stderr %s", code, stderr)
	}
	if !strings.Contains(stderr, "handler_exit") {
		t.Errorf("run: stderr %q, want the recorded code", stderr)
	}
	if !strings.Contains(stdout, "op_hvac_1") {
		t.Errorf("run: stdout %q, want the operation record", stdout)
	}
}

// TestRunInterrupted proves Ctrl+C ends the wait without cancelling the
// operation and leaves the ID behind.
func TestRunInterrupted(t *testing.T) {
	fixture, configPath := newRunFixture(t, false, runRecord("op_hvac_1", "running", ""))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fixture.afterRun = func() {
		time.AfterFunc(50*time.Millisecond, cancel)
	}

	var out, errOut bytes.Buffer
	code := Run(ctx, []string{"run", "hvac", "set", "--zone", "loft"}, Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &out,
		Stderr:     &errOut,
		ConfigPath: configPath,
	})
	if code != 0 {
		t.Fatalf("run: exit code %d, want 0; stderr %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "op_hvac_1") || !strings.Contains(errOut.String(), "still running") {
		t.Errorf("run: stderr %q, want the running operation's ID", errOut.String())
	}
	fixture.mu.Lock()
	cancels := fixture.cancels
	fixture.mu.Unlock()
	if cancels != 0 {
		t.Errorf("run: %d cancel requests, want none", cancels)
	}
}

// TestComplete covers the hidden completion hook: commands, capabilities,
// operations, flags, enum values, and the nodes of a hanse:node argument.
func TestComplete(t *testing.T) {
	_, configPath := newRunFixture(t, false)

	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"command", []string{"__complete", "ru"}, []string{"run"}},
		{"capability", []string{"__complete", "run", "h"}, []string{"hvac"}},
		{"operation", []string{"__complete", "run", "hvac", ""}, []string{"set"}},
		{"flags", []string{"__complete", "run", "hvac", "set", "--"}, []string{"--zone", "--detach", "--executor", "--args-json"}},
		{"enum", []string{"__complete", "run", "hvac", "set", "--mode", ""}, []string{"auto", "cool", "heat"}},
		{"nodes", []string{"__complete", "run", "hvac", "set", "--zone", ""}, []string{"loft", "node-a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := runCLI(t, tc.args, configPath)
			if code != 0 {
				t.Fatalf("%v: exit code %d, stderr %s", tc.args, code, stderr)
			}
			for _, want := range tc.want {
				if !strings.Contains(stdout, want+"\n") && strings.TrimSpace(stdout) != want {
					t.Errorf("%v: output %q does not contain %q", tc.args, stdout, want)
				}
			}
		})
	}
}

// TestCompleteSilentOnError requires a broken Gateway to print nothing and
// exit 0, so completion never breaks a shell.
func TestCompleteSilentOnError(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "hanse", "config.json")
	stdout, stderr, code := runCLI(t, []string{"__complete", "run", "hvac", "set", "--zone", ""}, configPath)
	if code != 0 {
		t.Fatalf("__complete: exit code %d, want 0; stderr %s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("__complete: stdout %q, want nothing", stdout)
	}
}

// sameJSON reports whether got and want hold the same JSON value.
func sameJSON(got, want any) bool {
	gotJSON, err := json.Marshal(got)
	if err != nil {
		return false
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		return false
	}
	var gotValue, wantValue any
	if err := json.Unmarshal(gotJSON, &gotValue); err != nil {
		return false
	}
	if err := json.Unmarshal(wantJSON, &wantValue); err != nil {
		return false
	}
	return reflect.DeepEqual(gotValue, wantValue)
}

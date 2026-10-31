package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureDescribeBody is the body of
// GET /v1/operations/pc/up/describe. Every value is a placeholder: the
// addresses in 192.0.2.0/24 and the MACs in 00:00:5e:00:53:xx are reserved
// for documentation.
const fixtureDescribeBody = `{
  "capability": "pc",
  "operation": "up",
  "description": "Wake a node with Wake-on-LAN and wait until it answers",
  "mode": "async",
  "privileged": false,
  "schema": {
    "additionalProperties": false,
    "properties": {
      "node": {
        "description": "The node to wake",
        "format": "hanse:node",
        "type": "string"
      },
      "wait": {
        "description": "Wait until the node answers",
        "type": "boolean"
      }
    },
    "required": [
      "node"
    ],
    "type": "object"
  },
  "required": [
    "node"
  ],
  "options": [
    {
      "field": "node",
      "provider": "nodes"
    }
  ],
  "node_filters": {
    "node": {
      "os": [
        "linux"
      ],
      "roles": [
        "server"
      ]
    }
  },
  "executors": [
    {
      "id": "node-a",
      "status": "online",
      "default": true
    },
    {
      "id": "node-b",
      "status": "offline",
      "default": false
    }
  ],
  "default_executor": "node-a",
  "executor_required": false
}`

// validateAnswer is the request body the validation fixture receives.
type validateAnswer struct {
	Capability string                     `json:"capability"`
	Operation  string                     `json:"operation"`
	Arguments  map[string]json.RawMessage `json:"arguments"`
	Executor   string                     `json:"executor"`
}

// newDescribeGateway starts a Gateway fixture for describe and validate. It
// requires the test token and checks the arguments of a validation the way
// the Gateway does: a missing required argument, an unknown node, and a
// wrong type each produce the documented error.
func newDescribeGateway(t *testing.T) *httptest.Server {
	t.Helper()
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
		case r.URL.Path == "/v1/operations/pc/up/describe":
			_, _ = w.Write([]byte(fixtureDescribeBody))
		case r.URL.Path == "/v1/operations/validate" && r.Method == http.MethodPost:
			writeValidateAnswer(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"unknown_capability","message":"capability does not exist"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// writeValidateAnswer answers one validation the way the Gateway does,
// with placeholder results. node-a is the default executor.
func writeValidateAnswer(w http.ResponseWriter, r *http.Request) {
	var body validateAnswer
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"invalid_request","message":"the request body is malformed"}`))
		return
	}
	raw, ok := body.Arguments["node"]
	if !ok || strings.TrimSpace(string(raw)) == "null" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"missing_argument","message":"the argument \"node\" is required","details":{"field":"node"}}`))
		return
	}
	var node string
	if err := json.Unmarshal(raw, &node); err != nil {
		writeArgumentProblem(w, "/node", "type", "the value must be a string")
		return
	}
	switch node {
	case "node-a", "node-c":
	default:
		writeArgumentProblem(w, "/node", "format", "the value does not name a known node")
		return
	}
	if raw, ok := body.Arguments["wait"]; ok {
		var wait bool
		if err := json.Unmarshal(raw, &wait); err != nil {
			writeArgumentProblem(w, "/wait", "type", "the value must be a boolean")
			return
		}
	}
	if body.Executor != "" && body.Executor != "node-a" && body.Executor != "node-b" {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"invalid_executor","message":"the executor is not a known node","details":{"executor":"` + body.Executor + `"}}`))
		return
	}
	executor, source := body.Executor, "explicit"
	if executor == "" {
		executor, source = "node-a", "default"
	}
	answer, err := json.Marshal(struct {
		Valid          bool   `json:"valid"`
		Capability     string `json:"capability"`
		Operation      string `json:"operation"`
		Mode           string `json:"mode"`
		Executor       string `json:"executor"`
		ExecutorSource string `json:"executor_source"`
	}{true, body.Capability, body.Operation, "async", executor, source})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(answer)
}

// writeArgumentProblem writes one invalid_arguments error object.
func writeArgumentProblem(w http.ResponseWriter, path, rule, message string) {
	w.WriteHeader(http.StatusUnprocessableEntity)
	answer, err := json.Marshal(struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	}{
		Code:    "invalid_arguments",
		Message: "the arguments do not match the schema",
		Details: map[string]any{"errors": []any{
			map[string]any{"path": path, "rule": rule, "message": message},
		}},
	})
	if err != nil {
		return
	}
	_, _ = w.Write(answer)
}

// newDescribeConfig writes a configuration for a fixture Gateway.
func newDescribeConfig(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "hanse", "config.json")
	writeTestConfig(t, configPath, srv.URL, testToken)
	return configPath
}

// TestDescribeGoldenJSON covers the describe JSON output.
func TestDescribeGoldenJSON(t *testing.T) {
	srv := newDescribeGateway(t)
	configPath := newDescribeConfig(t, srv)

	stdout, stderr, code := runCLI(t, []string{"describe", "pc", "up", "--format", "json"}, configPath)
	if code != 0 {
		t.Fatalf("describe: exit code %d, stderr %s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("describe: unexpected stderr %q", stderr)
	}
	if strings.Contains(stdout, "\x1b[") {
		t.Errorf("describe: JSON output contains a terminal escape")
	}
	checkGolden(t, "describe", []byte(stdout))

	again, _, againCode := runCLI(t, []string{"describe", "pc", "up", "--format", "json"}, configPath)
	if againCode != 0 {
		t.Fatalf("describe again: exit code %d", againCode)
	}
	if again != stdout {
		t.Errorf("describe: two runs differ\n--- first ---\n%s\n--- second ---\n%s", stdout, again)
	}
}

// TestDescribeText covers the readable output: the arguments with their
// types and required flag, the executors, the option providers, and the
// schema.
func TestDescribeText(t *testing.T) {
	srv := newDescribeGateway(t)
	configPath := newDescribeConfig(t, srv)

	stdout, stderr, code := runCLI(t, []string{"describe", "pc", "up"}, configPath)
	if code != 0 {
		t.Fatalf("describe: exit code %d, stderr %s", code, stderr)
	}
	for _, want := range []string{
		"Capability:", "pc", "Operation:", "up", "Mode:", "async",
		"Arguments:", "FIELD", "TYPE", "REQUIRED", "DESCRIPTION",
		"node", "string (hanse:node)", "wait", "boolean",
		"Executors:", "node-a", "online", "node-b",
		"Options:", "nodes",
		"Node filters:", "linux", "server",
		"Schema:", `"required"`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("describe: output does not contain %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "\x1b[") {
		t.Errorf("describe: text output contains a terminal escape")
	}
	// The required argument is marked and the optional one is not.
	if !strings.Contains(stdout, "node") || !strings.Contains(stdout, "yes") {
		t.Errorf("describe: output does not mark the required argument:\n%s", stdout)
	}
}

// TestValidateGoldenJSON covers the validate JSON output for a valid
// request that names an executor.
func TestValidateGoldenJSON(t *testing.T) {
	srv := newDescribeGateway(t)
	configPath := newDescribeConfig(t, srv)

	args := []string{
		"validate", "pc", "up",
		"--args-json", `{"node":"node-c","wait":true}`,
		"--executor", "node-b",
		"--format", "json",
	}
	stdout, stderr, code := runCLI(t, args, configPath)
	if code != 0 {
		t.Fatalf("validate: exit code %d, stderr %s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("validate: unexpected stderr %q", stderr)
	}
	checkGolden(t, "validate", []byte(stdout))

	again, _, againCode := runCLI(t, args, configPath)
	if againCode != 0 {
		t.Fatalf("validate again: exit code %d", againCode)
	}
	if again != stdout {
		t.Errorf("validate: two runs differ\n--- first ---\n%s\n--- second ---\n%s", stdout, again)
	}
}

// TestValidateDefaultExecutor covers a valid request with no executor: the
// Gateway answers with the default one, and the text output shows it.
func TestValidateDefaultExecutor(t *testing.T) {
	srv := newDescribeGateway(t)
	configPath := newDescribeConfig(t, srv)

	stdout, stderr, code := runCLI(t, []string{
		"validate", "pc", "up", "--args-json", `{"node":"node-c"}`,
	}, configPath)
	if code != 0 {
		t.Fatalf("validate: exit code %d, stderr %s", code, stderr)
	}
	for _, want := range []string{"Valid:", "yes", "node-a", "default", "async"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("validate: output does not contain %q:\n%s", want, stdout)
		}
	}
}

// TestValidateArgsFile covers the arguments from a file.
func TestValidateArgsFile(t *testing.T) {
	srv := newDescribeGateway(t)
	configPath := newDescribeConfig(t, srv)

	path := filepath.Join(t.TempDir(), "arguments.json")
	if err := os.WriteFile(path, []byte("{\n  \"node\": \"node-c\"\n}\n"), 0o600); err != nil {
		t.Fatalf("write arguments file: %v", err)
	}
	stdout, stderr, code := runCLI(t, []string{
		"validate", "pc", "up", "--args-file", path, "--format", "json",
	}, configPath)
	if code != 0 {
		t.Fatalf("validate: exit code %d, stderr %s", code, stderr)
	}
	if !strings.Contains(stdout, `"valid": true`) || !strings.Contains(stdout, `"executor": "node-a"`) {
		t.Errorf("validate: unexpected output %s", stdout)
	}
}

// TestValidateInvalidRequests covers the documented exit codes and the
// printed problems of a rejected request.
func TestValidateInvalidRequests(t *testing.T) {
	srv := newDescribeGateway(t)
	configPath := newDescribeConfig(t, srv)

	cases := []struct {
		name   string
		args   []string
		code   int
		stderr []string
		absent []string
	}{
		{
			name:   "invalid arguments",
			args:   []string{"validate", "pc", "up", "--args-json", `{"node":"node-z"}`, "--format", "json"},
			code:   22,
			stderr: []string{"invalid_arguments", "problem: /node: format", "known node"},
			absent: []string{"errors: ["},
		},
		{
			name:   "wrong type",
			args:   []string{"validate", "pc", "up", "--args-json", `{"node":"node-c","wait":"yes"}`, "--format", "json"},
			code:   22,
			stderr: []string{"invalid_arguments", "problem: /wait: type"},
		},
		{
			name:   "missing argument",
			args:   []string{"validate", "pc", "up", "--format", "json"},
			code:   23,
			stderr: []string{"missing_argument", "required"},
		},
		{
			name:   "invalid executor",
			args:   []string{"validate", "pc", "up", "--args-json", `{"node":"node-c"}`, "--executor", "node-z", "--format", "json"},
			code:   24,
			stderr: []string{"invalid_executor"},
		},
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
			for _, want := range tc.stderr {
				if !strings.Contains(stderr, want) {
					t.Errorf("%v: stderr %q, want %q", tc.args, stderr, want)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(stderr, absent) {
					t.Errorf("%v: stderr %q repeats %q", tc.args, stderr, absent)
				}
			}
		})
	}
}

// TestDescribeValidateUsageErrors covers command line mistakes.
func TestDescribeValidateUsageErrors(t *testing.T) {
	srv := newDescribeGateway(t)
	configPath := newDescribeConfig(t, srv)

	path := filepath.Join(t.TempDir(), "arguments.json")
	if err := os.WriteFile(path, []byte(`{"node":"node-c"}`), 0o600); err != nil {
		t.Fatalf("write arguments file: %v", err)
	}
	cases := [][]string{
		{"describe", "pc"},
		{"describe", "pc", "up", "--format", "yaml"},
		{"validate", "pc"},
		{"validate", "pc", "up", "--args-json", `{"node":"node-c"}`, "--args-file", path},
		{"validate", "pc", "up", "--args-json", `{"node":"node-c"`},
		{"validate", "pc", "up", "--args-json", `["node"]`},
		{"validate", "pc", "up", "--args-json", `{"node":"node-c"} {}`},
		{"validate", "pc", "up", "--args-file", filepath.Join(t.TempDir(), "missing.json")},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, stderr, code := runCLI(t, args, configPath)
			if code != 2 {
				t.Fatalf("%v: exit code %d, want 2; stderr %s", args, code, stderr)
			}
			if stderr == "" {
				t.Errorf("%v: no error message", args)
			}
		})
	}
}

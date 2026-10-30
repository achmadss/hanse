package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	herrors "github.com/achmadss/hanse/internal/errors"
)

// fixtureDescribe is the body of
// GET /v1/operations/pc/up/describe. Every value is a placeholder:
// 192.0.2.0/24 and 00:00:5e:00:53:xx are reserved for documentation.
const fixtureDescribe = `{
  "capability": "pc",
  "operation": "up",
  "description": "Wake a node with Wake-on-LAN",
  "mode": "async",
  "privileged": false,
  "schema": {
    "additionalProperties": false,
    "properties": {
      "node": {"description": "The node to wake", "format": "hanse:node", "type": "string"},
      "wait": {"description": "Wait until the node answers", "type": "boolean"}
    },
    "required": ["node"],
    "type": "object"
  },
  "required": ["node"],
  "options": [{"field": "node", "provider": "nodes"}],
  "node_filters": {
    "node": {"os": ["linux"], "roles": ["server"]}
  },
  "executors": [
    {"id": "node-a", "status": "online", "default": true},
    {"id": "node-b", "status": "offline", "default": false}
  ],
  "default_executor": "node-a",
  "executor_required": false
}`

// fixtureValidate is the body of POST /v1/operations/validate.
const fixtureValidate = `{
  "valid": true,
  "capability": "pc",
  "operation": "up",
  "mode": "async",
  "executor": "node-b",
  "executor_source": "explicit"
}`

// TestDescribe covers GET /v1/operations/{capability}/{operation}/describe.
func TestDescribe(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"/v1/operations/pc/up/describe": jsonRoute(fixtureDescribe),
	})
	c := newTestClient(t, srv)

	describe, err := c.Describe(context.Background(), "pc", "up")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if describe.Capability != "pc" || describe.Operation != "up" {
		t.Errorf("describe: got %q/%q", describe.Capability, describe.Operation)
	}
	if describe.Mode != "async" || describe.Privileged {
		t.Errorf("describe: got mode %q, privileged %v", describe.Mode, describe.Privileged)
	}
	if strings.Join(describe.Required, ",") != "node" {
		t.Errorf("describe: required %v, want [node]", describe.Required)
	}
	if len(describe.Options) != 1 || describe.Options[0].Field != "node" || describe.Options[0].Provider != "nodes" {
		t.Errorf("describe: options %+v", describe.Options)
	}
	filter, ok := describe.NodeFilters["node"]
	if !ok {
		t.Fatalf("describe: node_filters %+v, want a node filter", describe.NodeFilters)
	}
	if strings.Join(filter.OS, ",") != "linux" || strings.Join(filter.Roles, ",") != "server" {
		t.Errorf("describe: node filter %+v", filter)
	}
	if len(describe.Executors) != 2 {
		t.Fatalf("describe: executors %+v, want two", describe.Executors)
	}
	if describe.Executors[0].ID != "node-a" || !describe.Executors[0].Default {
		t.Errorf("describe: first executor %+v, want the default node-a", describe.Executors[0])
	}
	if describe.DefaultExecutor != "node-a" || describe.ExecutorRequired {
		t.Errorf("describe: default %q, required %v", describe.DefaultExecutor, describe.ExecutorRequired)
	}
	object, ok := describe.Schema.(map[string]any)
	if !ok {
		t.Fatalf("describe: schema is %T, want a JSON object", describe.Schema)
	}
	if _, ok := object["properties"]; !ok {
		t.Errorf("describe: schema %+v has no properties", object)
	}
}

// TestDescribeKeepsNumbers requires json.Number, so a large default keeps
// every digit when the CLI prints the describe response.
func TestDescribeKeepsNumbers(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"/v1/operations/pc/up/describe": jsonRoute(`{
			"capability": "pc", "operation": "up",
			"schema": {"properties": {"timeout": {"type": "integer", "default": 9007199254740993}}}
		}`),
	})
	c := newTestClient(t, srv)
	describe, err := c.Describe(context.Background(), "pc", "up")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	data, err := json.Marshal(describe)
	if err != nil {
		t.Fatalf("marshal describe: %v", err)
	}
	if !strings.Contains(string(data), "9007199254740993") {
		t.Errorf("describe: %s lost digits", data)
	}
}

// TestDescribeNormalizesEmptyLists requires empty lists and maps rather than
// null, so the CLI prints [] and {}.
func TestDescribeNormalizesEmptyLists(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"/v1/operations/pc/up/describe": jsonRoute(`{
			"capability": "pc", "operation": "up", "schema": {},
			"required": null, "options": null, "node_filters": null, "executors": null
		}`),
	})
	c := newTestClient(t, srv)
	describe, err := c.Describe(context.Background(), "pc", "up")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if describe.Required == nil || describe.Options == nil || describe.Executors == nil {
		t.Errorf("describe: got %+v, want empty lists", describe)
	}
	if describe.NodeFilters == nil {
		t.Error("describe: node_filters is nil, want an empty map")
	}
}

// TestValidate covers POST /v1/operations/validate: the request fields and
// the decoded answer.
func TestValidate(t *testing.T) {
	var got map[string]any
	var method, contentType string
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"/v1/operations/validate": func(w http.ResponseWriter, r *http.Request) {
			method = r.Method
			contentType = r.Header.Get("Content-Type")
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read request body: %v", err)
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Errorf("request body is not JSON: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fixtureValidate))
		},
	})
	c := newTestClient(t, srv)

	result, err := c.Validate(context.Background(), ValidateRequest{
		Capability: "pc",
		Operation:  "up",
		Arguments:  json.RawMessage(`{"node":"node-c"}`),
		Executor:   "node-b",
	})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if method != http.MethodPost {
		t.Errorf("validate: method %q, want POST", method)
	}
	if contentType != "application/json" {
		t.Errorf("validate: Content-Type %q, want application/json", contentType)
	}
	if got["capability"] != "pc" || got["operation"] != "up" || got["executor"] != "node-b" {
		t.Errorf("validate: request body %+v", got)
	}
	arguments, ok := got["arguments"].(map[string]any)
	if !ok || arguments["node"] != "node-c" {
		t.Errorf("validate: arguments %+v, want an object with node", got["arguments"])
	}
	if !result.Valid || result.Executor != "node-b" || result.ExecutorSource != "explicit" {
		t.Errorf("validate: result %+v", result)
	}
	if result.Mode != "async" {
		t.Errorf("validate: mode %q, want async", result.Mode)
	}
}

// TestValidateOmitsEmptyFields requires a request without arguments or an
// executor to carry neither field, so the Gateway sees an absent value
// rather than a null one.
func TestValidateOmitsEmptyFields(t *testing.T) {
	var body string
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"/v1/operations/validate": func(w http.ResponseWriter, r *http.Request) {
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read request body: %v", err)
			}
			body = string(data)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fixtureValidate))
		},
	})
	c := newTestClient(t, srv)
	if _, err := c.Validate(context.Background(), ValidateRequest{Capability: "pc", Operation: "up"}); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if strings.Contains(body, "arguments") || strings.Contains(body, "executor") {
		t.Errorf("validate: request body %q, want no empty fields", body)
	}
}

// TestValidateErrorMapping maps a rejected request to the shared code, its
// exit code, and its details.
func TestValidateErrorMapping(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"/v1/operations/validate": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"code":"invalid_arguments","message":"the arguments do not match the schema","details":{"errors":[{"path":"/node","rule":"format","message":"the value does not name a known node"}]}}`))
		},
	})
	c := newTestClient(t, srv)
	_, err := c.Validate(context.Background(), ValidateRequest{Capability: "pc", Operation: "up"})
	if !errors.Is(err, herrors.InvalidArguments) {
		t.Fatalf("validate: error %v, want the InvalidArguments code", err)
	}
	if got := herrors.ExitCode(err); got != 22 {
		t.Errorf("validate: exit code %d, want 22", got)
	}
	var apiErr *herrors.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("validate: error type %T, want *herrors.Error", err)
	}
	problems, ok := apiErr.Details["errors"].([]any)
	if !ok || len(problems) != 1 {
		t.Errorf("validate: details %+v, want one problem", apiErr.Details)
	}
}

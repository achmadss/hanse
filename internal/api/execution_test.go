package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/achmadss/hanse/internal/auth"
	"github.com/achmadss/hanse/internal/config"
	"github.com/achmadss/hanse/internal/operations"
	"github.com/achmadss/hanse/internal/runtime"
	"github.com/achmadss/hanse/internal/runtime/builtin/echotest"
)

// executionFixture is one Gateway under test: the echo capability, the
// operations manager over the builtin dispatcher, and two API tokens.
type executionFixture struct {
	handler http.Handler
	manager *operations.Manager
	token   string
	limited string
}

// newExecutionFixture assembles the Gateway. The auth middleware wraps the
// API handler exactly as the server does.
func newExecutionFixture(t *testing.T) *executionFixture {
	t.Helper()
	reg := newFixture(t)
	handlers := runtime.NewRegistry()
	if err := echotest.Register(handlers, reg, echotest.Options{
		GatewayHost:   "gateway",
		SleepInterval: 5 * time.Millisecond,
	}); err != nil {
		t.Fatalf("register echo: %v", err)
	}
	manager, err := operations.New(operations.Config{
		Registry:   reg,
		Dispatcher: operations.NewBuiltinDispatcher(handlers),
		Timeouts: config.Timeouts{
			Operation:   10 * time.Second,
			GracePeriod: time.Second,
		},
	})
	if err != nil {
		t.Fatalf("build manager: %v", err)
	}
	store, err := auth.LoadStore(filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatalf("load token store: %v", err)
	}
	token, err := store.Add("tester", nil)
	if err != nil {
		t.Fatalf("add token: %v", err)
	}
	limited, err := store.Add("limited", []string{"other.op"})
	if err != nil {
		t.Fatalf("add limited token: %v", err)
	}
	return &executionFixture{
		handler: store.Middleware(New(reg, manager, nil)),
		manager: manager,
		token:   token,
		limited: limited,
	}
}

// call performs one API request with a bearer token and optional extra
// headers, and returns the response and its body.
func call(t *testing.T, handler http.Handler, token, method, path, body string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Result(), rec.Body.Bytes()
}

// decodeOperation decodes one operation view from a submit response.
func decodeOperation(t *testing.T, body []byte) operationView {
	t.Helper()
	var view operationView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("body is not an operation: %v (%s)", err, body)
	}
	return view
}

// decodeWireError decodes the shared error object.
func decodeWireError(t *testing.T, body []byte) wireError {
	t.Helper()
	var got wireError
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body is not a JSON error: %v (%s)", err, body)
	}
	return got
}

// awaitOperation polls GET /v1/operations/{id} until the operation reaches
// want.
func awaitOperation(t *testing.T, fixture *executionFixture, id string, want string) operationView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, body := call(t, fixture.handler, fixture.token, http.MethodGet, "/v1/operations/"+id, "", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET operation %s: status %d, body %s", id, resp.StatusCode, body)
		}
		var wrapped operationResponse
		if err := json.Unmarshal(body, &wrapped); err != nil {
			t.Fatalf("GET operation %s: %v (%s)", id, err, body)
		}
		if wrapped.Operation.Status == want {
			return wrapped.Operation
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("operation %s did not reach %s", id, want)
	return operationView{}
}

// TestSubmitSync runs echo.say end to end and requires the result in the
// submit response.
func TestSubmitSync(t *testing.T) {
	fixture := newExecutionFixture(t)
	body := `{"capability":"echo","operation":"say","arguments":{"message":"hello"}}`
	resp, payload := call(t, fixture.handler, fixture.token, http.MethodPost, "/v1/operations", body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("submit: status %d, body %s", resp.StatusCode, payload)
	}
	view := decodeOperation(t, payload)
	if view.Status != "completed" {
		t.Errorf("status %q, want completed", view.Status)
	}
	if view.Capability != "echo" || view.Operation != "say" {
		t.Errorf("operation %s.%s, want echo.say", view.Capability, view.Operation)
	}
	if view.Executor != "gateway" || view.ExecutorSource != "default" {
		t.Errorf("executor %q source %q, want gateway default", view.Executor, view.ExecutorSource)
	}
	if view.Mode != "sync" {
		t.Errorf("mode %q, want sync", view.Mode)
	}
	var result struct {
		Said string `json:"said"`
	}
	if err := json.Unmarshal(view.Result, &result); err != nil || result.Said != "hello" {
		t.Errorf("result %s, want the echoed message", view.Result)
	}
	if view.StartedAt == nil || view.FinishedAt == nil {
		t.Error("completed operation has no start or finish time")
	}
}

// TestValidateEndpoint validates without creating an operation.
func TestValidateEndpoint(t *testing.T) {
	fixture := newExecutionFixture(t)
	body := `{"capability":"echo","operation":"say","arguments":{"message":"hello"}}`
	resp, payload := call(t, fixture.handler, fixture.token, http.MethodPost, "/v1/operations/validate", body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("validate: status %d, body %s", resp.StatusCode, payload)
	}
	var validated validateResponse
	if err := json.Unmarshal(payload, &validated); err != nil {
		t.Fatalf("validate body: %v (%s)", err, payload)
	}
	if !validated.Valid || validated.Executor != "gateway" || validated.ExecutorSource != "default" {
		t.Errorf("validate response %+v", validated)
	}
	if validated.Mode != "sync" {
		t.Errorf("validate mode %q, want sync", validated.Mode)
	}
	records, err := fixture.manager.List(operations.Filter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("validate created %d operations, want none", len(records))
	}

	// A request that would fail validation fails here without running.
	bad := `{"capability":"echo","operation":"say","arguments":{}}`
	resp, payload = call(t, fixture.handler, fixture.token, http.MethodPost, "/v1/operations/validate", bad, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("validate bad arguments: status %d, body %s", resp.StatusCode, payload)
	}
	if got := decodeWireError(t, payload).Code; got != "invalid_arguments" {
		t.Errorf("validate bad arguments: code %q, want invalid_arguments", got)
	}
}

// TestSubmitAsync starts echo.sleep, gets the operation ID at once, and
// follows it to completion.
func TestSubmitAsync(t *testing.T) {
	fixture := newExecutionFixture(t)
	body := `{"capability":"echo","operation":"sleep","arguments":{"seconds":1}}`
	resp, payload := call(t, fixture.handler, fixture.token, http.MethodPost, "/v1/operations", body, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("submit: status %d, body %s", resp.StatusCode, payload)
	}
	view := decodeOperation(t, payload)
	if view.ID == "" {
		t.Fatal("async submit returned no operation ID")
	}
	if view.Mode != "async" {
		t.Errorf("mode %q, want async", view.Mode)
	}
	if view.Status != "queued" && view.Status != "running" {
		t.Errorf("status %q, want queued or running", view.Status)
	}

	finished := awaitOperation(t, fixture, view.ID, "completed")
	var result struct {
		Slept int `json:"slept"`
	}
	if err := json.Unmarshal(finished.Result, &result); err != nil || result.Slept != 1 {
		t.Errorf("result %s, want the slept count", finished.Result)
	}

	// The list endpoint returns the finished operation.
	resp, payload = call(t, fixture.handler, fixture.token, http.MethodGet, "/v1/operations", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: status %d, body %s", resp.StatusCode, payload)
	}
	var listed operationsResponse
	if err := json.Unmarshal(payload, &listed); err != nil {
		t.Fatalf("list body: %v (%s)", err, payload)
	}
	if len(listed.Operations) != 1 || listed.Operations[0].ID != view.ID {
		t.Errorf("list %+v, want operation %s", listed.Operations, view.ID)
	}
}

// TestIdempotencyKey retries a state-changing request and gets the
// original operation instead of a second run.
func TestIdempotencyKey(t *testing.T) {
	fixture := newExecutionFixture(t)
	body := `{"capability":"echo","operation":"say","arguments":{"message":"once"}}`
	headers := map[string]string{"Idempotency-Key": "retry-1"}

	resp, payload := call(t, fixture.handler, fixture.token, http.MethodPost, "/v1/operations", body, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first submit: status %d, body %s", resp.StatusCode, payload)
	}
	first := decodeOperation(t, payload)

	resp, payload = call(t, fixture.handler, fixture.token, http.MethodPost, "/v1/operations", body, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retry: status %d, body %s", resp.StatusCode, payload)
	}
	retry := decodeOperation(t, payload)
	if retry.ID != first.ID {
		t.Errorf("retry returned operation %s, want %s", retry.ID, first.ID)
	}
	if !bytes.Equal(retry.Result, first.Result) {
		t.Errorf("retry result %s, want %s", retry.Result, first.Result)
	}
	if records, err := fixture.manager.List(operations.Filter{}); err != nil {
		t.Fatalf("list: %v", err)
	} else if len(records) != 1 {
		t.Errorf("the retry stored %d operations, want 1", len(records))
	}

	// The same key with other arguments is rejected, not run.
	other := `{"capability":"echo","operation":"say","arguments":{"message":"twice"}}`
	resp, payload = call(t, fixture.handler, fixture.token, http.MethodPost, "/v1/operations", other, headers)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("reused key: status %d, body %s", resp.StatusCode, payload)
	}
	if got := decodeWireError(t, payload).Code; got != "invalid_arguments" {
		t.Errorf("reused key: code %q, want invalid_arguments", got)
	}
}

// TestCancelOperation cancels a running asynchronous operation.
func TestCancelOperation(t *testing.T) {
	fixture := newExecutionFixture(t)
	body := `{"capability":"echo","operation":"sleep","arguments":{"seconds":30}}`
	resp, payload := call(t, fixture.handler, fixture.token, http.MethodPost, "/v1/operations", body, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("submit: status %d, body %s", resp.StatusCode, payload)
	}
	view := decodeOperation(t, payload)
	awaitOperation(t, fixture, view.ID, "running")

	resp, payload = call(t, fixture.handler, fixture.token, http.MethodPost, "/v1/operations/"+view.ID+"/cancel", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel: status %d, body %s", resp.StatusCode, payload)
	}
	cancelled := decodeOperation(t, payload)
	if cancelled.Status != "cancelled" {
		t.Errorf("cancel status %q, want cancelled", cancelled.Status)
	}
	if cancelled.FinishedAt == nil {
		t.Error("cancelled operation has no finish time")
	}
	awaitOperation(t, fixture, view.ID, "cancelled")
}

// TestUnknownOperationID reports a stored operation that does not exist.
func TestUnknownOperationID(t *testing.T) {
	fixture := newExecutionFixture(t)
	resp, payload := call(t, fixture.handler, fixture.token, http.MethodGet, "/v1/operations/op_missing", "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET missing operation: status %d, body %s", resp.StatusCode, payload)
	}
	if got := decodeWireError(t, payload).Code; got != "unknown_operation" {
		t.Errorf("GET missing operation: code %q, want unknown_operation", got)
	}
}

// TestOperationAuth covers the missing token and a token without the
// capability permission.
func TestOperationAuth(t *testing.T) {
	fixture := newExecutionFixture(t)
	body := `{"capability":"echo","operation":"say","arguments":{"message":"hello"}}`
	resp, payload := call(t, fixture.handler, "", http.MethodPost, "/v1/operations", body, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: status %d, body %s", resp.StatusCode, payload)
	}
	if got := decodeWireError(t, payload).Code; got != "authentication_failed" {
		t.Errorf("no token: code %q, want authentication_failed", got)
	}

	resp, payload = call(t, fixture.handler, fixture.limited, http.MethodPost, "/v1/operations", body, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("limited token: status %d, body %s", resp.StatusCode, payload)
	}
	if got := decodeWireError(t, payload).Code; got != "forbidden" {
		t.Errorf("limited token: code %q, want forbidden", got)
	}
}

// TestElevationNotReturned sends the optional elevation object and
// requires the password to stay out of every response and record.
func TestElevationNotReturned(t *testing.T) {
	fixture := newExecutionFixture(t)
	const secret = "hunter2-not-a-real-password"
	body := `{"capability":"echo","operation":"say","arguments":{"message":"hello"},` +
		`"elevation":{"password":"` + secret + `"}}`
	resp, payload := call(t, fixture.handler, fixture.token, http.MethodPost, "/v1/operations", body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("submit with elevation: status %d, body %s", resp.StatusCode, payload)
	}
	if bytes.Contains(payload, []byte(secret)) {
		t.Errorf("the submit response holds the elevation password: %s", payload)
	}
	view := decodeOperation(t, payload)

	resp, payload = call(t, fixture.handler, fixture.token, http.MethodGet, "/v1/operations/"+view.ID, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET operation: status %d, body %s", resp.StatusCode, payload)
	}
	if bytes.Contains(payload, []byte(secret)) {
		t.Errorf("the operation response holds the elevation password: %s", payload)
	}
}

// TestOperationsUnavailable reports the Internal code when the Gateway has
// no operations manager.
func TestOperationsUnavailable(t *testing.T) {
	store, err := auth.LoadStore(filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatalf("load token store: %v", err)
	}
	token, err := store.Add("tester", nil)
	if err != nil {
		t.Fatalf("add token: %v", err)
	}
	handler := store.Middleware(New(newFixture(t), nil, nil))
	body := `{"capability":"echo","operation":"say","arguments":{"message":"hello"}}`
	resp, payload := call(t, handler, token, http.MethodPost, "/v1/operations", body, nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("no manager: status %d, body %s", resp.StatusCode, payload)
	}
	if got := decodeWireError(t, payload).Code; got != "internal" {
		t.Errorf("no manager: code %q, want internal", got)
	}
}

// TestOperationAuthorization covers the allow list on read, list, and
// cancel: a token never sees or stops an operation outside its list.
func TestOperationAuthorization(t *testing.T) {
	fixture := newExecutionFixture(t)
	body := `{"capability":"echo","operation":"say","arguments":{"message":"hello"}}`
	resp, payload := call(t, fixture.handler, fixture.token, http.MethodPost, "/v1/operations", body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("submit: status %d, body %s", resp.StatusCode, payload)
	}
	view := decodeOperation(t, payload)

	resp, payload = call(t, fixture.handler, fixture.limited, http.MethodGet, "/v1/operations/"+view.ID, "", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("read with a limited token: status %d, body %s", resp.StatusCode, payload)
	}
	if got := decodeWireError(t, payload).Code; got != "forbidden" {
		t.Errorf("read with a limited token: code %q, want forbidden", got)
	}

	resp, payload = call(t, fixture.handler, fixture.limited, http.MethodGet, "/v1/operations", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list with a limited token: status %d, body %s", resp.StatusCode, payload)
	}
	var listed operationsResponse
	if err := json.Unmarshal(payload, &listed); err != nil {
		t.Fatalf("list body: %v (%s)", err, payload)
	}
	if len(listed.Operations) != 0 {
		t.Errorf("list with a limited token returned %d operations, want none", len(listed.Operations))
	}

	// A running operation is not stopped by a token outside its list.
	body = `{"capability":"echo","operation":"sleep","arguments":{"seconds":30}}`
	resp, payload = call(t, fixture.handler, fixture.token, http.MethodPost, "/v1/operations", body, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("submit sleep: status %d, body %s", resp.StatusCode, payload)
	}
	running := decodeOperation(t, payload)
	awaitOperation(t, fixture, running.ID, "running")

	resp, payload = call(t, fixture.handler, fixture.limited, http.MethodPost, "/v1/operations/"+running.ID+"/cancel", "", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cancel with a limited token: status %d, body %s", resp.StatusCode, payload)
	}
	resp, payload = call(t, fixture.handler, fixture.token, http.MethodGet, "/v1/operations/"+running.ID, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read running operation: status %d, body %s", resp.StatusCode, payload)
	}
	var wrapped operationResponse
	if err := json.Unmarshal(payload, &wrapped); err != nil {
		t.Fatalf("read running operation: %v (%s)", err, payload)
	}
	if wrapped.Operation.Status != "running" {
		t.Errorf("the refused cancel changed the status to %q, want running", wrapped.Operation.Status)
	}

	resp, payload = call(t, fixture.handler, fixture.token, http.MethodPost, "/v1/operations/"+running.ID+"/cancel", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel with the full token: status %d, body %s", resp.StatusCode, payload)
	}
	if got := decodeOperation(t, payload).Status; got != "cancelled" {
		t.Errorf("cancel with the full token: status %q, want cancelled", got)
	}
}

// TestMalformedRequestBodies requires the InvalidRequest code for every
// body that is not exactly one known JSON object, and no operation.
func TestMalformedRequestBodies(t *testing.T) {
	fixture := newExecutionFixture(t)
	valid := `{"capability":"echo","operation":"say","arguments":{"message":"hello"}}`
	cases := []struct {
		name string
		body string
	}{
		{"not json", `not json`},
		{"not an object", `["echo"]`},
		{"trailing data", valid + ` {}`},
		{"unknown field", `{"capability":"echo","operation":"say","arguments":{"message":"hello"},"argument":{}}`},
		{"too large", valid + strings.Repeat(" ", maxRequestBody)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, payload := call(t, fixture.handler, fixture.token, http.MethodPost, "/v1/operations", tc.body, nil)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status %d, want 400, body %s", resp.StatusCode, payload)
			}
			if got := decodeWireError(t, payload).Code; got != "invalid_request" {
				t.Errorf("code %q, want invalid_request", got)
			}
		})
	}
	if records, err := fixture.manager.List(operations.Filter{}); err != nil {
		t.Fatalf("list: %v", err)
	} else if len(records) != 0 {
		t.Errorf("malformed requests created %d operations, want none", len(records))
	}
}

// TestMissingRequestFields reports the request envelope's own missing
// fields as InvalidRequest, not as a missing operation argument.
func TestMissingRequestFields(t *testing.T) {
	fixture := newExecutionFixture(t)
	cases := []struct {
		name string
		path string
		body string
	}{
		{"submit without capability", "/v1/operations", `{"operation":"say","arguments":{"message":"hello"}}`},
		{"submit without operation", "/v1/operations", `{"capability":"echo","arguments":{"message":"hello"}}`},
		{"validate without capability", "/v1/operations/validate", `{"operation":"say"}`},
		{"validate without operation", "/v1/operations/validate", `{"capability":"echo"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, payload := call(t, fixture.handler, fixture.token, http.MethodPost, tc.path, tc.body, nil)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status %d, want 400, body %s", resp.StatusCode, payload)
			}
			if got := decodeWireError(t, payload).Code; got != "invalid_request" {
				t.Errorf("code %q, want invalid_request", got)
			}
		})
	}
}

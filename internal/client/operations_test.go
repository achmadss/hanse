package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	herrors "github.com/achmadss/hanse/internal/errors"
)

// The fixture records below follow internal/api/execution.go. Every value
// is a placeholder.

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
  "started_at": "2026-10-30T08:00:01Z"
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

// TestOperationList covers GET /v1/operations with and without filters.
func TestOperationList(t *testing.T) {
	t.Run("no filter", func(t *testing.T) {
		srv := newTestServer(t, map[string]http.HandlerFunc{
			"/v1/operations": func(w http.ResponseWriter, r *http.Request) {
				if r.URL.RawQuery != "" {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"code":"invalid_request","message":"an unexpected filter was sent"}`))
					return
				}
				jsonRoute(`{"operations":[`+fixtureOperationRunning+`]}`)(w, r)
			},
		})
		list, err := newTestClient(t, srv).Operations(context.Background(), OperationFilter{})
		if err != nil {
			t.Fatalf("operations: %v", err)
		}
		if len(list.Operations) != 1 {
			t.Fatalf("operations: got %d records, want 1", len(list.Operations))
		}
		record := list.Operations[0]
		if record.ID != "op_01J9Z3" || record.Status != "running" || record.Executor != "node-a" {
			t.Errorf("operations: got %+v", record)
		}
		if record.StartedAt == nil || record.FinishedAt != nil {
			t.Errorf("operations: got started %v, finished %v", record.StartedAt, record.FinishedAt)
		}
	})

	t.Run("every filter", func(t *testing.T) {
		want := "capability=pc&executor=node-a&operation=up&requester=alice&status=running"
		srv := newTestServer(t, map[string]http.HandlerFunc{
			"/v1/operations": func(w http.ResponseWriter, r *http.Request) {
				if got := r.URL.Query().Encode(); got != want {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"code":"invalid_request","message":"the filters do not match"}`))
					return
				}
				jsonRoute(`{"operations":[]}`)(w, r)
			},
		})
		list, err := newTestClient(t, srv).Operations(context.Background(), OperationFilter{
			Capability: "pc",
			Operation:  "up",
			Executor:   "node-a",
			Status:     "running",
			Requester:  "alice",
		})
		if err != nil {
			t.Fatalf("operations: %v", err)
		}
		if len(list.Operations) != 0 {
			t.Errorf("operations: got %d records, want none", len(list.Operations))
		}
	})
}

// TestOperationListNullBecomesEmpty requires an empty slice rather than
// null, so the CLI always prints the same JSON type.
func TestOperationListNullBecomesEmpty(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"/v1/operations": jsonRoute(`{"operations":null}`),
	})
	list, err := newTestClient(t, srv).Operations(context.Background(), OperationFilter{})
	if err != nil {
		t.Fatalf("operations: %v", err)
	}
	if list.Operations == nil {
		t.Error("operations: got nil, want an empty slice")
	}
}

// TestOperationGet covers GET /v1/operations/{id}.
func TestOperationGet(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"/v1/operations/op_01J9Z4": jsonRoute(`{"operation":` + fixtureOperationCompleted + `}`),
	})
	record, err := newTestClient(t, srv).Operation(context.Background(), "op_01J9Z4")
	if err != nil {
		t.Fatalf("operation: %v", err)
	}
	if record.ID != "op_01J9Z4" || record.Status != "completed" {
		t.Errorf("operation: got %+v", record)
	}
	if record.FinishedAt == nil || record.StartedAt == nil {
		t.Errorf("operation: got started %v, finished %v", record.StartedAt, record.FinishedAt)
	}
	if !strings.Contains(string(record.Result), `"removed"`) {
		t.Errorf("operation: result %s", record.Result)
	}
}

// TestOperationCancel covers POST /v1/operations/{id}/cancel. The response
// is the record itself, without the get endpoint's envelope.
func TestOperationCancel(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"/v1/operations/op_01J9Z3/cancel": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				_, _ = w.Write([]byte(`{"code":"internal","message":"the cancel request used the wrong method"}`))
				return
			}
			jsonRoute(fixtureOperationCancelled)(w, r)
		},
	})
	record, err := newTestClient(t, srv).CancelOperation(context.Background(), "op_01J9Z3")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if record.Status != "cancelled" || record.FinishedAt == nil {
		t.Errorf("cancel: got %+v", record)
	}
}

// TestOperationUnknown maps an unknown ID to the shared code.
func TestOperationUnknown(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"/v1/operations/op_99ZZZZ": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"unknown_operation","message":"operation \"op_99ZZZZ\" does not exist"}`))
		},
	})
	_, err := newTestClient(t, srv).Operation(context.Background(), "op_99ZZZZ")
	if !errors.Is(err, herrors.UnknownOperation) {
		t.Fatalf("operation: error %v, want the UnknownOperation code", err)
	}
	if got := herrors.ExitCode(err); got != 21 {
		t.Errorf("operation: exit code %d, want 21", got)
	}
}

// TestOperationInvalidID requires a hostile ID to fail before any request.
func TestOperationInvalidID(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv)

	for _, id := range []string{"../version", "a/b", "..", ""} {
		if _, err := c.Operation(context.Background(), id); !errors.Is(err, herrors.InvalidArguments) {
			t.Errorf("Operation(%q): error %v, want InvalidArguments", id, err)
		}
		if _, err := c.CancelOperation(context.Background(), id); !errors.Is(err, herrors.InvalidArguments) {
			t.Errorf("CancelOperation(%q): error %v, want InvalidArguments", id, err)
		}
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("sent %d requests with a hostile identifier, want 0", got)
	}
}

// submitBody is the request body the submit fixture receives.
type submitBody struct {
	Capability string                     `json:"capability"`
	Operation  string                     `json:"operation"`
	Arguments  map[string]json.RawMessage `json:"arguments"`
	Executor   string                     `json:"executor"`
	Elevation  *Elevation                 `json:"elevation"`
}

// TestSubmit covers POST /v1/operations: the encoded request, the accepted
// record of an asynchronous operation, and the omitted optional fields.
func TestSubmit(t *testing.T) {
	var received submitBody
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"/v1/operations": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				_, _ = w.Write([]byte(`{"code":"internal","message":"the submit used the wrong method"}`))
				return
			}
			if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"code":"invalid_request","message":"the request body is malformed"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(fixtureOperationRunning))
		},
	})

	record, err := newTestClient(t, srv).Submit(context.Background(), SubmitRequest{
		Capability: "pc",
		Operation:  "up",
		Arguments:  json.RawMessage(`{"node":"node-c"}`),
		Executor:   "node-b",
		Elevation:  &Elevation{Password: "placeholder-elevation-password"},
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if record.ID != "op_01J9Z3" || record.Status != "running" {
		t.Errorf("submit: got %+v", record)
	}
	if received.Capability != "pc" || received.Operation != "up" || received.Executor != "node-b" {
		t.Errorf("submit: request %+v", received)
	}
	if !strings.Contains(string(received.Arguments["node"]), "node-c") {
		t.Errorf("submit: arguments %v", received.Arguments)
	}
	if received.Elevation == nil || received.Elevation.Password != "placeholder-elevation-password" {
		t.Errorf("submit: elevation %+v", received.Elevation)
	}
}

// TestSubmitOmitsOptionalFields requires an empty executor and no elevation
// to stay out of the request body.
func TestSubmitOmitsOptionalFields(t *testing.T) {
	raw := map[string]json.RawMessage{}
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"/v1/operations": func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"code":"invalid_request","message":"the request body is malformed"}`))
				return
			}
			jsonRoute(fixtureOperationCompleted)(w, r)
		},
	})
	record, err := newTestClient(t, srv).Submit(context.Background(), SubmitRequest{
		Capability: "demo",
		Operation:  "clean",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if record.Status != "completed" {
		t.Errorf("submit: got %+v", record)
	}
	for _, field := range []string{"executor", "elevation", "arguments"} {
		if _, ok := raw[field]; ok {
			t.Errorf("submit: the request carries an empty %q", field)
		}
	}
}

// TestSubmitConflict requires the running operation's ID to reach the
// caller through the error details.
func TestSubmitConflict(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"/v1/operations": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"operation_conflict","message":"operation \"op_running\" is already running",` +
				`"details":{"operation_id":"op_running"}}`))
		},
	})
	_, err := newTestClient(t, srv).Submit(context.Background(), SubmitRequest{Capability: "pc", Operation: "up"})
	if !errors.Is(err, herrors.OperationConflict) {
		t.Fatalf("submit: error %v, want the OperationConflict code", err)
	}
	var apiErr *herrors.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("submit: error %v is not an *herrors.Error", err)
	}
	if apiErr.Details["operation_id"] != "op_running" {
		t.Errorf("submit: details %v, want the running operation ID", apiErr.Details)
	}
	if got := herrors.ExitCode(err); got != 32 {
		t.Errorf("submit: exit code %d, want 32", got)
	}
}

// TestCode maps a wire value to its shared code.
func TestCode(t *testing.T) {
	code, ok := Code("handler_exit")
	if !ok || code != herrors.HandlerExit {
		t.Errorf("Code(handler_exit) = %v, %t", code, ok)
	}
	if _, ok := Code("not_a_code"); ok {
		t.Error("Code(not_a_code) reported a known code")
	}
}

package client

import (
	"context"
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

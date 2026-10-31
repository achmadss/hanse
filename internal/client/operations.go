package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"

	herrors "github.com/achmadss/hanse/internal/errors"
)

// The types in this file mirror the operation response shapes of
// internal/api/execution.go. The field order follows the Gateway's, which
// keeps the CLI's JSON output stable. A record carries no secret value:
// the arguments are not stored, and an elevation password is never
// recorded.

// OperationError is the recorded reason an operation failed or was
// cancelled.
type OperationError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// OperationRecord is one operation record.
type OperationRecord struct {
	ID             string          `json:"id"`
	Capability     string          `json:"capability"`
	Operation      string          `json:"operation"`
	Requester      string          `json:"requester"`
	Executor       string          `json:"executor"`
	ExecutorSource string          `json:"executor_source"`
	Status         string          `json:"status"`
	Mode           string          `json:"mode"`
	CreatedAt      time.Time       `json:"created_at"`
	StartedAt      *time.Time      `json:"started_at,omitempty"`
	FinishedAt     *time.Time      `json:"finished_at,omitempty"`
	Result         json.RawMessage `json:"result,omitempty"`
	Summary        string          `json:"summary,omitempty"`
	Error          *OperationError `json:"error,omitempty"`
}

// OperationList is the body of GET /v1/operations.
type OperationList struct {
	Operations []OperationRecord `json:"operations"`
}

// Elevation is the password of a privileged operation
// (docs/architecture.md section 9 and docs/configuration.md section 6.2).
// The Gateway passes it to the handler and never stores or returns it, so a
// client sends it once and holds it no longer than the request needs.
type Elevation struct {
	Password string `json:"password"`
}

// SubmitRequest is the body of POST /v1/operations. Arguments is the raw
// JSON object, so a large integer keeps every digit. An empty Executor lets
// the Gateway resolve the default one.
type SubmitRequest struct {
	Capability string          `json:"capability"`
	Operation  string          `json:"operation"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
	Executor   string          `json:"executor,omitempty"`
	Elevation  *Elevation      `json:"elevation,omitempty"`
}

// Submit returns POST /v1/operations: the new operation record, already
// finished for a synchronous operation and still running for an
// asynchronous one. A rejected request comes back as an *herrors.Error with
// the Gateway's code and details, so its CLI exit code is the documented
// one; a concurrency conflict carries the running operation's ID in the
// "operation_id" detail.
func (c *Client) Submit(ctx context.Context, req SubmitRequest) (OperationRecord, error) {
	var out OperationRecord
	if err := c.postJSON(ctx, &out, req, "v1", "operations"); err != nil {
		return OperationRecord{}, err
	}
	return out, nil
}

// Code returns the shared error code for a wire value, such as the code of
// an operation record's error, and reports whether the value is one of
// them.
func Code(wire string) (*herrors.Code, bool) {
	code, ok := codesByWire[wire]
	return code, ok
}

// OperationRecordEnvelope is the body of GET /v1/operations/{id}. The
// cancel endpoint answers with the record itself, without an envelope.
type OperationRecordEnvelope struct {
	Operation OperationRecord `json:"operation"`
}

// OperationFilter holds the query filters of GET /v1/operations, the same
// five the Gateway accepts plus the list size. An empty field is not sent,
// and a zero Limit leaves the Gateway default.
type OperationFilter struct {
	Capability string
	Operation  string
	Executor   string
	Status     string
	Requester  string
	Limit      int
}

// query renders the filter as request query parameters. url.Values sorts
// its keys, so the request URL is deterministic.
func (f OperationFilter) query() url.Values {
	query := url.Values{}
	if f.Capability != "" {
		query.Set("capability", f.Capability)
	}
	if f.Operation != "" {
		query.Set("operation", f.Operation)
	}
	if f.Executor != "" {
		query.Set("executor", f.Executor)
	}
	if f.Status != "" {
		query.Set("status", f.Status)
	}
	if f.Requester != "" {
		query.Set("requester", f.Requester)
	}
	if f.Limit > 0 {
		query.Set("limit", strconv.Itoa(f.Limit))
	}
	return query
}

// Operations returns GET /v1/operations. The Gateway sends the list newest
// first: by creation time descending, then by ID descending. The client
// keeps the Gateway order.
func (c *Client) Operations(ctx context.Context, filter OperationFilter) (OperationList, error) {
	var out OperationList
	err := c.do(ctx, http.MethodGet, &out, filter.query(), nil, "v1", "operations")
	normalize(&out.Operations)
	return out, err
}

// Operation returns GET /v1/operations/{id}.
func (c *Client) Operation(ctx context.Context, id string) (OperationRecord, error) {
	var out OperationRecordEnvelope
	err := c.do(ctx, http.MethodGet, &out, nil, nil, "v1", "operations", id)
	return out.Operation, err
}

// CancelOperation returns POST /v1/operations/{id}/cancel: the record as it
// stands after the cancel request.
func (c *Client) CancelOperation(ctx context.Context, id string) (OperationRecord, error) {
	var out OperationRecord
	err := c.do(ctx, http.MethodPost, &out, nil, nil, "v1", "operations", id, "cancel")
	return out, err
}

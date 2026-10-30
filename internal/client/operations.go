package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

// OperationRecordEnvelope is the body of GET /v1/operations/{id}. The
// cancel endpoint answers with the record itself, without an envelope.
type OperationRecordEnvelope struct {
	Operation OperationRecord `json:"operation"`
}

// OperationFilter holds the query filters of GET /v1/operations, the same
// five the Gateway accepts. An empty field is not sent.
type OperationFilter struct {
	Capability string
	Operation  string
	Executor   string
	Status     string
	Requester  string
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
	return query
}

// Operations returns GET /v1/operations.
func (c *Client) Operations(ctx context.Context, filter OperationFilter) (OperationList, error) {
	var out OperationList
	err := c.operationRequest(ctx, http.MethodGet, &out, filter.query(), "v1", "operations")
	normalize(&out.Operations)
	return out, err
}

// Operation returns GET /v1/operations/{id}.
func (c *Client) Operation(ctx context.Context, id string) (OperationRecord, error) {
	var out OperationRecordEnvelope
	err := c.operationRequest(ctx, http.MethodGet, &out, nil, "v1", "operations", id)
	return out.Operation, err
}

// CancelOperation returns POST /v1/operations/{id}/cancel: the record as it
// stands after the cancel request.
func (c *Client) CancelOperation(ctx context.Context, id string) (OperationRecord, error) {
	var out OperationRecord
	err := c.operationRequest(ctx, http.MethodPost, &out, nil, "v1", "operations", id, "cancel")
	return out, err
}

// operationRequest performs one operation request and decodes its JSON body
// into out. The path segments go through endpoint, so an ID cannot reach
// another endpoint, and the query never changes the path. The request body
// is empty: neither list, get, nor cancel sends one.
func (c *Client) operationRequest(ctx context.Context, method string, out any, query url.Values, segments ...string) error {
	endpoint, err := c.endpoint(segments)
	if err != nil {
		return err
	}
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return herrors.New(herrors.Internal, "the request could not be built: "+err.Error(), nil)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("cannot reach the Gateway at %s: %v", c.baseURL.Redacted(), err), nil)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("the response from %s could not be read: %v", c.baseURL.Redacted(), err), nil)
	}
	if len(body) > maxResponseBytes {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("the response from %s is larger than %d bytes", c.baseURL.Redacted(), maxResponseBytes), nil)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return errorFromResponse(resp.StatusCode, body)
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("the Gateway at %s returned an invalid JSON response: %v", c.baseURL.Redacted(), err), nil)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("the Gateway at %s returned an invalid JSON response: trailing data", c.baseURL.Redacted()), nil)
	}
	return nil
}

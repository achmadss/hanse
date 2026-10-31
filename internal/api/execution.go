package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/achmadss/hanse/internal/auth"
	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/operations"
	"github.com/achmadss/hanse/internal/registry"
)

// submitInput is the body of POST /v1/operations and
// POST /v1/operations/validate (docs/architecture.md section 11).
type submitInput struct {
	Capability string          `json:"capability"`
	Operation  string          `json:"operation"`
	Arguments  json.RawMessage `json:"arguments"`
	Executor   string          `json:"executor"`
	// Elevation is the optional password of a privileged operation. It is
	// passed to dispatch and never stored or returned.
	Elevation *elevationInput `json:"elevation"`
}

// elevationInput is the elevation object of a request.
type elevationInput struct {
	Password string `json:"password"`
}

// failureView is the recorded reason an operation failed or was cancelled.
type failureView struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// operationView is one operation record. It holds no secret value: the
// elevation password is never recorded, and the arguments are not stored.
type operationView struct {
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
	Error          *failureView    `json:"error,omitempty"`
}

// operationsResponse is the body of GET /v1/operations.
type operationsResponse struct {
	Operations []operationView `json:"operations"`
}

// operationResponse is the body of GET /v1/operations/{id}.
type operationResponse struct {
	Operation operationView `json:"operation"`
}

// validateResponse is the body of POST /v1/operations/validate. The
// endpoint never runs anything; it reports the executor the request would
// use.
type validateResponse struct {
	Valid          bool   `json:"valid"`
	Capability     string `json:"capability"`
	Operation      string `json:"operation"`
	Mode           string `json:"mode"`
	Executor       string `json:"executor"`
	ExecutorSource string `json:"executor_source"`
}

// submitOperation answers POST /v1/operations: it authenticates through
// the middleware, authorizes the capability, then leaves validation,
// idempotency, concurrency, and dispatch to the operations manager.
func (s *server) submitOperation(w http.ResponseWriter, r *http.Request) {
	id, ok := s.identity(w, r)
	if !ok {
		return
	}
	body, ok := decodeSubmit(w, r)
	if !ok {
		return
	}
	if err := id.Authorize(body.Capability, body.Operation); err != nil {
		writeError(w, err)
		return
	}
	ops, ok := s.operations(w)
	if !ok {
		return
	}
	rec, err := ops.Submit(operations.Request{
		Capability:     body.Capability,
		Operation:      body.Operation,
		Arguments:      body.Arguments,
		Executor:       body.Executor,
		Elevation:      elevationOf(body.Elevation),
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
		Requester:      id.Name,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	if rec.Mode == registry.ModeSync {
		if err := finalError(rec); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, operationViewOf(rec))
		return
	}
	writeJSON(w, http.StatusAccepted, operationViewOf(rec))
}

// validateOperation answers POST /v1/operations/validate. It runs the same
// lookup, argument validation, and executor resolution as a submit, and
// creates no operation.
func (s *server) validateOperation(w http.ResponseWriter, r *http.Request) {
	id, ok := s.identity(w, r)
	if !ok {
		return
	}
	body, ok := decodeSubmit(w, r)
	if !ok {
		return
	}
	if err := id.Authorize(body.Capability, body.Operation); err != nil {
		writeError(w, err)
		return
	}
	ops, ok := s.operations(w)
	if !ok {
		return
	}
	check, err := ops.Validate(operations.Request{
		Capability: body.Capability,
		Operation:  body.Operation,
		Arguments:  body.Arguments,
		Executor:   body.Executor,
		Elevation:  elevationOf(body.Elevation),
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, validateResponse{
		Valid:          true,
		Capability:     body.Capability,
		Operation:      body.Operation,
		Mode:           check.Mode,
		Executor:       check.Executor,
		ExecutorSource: check.ExecutorSource,
	})
}

// getOperation answers GET /v1/operations/{id}. A token may read only an
// operation its allow list covers (docs/architecture.md section 15).
func (s *server) getOperation(w http.ResponseWriter, r *http.Request) {
	id, ok := s.identity(w, r)
	if !ok {
		return
	}
	ops, ok := s.operations(w)
	if !ok {
		return
	}
	rec, err := ops.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if err := id.Authorize(rec.Capability, rec.Operation); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, operationResponse{Operation: operationViewOf(rec)})
}

// listOperations answers GET /v1/operations. The filters are the query
// parameters capability, operation, executor, status, and requester. The
// list drops every operation the token's allow list does not cover.
func (s *server) listOperations(w http.ResponseWriter, r *http.Request) {
	id, ok := s.identity(w, r)
	if !ok {
		return
	}
	ops, ok := s.operations(w)
	if !ok {
		return
	}
	query := r.URL.Query()
	filter := operations.Filter{
		Capability: query.Get("capability"),
		Operation:  query.Get("operation"),
		Executor:   query.Get("executor"),
		Status:     operations.State(query.Get("status")),
		Requester:  query.Get("requester"),
	}
	if filter.Status != "" && !filter.Status.Valid() {
		writeError(w, herrors.New(herrors.InvalidArguments,
			"the status filter is not a known operation state",
			map[string]any{"errors": []any{map[string]any{
				"path": "status", "rule": "enum",
				"message": "must be queued, running, completed, failed, or cancelled",
			}}}))
		return
	}
	records, err := ops.List(filter)
	if err != nil {
		writeError(w, err)
		return
	}
	out := operationsResponse{Operations: make([]operationView, 0, len(records))}
	for _, rec := range records {
		if err := id.Authorize(rec.Capability, rec.Operation); err != nil {
			continue
		}
		out.Operations = append(out.Operations, operationViewOf(rec))
	}
	writeJSON(w, http.StatusOK, out)
}

// cancelOperation answers POST /v1/operations/{id}/cancel. The token is
// authorized against the stored record before anything changes.
func (s *server) cancelOperation(w http.ResponseWriter, r *http.Request) {
	id, ok := s.identity(w, r)
	if !ok {
		return
	}
	ops, ok := s.operations(w)
	if !ok {
		return
	}
	rec, err := ops.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if err := id.Authorize(rec.Capability, rec.Operation); err != nil {
		writeError(w, err)
		return
	}
	rec, err = ops.Cancel(rec.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, operationViewOf(rec))
}

// identity returns the authenticated identity of the request, or writes
// the AuthenticationFailed error. The server normally wraps this handler
// with auth.Store.Middleware; the check keeps an unwrapped handler closed.
func (s *server) identity(w http.ResponseWriter, r *http.Request) (*auth.Identity, bool) {
	id := auth.IdentityFrom(r)
	if id == nil {
		writeError(w, herrors.New(herrors.AuthenticationFailed, "", nil))
		return nil, false
	}
	return id, true
}

// operations returns the manager, or writes the Internal error when the
// Gateway has none.
func (s *server) operations(w http.ResponseWriter) (*operations.Manager, bool) {
	if s.ops == nil {
		writeError(w, herrors.New(herrors.Internal,
			"the operation manager is not available", nil))
		return nil, false
	}
	return s.ops, true
}

// maxRequestBody is the largest accepted request body: 1 MiB, the same
// value as the default handler response limit. gateway.yaml has no request
// size limit of its own, so the Gateway picks this cap
// (docs/architecture.md section 15, Limits).
const maxRequestBody = 1 << 20

// decodeSubmit reads one submit or validate body. A malformed body is the
// InvalidRequest code, HTTP 400 (docs/architecture.md section 14): a body
// that is not a JSON object, trailing data after the object, an unknown
// top-level field, or a body larger than maxRequestBody. Argument-schema
// failures stay InvalidArguments.
func decodeSubmit(w http.ResponseWriter, r *http.Request) (submitInput, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var body submitInput
	if err := dec.Decode(&body); err != nil {
		writeInvalidRequest(w, err)
		return submitInput{}, false
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		writeInvalidRequest(w, errors.New("trailing data after the request object"))
		return submitInput{}, false
	}
	return body, true
}

// writeInvalidRequest writes the InvalidRequest error for a malformed
// request body. The reason is the decoder's text, which names a field or a
// position and never a value.
func writeInvalidRequest(w http.ResponseWriter, err error) {
	writeError(w, herrors.New(herrors.InvalidRequest,
		"the request body is malformed",
		map[string]any{"reason": err.Error()}))
}

// elevationOf converts the request's elevation object, or returns nil.
func elevationOf(in *elevationInput) *operations.Elevation {
	if in == nil {
		return nil
	}
	return &operations.Elevation{Password: in.Password}
}

// finalError maps a finished synchronous operation that did not complete
// to the API error object. A handler's own error rides inside the
// HandlerFailed details, docs/architecture.md section 14.
func finalError(rec operations.Record) error {
	details := map[string]any{"operation_id": rec.ID}
	switch rec.Status {
	case operations.StateCompleted:
		return nil
	case operations.StateCancelled:
		return herrors.New(herrors.Cancelled, "", details)
	}
	if rec.Failure != nil {
		switch rec.Failure.Code {
		case herrors.Timeout.Name():
			return herrors.New(herrors.Timeout, rec.Failure.Message, details)
		case herrors.ElevationRequired.Name():
			return herrors.New(herrors.ElevationRequired, rec.Failure.Message, details)
		case herrors.ElevationDenied.Name():
			return herrors.New(herrors.ElevationDenied, rec.Failure.Message, details)
		}
		details["code"] = rec.Failure.Code
		details["message"] = rec.Failure.Message
		if len(rec.Failure.Details) > 0 {
			details["details"] = rec.Failure.Details
		}
	}
	return herrors.New(herrors.HandlerFailed, "the operation failed", details)
}

// operationViewOf renders one record for a response. The record carries no
// secret value, so nothing here needs redaction.
func operationViewOf(rec operations.Record) operationView {
	out := operationView{
		ID:             rec.ID,
		Capability:     rec.Capability,
		Operation:      rec.Operation,
		Requester:      rec.Requester,
		Executor:       rec.Executor,
		ExecutorSource: rec.ExecutorSource,
		Status:         string(rec.Status),
		Mode:           rec.Mode,
		CreatedAt:      rec.CreatedAt,
		StartedAt:      timeOf(rec.StartedAt),
		FinishedAt:     timeOf(rec.FinishedAt),
		Result:         rec.Result,
		Summary:        rec.Summary,
	}
	if rec.Failure != nil {
		out.Error = &failureView{
			Code:    rec.Failure.Code,
			Message: rec.Failure.Message,
			Details: rec.Failure.Details,
		}
	}
	return out
}

// timeOf returns a pointer to t, or nil when t is the zero value.
func timeOf(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

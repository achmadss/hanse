package runtime

import (
	"encoding/json"
	"time"
)

// ContractV1 is the handler contract version the builtin runtime speaks,
// docs/handler-contract.md section 13.
const ContractV1 = "hanse.handler/v1"

// Request kinds, handler contract section 2.
const (
	// KindOperation is `kind` of an operation run.
	KindOperation = "operation"
	// KindOptions is `kind` of an option provider call.
	KindOptions = "options"
)

// Response statuses, handler contract section 3.
const (
	// StatusOK is a successful run.
	StatusOK = "ok"
	// StatusError is a failed run. Response.Err must be set.
	StatusError = "error"
	// StatusCancelled is a run that stopped because a client cancelled it.
	StatusCancelled = "cancelled"
)

// Node is one node object from the request envelope, handler contract
// section 2.1. It holds only non-secret configuration.
type Node struct {
	ID         string
	Name       string
	OS         string
	Arch       string
	Address    string
	Attributes map[string]any
}

// Request is the request envelope the Gateway hands to a builtin handler,
// handler contract section 2. Builtin handlers receive it as a Go value;
// the JSON shapes are the contract's.
type Request struct {
	// Contract is `hanse.handler/v1`. Run rejects any other non-empty value.
	Contract string
	// Kind is KindOperation or KindOptions.
	Kind string
	// ID is the call ID: the operation ID, or an `opt_`-prefixed value for
	// option providers.
	ID string
	// Capability and Operation name the call's operation.
	Capability string
	Operation  string
	// Field names the argument of an options call; empty for operations.
	Field string
	// Arguments are the caller's argument values. The Gateway has
	// validated them against the operation's schema.
	Arguments json.RawMessage
	// Nodes holds every node a `hanse:node` argument refers to, keyed by
	// node ID.
	Nodes map[string]Node
	// Executor is the node that runs the handler; for builtin handlers,
	// the Gateway host.
	Executor Node
	// Secrets holds the operation's declared secret values.
	Secrets map[string]string
	// Deadline is the time by which the handler must finish. Handlers
	// honour the context and this value for early stops.
	Deadline time.Time
}

// ErrInfo is the error object of a failed response, handler contract
// section 3. The code belongs to the capability that runs the handler.
type ErrInfo struct {
	Code    string
	Message string
	Details map[string]any
}

// Response is the response envelope of one call, handler contract
// section 3.
type Response struct {
	// Status is StatusOK, StatusError, or StatusCancelled.
	Status string
	// Result carries the operation's result. JSON is the contract's
	// exchange format for builtin results too, so clients get identical
	// output no matter which runtime ran the operation.
	Result json.RawMessage
	// Summary is one short line for people.
	Summary string
	// Err is set exactly when Status is StatusError.
	Err *ErrInfo
}

// marshal builds a response with one status, an optional result, and an
// optional summary. The error return is a JSON encoding problem of result.
func marshal(status string, result any, summary string) (Response, error) {
	resp := Response{Status: status, Summary: summary}
	if result != nil {
		blob, err := json.Marshal(result)
		if err != nil {
			return Response{}, err
		}
		resp.Result = blob
	}
	return resp, nil
}

// OK returns an ok response. A nil result stays absent.
func OK(result any, summary string) (Response, error) {
	return marshal(StatusOK, result, summary)
}

// Cancelled returns a cancelled response. The result argument can describe
// the partial work.
func Cancelled(result any, summary string) (Response, error) {
	return marshal(StatusCancelled, result, summary)
}

// Fail returns an error response. The code is the capability's own
// snake_case code, not the Gateway's herrors set.
func Fail(code, message string, details map[string]any) Response {
	return Response{
		Status: StatusError,
		Err:    &ErrInfo{Code: code, Message: message, Details: details},
	}
}

package client

import (
	"context"
	"encoding/json"
	"net/http"
)

// The types in this file mirror the describe response shape of
// docs/architecture.md section 11.1 and the body of
// POST /v1/operations/validate. internal/api keeps its response bodies
// unexported, so the client owns the wire contract it decodes. Field order
// follows the documented key order, which also keeps the CLI's JSON output
// stable.

// Describe is the body of
// GET /v1/operations/{capability}/{operation}/describe: everything a client
// needs to build a valid request.
type Describe struct {
	Capability       string                        `json:"capability"`
	Operation        string                        `json:"operation"`
	Description      string                        `json:"description"`
	Mode             string                        `json:"mode"`
	Privileged       bool                          `json:"privileged"`
	Schema           any                           `json:"schema"`
	Required         []string                      `json:"required"`
	Options          []DescribeOption              `json:"options"`
	NodeFilters      map[string]DescribeNodeFilter `json:"node_filters"`
	Executors        []DescribeExecutor            `json:"executors"`
	DefaultExecutor  string                        `json:"default_executor"`
	ExecutorRequired bool                          `json:"executor_required"`
}

// DescribeOption points at the option provider of one argument.
type DescribeOption struct {
	Field    string `json:"field"`
	Provider string `json:"provider"`
}

// DescribeNodeFilter is the filter of one `hanse:node` argument
// (docs/configuration.md section 6.4). An empty field means no restriction.
type DescribeNodeFilter struct {
	OS         []string `json:"os,omitempty"`
	Arch       []string `json:"arch,omitempty"`
	Roles      []string `json:"roles,omitempty"`
	Attributes []string `json:"attributes,omitempty"`
}

// DescribeExecutor is one eligible executor. Status is the node's current
// connection state and is omitted when the Gateway has no node registry for
// it. CanElevate is false when the Gateway will refuse elevation for a
// privileged operation on this executor. It is nil when the Gateway did not
// send the field, as a Gateway older than the field does: the client then
// leaves the decision to the Gateway.
type DescribeExecutor struct {
	ID         string `json:"id"`
	Status     string `json:"status,omitempty"`
	Default    bool   `json:"default"`
	CanElevate *bool  `json:"can_elevate,omitempty"`
}

// ValidateRequest is the body of POST /v1/operations/validate. The endpoint
// runs the same lookup, argument validation, and executor resolution as a
// submit, and creates no operation. Arguments is the raw JSON object, so a
// large integer keeps every digit.
type ValidateRequest struct {
	Capability string          `json:"capability"`
	Operation  string          `json:"operation"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
	Executor   string          `json:"executor,omitempty"`
}

// ValidateResult is the body of POST /v1/operations/validate: the executor
// the request would use, and its source.
type ValidateResult struct {
	Valid          bool   `json:"valid"`
	Capability     string `json:"capability"`
	Operation      string `json:"operation"`
	Mode           string `json:"mode"`
	Executor       string `json:"executor"`
	ExecutorSource string `json:"executor_source"`
}

// Describe returns GET /v1/operations/{capability}/{operation}/describe.
func (c *Client) Describe(ctx context.Context, capability, operation string) (Describe, error) {
	var out Describe
	if err := c.get(ctx, &out, "v1", "operations", capability, operation, "describe"); err != nil {
		return Describe{}, err
	}
	normalizeDescribe(&out)
	return out, nil
}

// Validate returns POST /v1/operations/validate. A rejected request comes
// back as an *herrors.Error carrying the Gateway's code and details, so its
// CLI exit code is the documented one, for example 22 for invalid arguments.
func (c *Client) Validate(ctx context.Context, req ValidateRequest) (ValidateResult, error) {
	var out ValidateResult
	if err := c.postJSON(ctx, &out, req, "v1", "operations", "validate"); err != nil {
		return ValidateResult{}, err
	}
	return out, nil
}

// postJSON performs one POST request with a JSON body and decodes its JSON
// response into out.
func (c *Client) postJSON(ctx context.Context, out any, body any, segments ...string) error {
	return c.do(ctx, http.MethodPost, out, nil, body, segments...)
}

// normalizeDescribe replaces a nil list or map with an empty one, so a
// caller always sees the same JSON type and the CLI prints [] rather than
// null.
func normalizeDescribe(d *Describe) {
	normalize(&d.Required)
	normalize(&d.Options)
	normalize(&d.Executors)
	if d.NodeFilters == nil {
		d.NodeFilters = map[string]DescribeNodeFilter{}
	}
	for field, filter := range d.NodeFilters {
		normalize(&filter.OS)
		normalize(&filter.Arch)
		normalize(&filter.Roles)
		normalize(&filter.Attributes)
		d.NodeFilters[field] = filter
	}
}

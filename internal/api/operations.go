package api

import (
	"fmt"
	"net/http"
	"sort"

	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/registry"
)

// nodeFormat is the JSON Schema format of a node argument
// (docs/configuration.md section 6.3). The registry keeps its copy
// unexported.
const nodeFormat = "hanse:node"

// executorView is one eligible executor. Status is the node's current
// connection state and is omitted when the Gateway has no node registry
// for it. CanElevate is false when the Gateway knows it will refuse
// elevation for this executor, which it does for the host node its embedded
// agent runs.
type executorView struct {
	ID         string `json:"id"`
	Status     string `json:"status,omitempty"`
	Default    bool   `json:"default"`
	CanElevate bool   `json:"can_elevate"`
}

// optionRef points at the option provider of one argument.
type optionRef struct {
	Field    string `json:"field"`
	Provider string `json:"provider"`
}

// nodeFilterView is one node filter (docs/configuration.md section 6.4).
type nodeFilterView struct {
	OS         []string `json:"os,omitempty"`
	Arch       []string `json:"arch,omitempty"`
	Roles      []string `json:"roles,omitempty"`
	Attributes []string `json:"attributes,omitempty"`
}

// describeResponse is the body of
// GET /v1/operations/{capability}/{operation}/describe: everything a
// client needs to build a valid request (docs/architecture.md section 11).
type describeResponse struct {
	Capability       string                    `json:"capability"`
	Operation        string                    `json:"operation"`
	Description      string                    `json:"description"`
	Mode             string                    `json:"mode"`
	Privileged       bool                      `json:"privileged"`
	Schema           any                       `json:"schema"`
	Required         []string                  `json:"required"`
	Options          []optionRef               `json:"options"`
	NodeFilters      map[string]nodeFilterView `json:"node_filters"`
	Executors        []executorView            `json:"executors"`
	DefaultExecutor  string                    `json:"default_executor"`
	ExecutorRequired bool                      `json:"executor_required"`
}

// getSchema answers GET /v1/operations/{capability}/{operation}/schema. It
// serves the argument schema itself, so a client can print or store it
// without unwrapping an envelope.
func (s *server) getSchema(w http.ResponseWriter, r *http.Request) {
	c, op, ok := s.lookupOperation(w, r.PathValue("capability"), r.PathValue("operation"))
	if !ok {
		return
	}
	schema, err := operationSchema(c, op)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, schema)
}

// describeOperation answers
// GET /v1/operations/{capability}/{operation}/describe.
func (s *server) describeOperation(w http.ResponseWriter, r *http.Request) {
	capID, opID := r.PathValue("capability"), r.PathValue("operation")
	c, op, ok := s.lookupOperation(w, capID, opID)
	if !ok {
		return
	}
	schema, err := operationSchema(c, op)
	if err != nil {
		writeError(w, err)
		return
	}
	routing, _ := c.RoutingOf(op.ID)
	out := describeResponse{
		Capability:      c.ID,
		Operation:       op.ID,
		Description:     op.Description,
		Mode:            op.Mode,
		Privileged:      op.Privileged,
		Schema:          schema,
		Required:        requiredArguments(op),
		Options:         optionRefs(c, op),
		NodeFilters:     nodeFilterViews(op),
		Executors:       s.executorViews(routing),
		DefaultExecutor: routing.DefaultExecutor,
		// Without a default executor the request must name one
		// (docs/architecture.md section 5).
		ExecutorRequired: routing.DefaultExecutor == "",
	}
	writeJSON(w, http.StatusOK, out)
}

// operationSchema returns the operation's argument schema as the manifest
// wrote it. Every capability must carry it: a capability built in memory
// fills Manifest.Operations, and RegisterBuiltin rejects one that does not,
// because an empty schema would accept arguments the Gateway rejects. A
// missing value is therefore an Internal error, never a silent `{}`.
func operationSchema(c *registry.Capability, op *registry.Operation) (any, error) {
	if c != nil && c.Manifest != nil {
		if spec := c.Manifest.Operations[op.ID]; spec != nil && spec.Arguments != nil {
			return spec.Arguments, nil
		}
	}
	capID := ""
	if c != nil {
		capID = c.ID
	}
	return nil, herrors.New(herrors.Internal,
		fmt.Sprintf("capability %q has no raw argument schema for operation %q", capID, op.ID), nil)
}

// requiredArguments returns the schema's required argument names, sorted.
func requiredArguments(op *registry.Operation) []string {
	if op.Schema == nil {
		return []string{}
	}
	out := append([]string{}, op.Schema.Required...)
	sort.Strings(out)
	return out
}

// optionRefs returns the operation's option providers, sorted by argument
// name.
func optionRefs(c *registry.Capability, op *registry.Operation) []optionRef {
	if c == nil || c.Manifest == nil {
		return []optionRef{}
	}
	spec := c.Manifest.Operations[op.ID]
	if spec == nil || len(spec.Options) == 0 {
		return []optionRef{}
	}
	fields := make([]string, 0, len(spec.Options))
	for field := range spec.Options {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	out := make([]optionRef, 0, len(fields))
	for _, field := range fields {
		out = append(out, optionRef{Field: field, Provider: spec.Options[field]})
	}
	return out
}

// nodeFilterViews returns the operation's node filters, keyed by argument
// name.
func nodeFilterViews(op *registry.Operation) map[string]nodeFilterView {
	out := make(map[string]nodeFilterView, len(op.NodeFilters))
	for field, filter := range op.NodeFilters {
		out[field] = nodeFilterView{
			OS:         append([]string{}, filter.OS...),
			Arch:       append([]string{}, filter.Arch...),
			Roles:      append([]string{}, filter.Roles...),
			Attributes: append([]string{}, filter.Attributes...),
		}
	}
	return out
}

// executorViews returns the eligible executors sorted by node ID, each with
// its current status when the node registry has one.
func (s *server) executorViews(routing registry.OperationRouting) []executorView {
	ids := append([]string{}, routing.Executors...)
	sort.Strings(ids)
	out := make([]executorView, 0, len(ids))
	for _, id := range ids {
		view := executorView{
			ID:         id,
			Default:    id == routing.DefaultExecutor,
			CanElevate: s.canElevate(id),
		}
		if s.reg != nil {
			if status, ok := s.reg.Nodes.Status(id); ok {
				view.Status = string(status)
			}
		}
		out = append(out, view)
	}
	return out
}

// canElevate reports whether the Gateway runs a privileged handler with
// administrator rights on one executor. The embedded agent never elevates:
// it refuses every privileged operation with the ElevationDenied code
// (docs/guides/systemd.md). A remote node decides elevation locally, so the
// Gateway leaves it enabled and the node may still refuse the run.
func (s *server) canElevate(executorID string) bool {
	return s.embeddedNode == "" || executorID != s.embeddedNode
}

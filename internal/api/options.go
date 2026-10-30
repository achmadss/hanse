package api

import (
	"fmt"
	"net/http"

	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/validation"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// optionView is one value a client may send for an argument. The shape is
// the handler contract's option object (section 8).
type optionView struct {
	Value       string `json:"value"`
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
}

// optionsResponse is the body of
// GET /v1/operations/{capability}/{operation}/options/{field}.
type optionsResponse struct {
	Options []optionView `json:"options"`
}

// listOptions answers
// GET /v1/operations/{capability}/{operation}/options/{field}. In this
// issue it answers only for `hanse:node` arguments, with the enabled nodes
// that pass the argument's node filter, sorted by node ID. Every other
// field answers InvalidArguments, HTTP 422: the code set has no separate
// "no options" code, and issues #20 and #21 add the handler-backed
// providers.
func (s *server) listOptions(w http.ResponseWriter, r *http.Request) {
	capID, opID := r.PathValue("capability"), r.PathValue("operation")
	field := r.PathValue("field")
	_, op, ok := s.lookupOperation(w, capID, opID)
	if !ok {
		return
	}
	prop, ok := schemaProperty(op, field)
	if !ok {
		writeError(w, herrors.New(herrors.InvalidArguments,
			fmt.Sprintf("%q is not an argument of %s.%s", field, capID, opID),
			map[string]any{"capability": capID, "operation": opID, "field": field}))
		return
	}
	if prop.Format == nil || prop.Format.Name != nodeFormat {
		writeError(w, herrors.New(herrors.InvalidArguments,
			fmt.Sprintf("options for argument %q of %s.%s are not available", field, capID, opID),
			map[string]any{"capability": capID, "operation": opID, "field": field}))
		return
	}
	if s.reg == nil || s.reg.Nodes == nil {
		writeError(w, errNoRegistry())
		return
	}
	filter := op.NodeFilters[field]
	options := make([]optionView, 0)
	// List sorts by node ID and returns copies, so the response order is
	// stable.
	for _, view := range s.reg.Nodes.List() {
		// A disabled node cannot be a target (docs/configuration.md
		// section 4.1), so it is never an option.
		if !view.Enabled || !validation.MatchNode(view, filter) {
			continue
		}
		options = append(options, optionView{Value: view.ID})
	}
	writeJSON(w, http.StatusOK, optionsResponse{Options: options})
}

// schemaProperty returns the compiled schema of one top-level argument of
// an operation.
func schemaProperty(op *registry.Operation, name string) (*jsonschema.Schema, bool) {
	if op.Schema == nil || op.Schema.Properties == nil {
		return nil, false
	}
	prop, ok := op.Schema.Properties[name]
	return prop, ok
}

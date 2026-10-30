package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/operations"
	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/resolver"
	"github.com/achmadss/hanse/internal/runtime"
	"github.com/achmadss/hanse/internal/validation"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// optionView is one value a client may send for an argument. The shape is
// the handler contract's option object (section 8). Value is any JSON
// value, as the contract allows.
type optionView struct {
	Value       any    `json:"value"`
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
}

// optionsResponse is the body of
// GET /v1/operations/{capability}/{operation}/options/{field}.
type optionsResponse struct {
	Options []optionView `json:"options"`
}

// OptionProvider runs one option provider call. The exec dispatcher and
// the HTTP dispatcher both satisfy it through operations.Dispatcher.
type OptionProvider interface {
	Execute(ctx context.Context, call *operations.Call) (runtime.Response, error)
}

// OptionProviders are the handler-backed option providers of a Gateway.
// Exec serves capability packages with an exec handler and HTTP serves
// packages with an http handler. A nil provider leaves its capabilities
// without handler-backed options, which the options endpoint answers the
// same way as an argument without a provider.
type OptionProviders struct {
	Exec OptionProvider
	HTTP OptionProvider
}

// listOptions answers
// GET /v1/operations/{capability}/{operation}/options/{field}.
//
// An argument with the `hanse:node` format is answered from the node
// registry: the enabled nodes that pass the argument's node filter, sorted
// by node ID. Every other argument answers through the capability's option
// provider, handler contract section 8. An argument without a provider, or
// a capability whose handler-backed provider is not configured, answers
// InvalidArguments, HTTP 422.
func (s *server) listOptions(w http.ResponseWriter, r *http.Request) {
	capID, opID := r.PathValue("capability"), r.PathValue("operation")
	field := r.PathValue("field")
	capability, op, ok := s.lookupOperation(w, capID, opID)
	if !ok {
		return
	}
	prop, ok := schemaProperty(op, field)
	if !ok {
		writeError(w, noOptionsError(capID, opID, field))
		return
	}
	if isNodeArgument(prop) {
		s.writeNodeOptions(w, field, op)
		return
	}
	provider := s.optionProvider(capability)
	if !hasOptionProvider(capability, op.ID, field) || provider == nil {
		writeError(w, noOptionsError(capID, opID, field))
		return
	}
	req, err := s.optionsRequest(r, capability, op, field)
	if err != nil {
		writeError(w, err)
		return
	}
	resp, err := provider.Execute(r.Context(), &operations.Call{Request: req})
	if err != nil {
		writeError(w, err)
		return
	}
	if resp.Status == runtime.StatusError {
		writeError(w, optionsFailure(resp.Err))
		return
	}
	if resp.Status != runtime.StatusOK {
		writeError(w, herrors.New(herrors.HandlerOutputInvalid,
			fmt.Sprintf("the option provider of %s.%s returned status %q", capID, opID, resp.Status), nil))
		return
	}
	options, err := decodeOptions(resp.Result)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, optionsResponse{Options: options})
}

// writeNodeOptions answers a `hanse:node` argument from the node registry.
func (s *server) writeNodeOptions(w http.ResponseWriter, field string, op *registry.Operation) {
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

// noOptionsError is the answer for an argument without an available
// provider. The code set has no separate "no options" code.
func noOptionsError(capID, opID, field string) error {
	return herrors.New(herrors.InvalidArguments,
		fmt.Sprintf("options for argument %q of %s.%s are not available", field, capID, opID),
		map[string]any{"capability": capID, "operation": opID, "field": field})
}

// hasOptionProvider reports whether the manifest names a handler-backed
// provider for one argument of an operation.
func hasOptionProvider(capability *registry.Capability, operationID, field string) bool {
	if capability == nil || capability.Manifest == nil || capability.Manifest.Operations == nil {
		return false
	}
	spec := capability.Manifest.Operations[operationID]
	return spec != nil && spec.Options[field] == "provider"
}

// optionProvider returns the handler-backed provider of one capability.
func (s *server) optionProvider(capability *registry.Capability) OptionProvider {
	if capability == nil || capability.Manifest == nil {
		return nil
	}
	switch capability.Manifest.Handler.Kind {
	case registry.KindExec:
		return s.optionProviders.Exec
	case registry.KindHTTP:
		return s.optionProviders.HTTP
	default:
		return nil
	}
}

// optionsRequest builds the request envelope of one provider call. The
// executor is the one the operation would use, chosen by the normal
// executor resolution, and the arguments are the query values that name a
// top-level argument of the schema. The call has no deadline here: the
// provider applies the documented option provider timeout.
func (s *server) optionsRequest(r *http.Request, capability *registry.Capability, op *registry.Operation, field string) (*runtime.Request, error) {
	if s.reg == nil || s.reg.Nodes == nil {
		return nil, errNoRegistry()
	}
	resolved, err := resolver.New(s.reg, s.reg.Nodes).Resolve(capability.ID, op.ID, "")
	if err != nil {
		return nil, err
	}
	view, ok := s.reg.Nodes.Get(resolved.Executor)
	if !ok {
		return nil, herrors.New(herrors.InvalidExecutor,
			fmt.Sprintf("executor %q is not a node in nodes.yaml", resolved.Executor), nil)
	}
	arguments, err := queryArguments(r, op)
	if err != nil {
		return nil, err
	}
	return &runtime.Request{
		Contract:   runtime.ContractV1,
		Kind:       runtime.KindOptions,
		ID:         newOptionCallID(),
		Capability: capability.ID,
		Operation:  op.ID,
		Field:      field,
		Arguments:  arguments,
		Nodes:      map[string]runtime.Node{},
		Executor:   envelopeNode(view),
		Secrets:    map[string]string{},
	}, nil
}

// queryArguments collects the options request's query values that name a
// top-level argument of the schema. A value that parses as JSON keeps its
// type; anything else is a string.
func queryArguments(r *http.Request, op *registry.Operation) (json.RawMessage, error) {
	values := map[string]any{}
	query := r.URL.Query()
	for name := range query {
		if _, ok := schemaProperty(op, name); !ok {
			continue
		}
		raw := query.Get(name)
		var value any
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&value); err != nil {
			value = raw
		}
		values[name] = value
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return nil, herrors.New(herrors.Internal,
			fmt.Sprintf("the options arguments cannot be encoded: %s", err), nil)
	}
	return encoded, nil
}

// decodeOptions parses the options result of an ok response, handler
// contract section 14.3.
func decodeOptions(result json.RawMessage) ([]optionView, error) {
	trimmed := bytes.TrimSpace(result)
	var shape struct {
		Options json.RawMessage `json:"options"`
	}
	if len(trimmed) == 0 || json.Unmarshal(trimmed, &shape) != nil ||
		len(shape.Options) == 0 || bytes.Equal(bytes.TrimSpace(shape.Options), []byte("null")) {
		return nil, herrors.New(herrors.HandlerOutputInvalid,
			"the option provider returned an invalid options result", nil)
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(shape.Options, &items); err != nil {
		return nil, herrors.New(herrors.HandlerOutputInvalid,
			"the option provider returned an invalid options result", nil)
	}
	options := make([]optionView, 0, len(items))
	for _, item := range items {
		rawValue, ok := item["value"]
		if !ok || len(bytes.TrimSpace(rawValue)) == 0 {
			return nil, herrors.New(herrors.HandlerOutputInvalid,
				"the option provider returned an option without a value", nil)
		}
		var value any
		dec := json.NewDecoder(bytes.NewReader(rawValue))
		dec.UseNumber()
		if err := dec.Decode(&value); err != nil {
			return nil, herrors.New(herrors.HandlerOutputInvalid,
				"the option provider returned an invalid option value", nil)
		}
		option := optionView{Value: value}
		if rawLabel, ok := item["label"]; ok {
			if err := json.Unmarshal(rawLabel, &option.Label); err != nil {
				return nil, herrors.New(herrors.HandlerOutputInvalid,
					"the option provider returned a non-string label", nil)
			}
		}
		if rawDescription, ok := item["description"]; ok {
			if err := json.Unmarshal(rawDescription, &option.Description); err != nil {
				return nil, herrors.New(herrors.HandlerOutputInvalid,
					"the option provider returned a non-string description", nil)
			}
		}
		options = append(options, option)
	}
	return options, nil
}

// optionsFailure turns a handler's error response into the options
// endpoint's HandlerFailed error, with the handler's code inside.
func optionsFailure(info *runtime.ErrInfo) error {
	if info == nil {
		return herrors.New(herrors.HandlerFailed,
			"the option provider reported an error without an error object", nil)
	}
	return herrors.New(herrors.HandlerFailed,
		fmt.Sprintf("the option provider failed: %s", info.Message),
		map[string]any{
			"handler_code":    info.Code,
			"handler_message": info.Message,
			"handler_details": info.Details,
		})
}

// envelopeNode renders one registry node as the request envelope's
// executor object.
func envelopeNode(view registry.NodeView) runtime.Node {
	attributes := make(map[string]any, len(view.Attributes))
	for name, value := range view.Attributes {
		attributes[name] = value
	}
	return runtime.Node{
		ID:         view.ID,
		Name:       view.Name,
		OS:         view.OS,
		Arch:       view.Arch,
		Address:    view.Address,
		Attributes: attributes,
	}
}

// newOptionCallID returns a fresh option provider call ID: the `opt_`
// prefix and 96 random bits in hex.
func newOptionCallID() string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(fmt.Sprintf("api: cannot generate an option call ID: %v", err))
	}
	return "opt_" + hex.EncodeToString(raw[:])
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

// isNodeArgument reports whether an argument uses the `hanse:node` format.
func isNodeArgument(prop *jsonschema.Schema) bool {
	return prop != nil && prop.Format != nil && prop.Format.Name == nodeFormat
}

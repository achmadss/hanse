package httprt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"

	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/runtime"
)

// wireRequest is the JSON request envelope of handler contract section 2.
// Every field the contract requires is present, with `{}` for empty
// objects, so a handler can rely on their types.
type wireRequest struct {
	Contract   string              `json:"contract"`
	Kind       string              `json:"kind"`
	ID         string              `json:"id"`
	Capability string              `json:"capability"`
	Operation  string              `json:"operation"`
	Field      string              `json:"field,omitempty"`
	Arguments  json.RawMessage     `json:"arguments"`
	Nodes      map[string]wireNode `json:"nodes"`
	Executor   wireNode            `json:"executor"`
	Secrets    map[string]string   `json:"secrets"`
	Deadline   string              `json:"deadline"`
}

// wireNode is one node object of handler contract section 2.1. Attributes
// are always an object, never nil.
type wireNode struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	OS         string         `json:"os"`
	Arch       string         `json:"arch"`
	Address    string         `json:"address"`
	Attributes map[string]any `json:"attributes"`
}

// wireResponse is the JSON response envelope of handler contract section
// 3. Pointers and raw messages keep the presence of optional fields.
type wireResponse struct {
	Contract string           `json:"contract"`
	Status   string           `json:"status"`
	Result   *json.RawMessage `json:"result"`
	Summary  string           `json:"summary"`
	Error    *json.RawMessage `json:"error"`
}

// wireError is the error object of a failed response.
type wireError struct {
	Code    *string        `json:"code"`
	Message *string        `json:"message"`
	Details map[string]any `json:"details"`
}

// encodeRequest builds the JSON body of one call. It refuses an envelope
// that the Gateway itself could not have built, because sending it would
// only produce a confusing handler error.
func encodeRequest(req *runtime.Request) ([]byte, error) {
	if req == nil {
		return nil, herrors.New(herrors.Internal,
			"the http runtime received no request envelope", nil)
	}
	if req.Contract != "" && req.Contract != runtime.ContractV1 {
		return nil, herrors.New(herrors.UnsupportedContract,
			fmt.Sprintf("handler contract %q, the http runtime speaks %s",
				req.Contract, runtime.ContractV1), nil)
	}
	switch req.Kind {
	case runtime.KindOperation:
	case runtime.KindOptions:
		if req.Field == "" {
			return nil, herrors.New(herrors.Internal,
				"an options request envelope needs the argument field", nil)
		}
	default:
		return nil, herrors.New(herrors.Internal,
			fmt.Sprintf("envelope kind %q is neither %q nor %q",
				req.Kind, runtime.KindOperation, runtime.KindOptions), nil)
	}
	if req.ID == "" {
		return nil, herrors.New(herrors.Internal,
			"the request envelope needs a call ID", nil)
	}
	if req.Capability == "" || req.Operation == "" {
		return nil, herrors.New(herrors.Internal,
			"the request envelope needs a capability and an operation", nil)
	}
	arguments, err := argumentsObject(req.Arguments)
	if err != nil {
		return nil, err
	}
	out := wireRequest{
		Contract:   runtime.ContractV1,
		Kind:       req.Kind,
		ID:         req.ID,
		Capability: req.Capability,
		Operation:  req.Operation,
		Field:      req.Field,
		Arguments:  arguments,
		Nodes:      make(map[string]wireNode, len(req.Nodes)),
		Executor:   wireNodeOf(req.Executor),
		Secrets:    make(map[string]string, len(req.Secrets)),
		Deadline:   req.Deadline.UTC().Format(time.RFC3339Nano),
	}
	for id, node := range req.Nodes {
		out.Nodes[id] = wireNodeOf(node)
	}
	for name, value := range req.Secrets {
		out.Secrets[name] = value
	}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, herrors.New(herrors.Internal,
			fmt.Sprintf("the request envelope cannot be encoded: %s", err), nil)
	}
	return body, nil
}

// wireNodeOf renders one runtime node as the contract's node object. The
// maps of the request are never nil, so the JSON objects are always there.
func wireNodeOf(node runtime.Node) wireNode {
	attributes := make(map[string]any, len(node.Attributes))
	for name, value := range node.Attributes {
		attributes[name] = value
	}
	return wireNode{
		ID:         node.ID,
		Name:       node.Name,
		OS:         node.OS,
		Arch:       node.Arch,
		Address:    node.Address,
		Attributes: attributes,
	}
}

// argumentsObject returns the envelope's arguments, defaulting to `{}`.
// The contract requires a JSON object.
func argumentsObject(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return json.RawMessage(`{}`), nil
	}
	if !json.Valid(trimmed) || trimmed[0] != '{' {
		return nil, herrors.New(herrors.Internal,
			"the request envelope arguments are not a JSON object", nil)
	}
	return trimmed, nil
}

// decodeResponse parses one response envelope. Any malformed value is the
// handler_output_invalid code, handler contract section 3.
func decodeResponse(body []byte) (runtime.Response, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return runtime.Response{}, invalidOutput("the http handler returned an empty body")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	var wire wireResponse
	if err := dec.Decode(&wire); err != nil {
		return runtime.Response{}, invalidOutput(
			fmt.Sprintf("the http handler body is not a valid response envelope: %s", err))
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return runtime.Response{}, invalidOutput(
			"the http handler body holds more than one JSON value")
	}
	return buildResponse(&wire)
}

// buildResponse checks one decoded response envelope against handler
// contract section 3.
func buildResponse(wire *wireResponse) (runtime.Response, error) {
	if wire.Contract != "" && wire.Contract != runtime.ContractV1 {
		return runtime.Response{}, invalidOutput(
			fmt.Sprintf("the response contract %q is not %s", wire.Contract, runtime.ContractV1))
	}
	resp := runtime.Response{Summary: wire.Summary}
	switch wire.Status {
	case runtime.StatusOK:
		if wire.Error != nil {
			return runtime.Response{}, invalidOutput(
				"an ok response carries an error object")
		}
		result, err := resultObject(wire.Result)
		if err != nil {
			return runtime.Response{}, err
		}
		resp.Status = runtime.StatusOK
		resp.Result = result
	case runtime.StatusCancelled:
		if wire.Error != nil {
			return runtime.Response{}, invalidOutput(
				"a cancelled response carries an error object")
		}
		result, err := resultObject(wire.Result)
		if err != nil {
			return runtime.Response{}, err
		}
		resp.Status = runtime.StatusCancelled
		resp.Result = result
	case runtime.StatusError:
		info, err := decodeError(wire.Error)
		if err != nil {
			return runtime.Response{}, err
		}
		resp.Status = runtime.StatusError
		resp.Err = info
	case "":
		return runtime.Response{}, invalidOutput("the response has no status")
	default:
		return runtime.Response{}, invalidOutput(
			fmt.Sprintf("the response status %q is not ok, error, or cancelled", wire.Status))
	}
	return resp, nil
}

// resultObject checks the result field: absent and null are both allowed,
// and anything else must be a JSON object.
func resultObject(raw *json.RawMessage) (json.RawMessage, error) {
	if raw == nil {
		return nil, nil
	}
	trimmed := bytes.TrimSpace(*raw)
	if bytes.Equal(trimmed, []byte("null")) {
		return json.RawMessage("null"), nil
	}
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return nil, invalidOutput("the response result is not a JSON object")
	}
	return json.RawMessage(trimmed), nil
}

// decodeError checks the error object of a failed response.
func decodeError(raw *json.RawMessage) (*runtime.ErrInfo, error) {
	if raw == nil {
		return nil, invalidOutput("a status error response needs an error object")
	}
	var wire wireError
	if err := json.Unmarshal(*raw, &wire); err != nil {
		return nil, invalidOutput("the response error object is not valid")
	}
	if wire.Code == nil || wire.Message == nil {
		return nil, invalidOutput("the response error object needs a code and a message")
	}
	if !validHandlerCode(*wire.Code) {
		return nil, invalidOutput(
			fmt.Sprintf("the response error code %q is not a snake_case handler code", *wire.Code))
	}
	return &runtime.ErrInfo{
		Code:    *wire.Code,
		Message: *wire.Message,
		Details: wire.Details,
	}, nil
}

// optionsProblem names the first mismatch between a result and the option
// provider schema of handler contract section 14.3, or "" when it matches.
func optionsProblem(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "an options response needs a result"
	}
	var shape struct {
		Options json.RawMessage `json:"options"`
	}
	if err := json.Unmarshal(trimmed, &shape); err != nil {
		return "the result is not an options object"
	}
	if len(shape.Options) == 0 || bytes.Equal(bytes.TrimSpace(shape.Options), []byte("null")) {
		return "the result has no options array"
	}
	var options []map[string]json.RawMessage
	if err := json.Unmarshal(shape.Options, &options); err != nil {
		return "options is not an array of objects"
	}
	for i, option := range options {
		value, ok := option["value"]
		if !ok || len(bytes.TrimSpace(value)) == 0 {
			return fmt.Sprintf("option %d has no value", i)
		}
		for _, name := range []string{"label", "description"} {
			text, ok := option[name]
			if !ok {
				continue
			}
			var decoded string
			if err := json.Unmarshal(text, &decoded); err != nil {
				return fmt.Sprintf("option %d has a non-string %s", i, name)
			}
		}
	}
	return ""
}

// validHandlerCode reports whether code matches the error code pattern of
// handler contract section 14.2: ^[a-z][a-z0-9_]*$, at most 64 characters.
func validHandlerCode(code string) bool {
	if code == "" || len(code) > 64 {
		return false
	}
	for i := 0; i < len(code); i++ {
		c := code[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9' || c == '_':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// invalidOutput is the handler_output_invalid error of one malformed
// response. It never carries the body, which can hold more than is safe to
// repeat.
func invalidOutput(message string) *herrors.Error {
	return herrors.New(herrors.HandlerOutputInvalid, message, nil)
}

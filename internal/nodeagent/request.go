package nodeagent

import (
	"encoding/json"
	"fmt"
	"time"

	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/runtime"
)

// wireRequest is the handler request envelope of docs/handler-contract.md
// section 2, as the Gateway sends it inside a run message.
type wireRequest struct {
	Contract   string              `json:"contract"`
	Kind       string              `json:"kind"`
	ID         string              `json:"id"`
	Capability string              `json:"capability"`
	Operation  string              `json:"operation"`
	Field      string              `json:"field"`
	Arguments  json.RawMessage     `json:"arguments"`
	Nodes      map[string]wireNode `json:"nodes"`
	Executor   wireNode            `json:"executor"`
	Secrets    map[string]string   `json:"secrets"`
	Deadline   string              `json:"deadline"`
}

// wireNode is one node object of the request envelope.
type wireNode struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	OS         string         `json:"os"`
	Arch       string         `json:"arch"`
	Address    string         `json:"address"`
	Attributes map[string]any `json:"attributes"`
}

// decodeRequest reads the run's request envelope into the runtime value the
// runner needs. The raw bytes are written to the handler unchanged; this
// decode only supplies the deadline and kind the runner enforces.
func decodeRequest(raw json.RawMessage) (*runtime.Request, error) {
	var wire wireRequest
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, herrors.New(herrors.Internal,
			fmt.Sprintf("the run request is not one JSON object: %s", err), nil)
	}
	if wire.Kind != "" && wire.Kind != runtime.KindOperation && wire.Kind != runtime.KindOptions {
		return nil, herrors.New(herrors.Internal,
			fmt.Sprintf("the run request kind %q is not an operation or an options call", wire.Kind), nil)
	}
	req := &runtime.Request{
		Contract:   wire.Contract,
		Kind:       wire.Kind,
		ID:         wire.ID,
		Capability: wire.Capability,
		Operation:  wire.Operation,
		Field:      wire.Field,
		Arguments:  wire.Arguments,
		Secrets:    wire.Secrets,
		Executor:   nodeOf(wire.Executor),
		Nodes:      make(map[string]runtime.Node, len(wire.Nodes)),
	}
	for id, node := range wire.Nodes {
		req.Nodes[id] = nodeOf(node)
	}
	if wire.Deadline != "" {
		deadline, err := time.Parse(time.RFC3339Nano, wire.Deadline)
		if err != nil {
			return nil, herrors.New(herrors.Internal,
				fmt.Sprintf("the run request deadline %q is not RFC 3339", wire.Deadline), nil)
		}
		req.Deadline = deadline.UTC()
	}
	return req, nil
}

// nodeOf converts one wire node.
func nodeOf(wire wireNode) runtime.Node {
	return runtime.Node{
		ID:         wire.ID,
		Name:       wire.Name,
		OS:         wire.OS,
		Arch:       wire.Arch,
		Address:    wire.Address,
		Attributes: wire.Attributes,
	}
}

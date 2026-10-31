package operations

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/achmadss/hanse/internal/agentconn"
	"github.com/achmadss/hanse/internal/config"
	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/runtime"
)

// AgentDispatcher runs a call on its executor's node agent
// (docs/agent-protocol.md section 4.3). It builds the run message from the
// request envelope and the capability's routing, and hands it to the
// connection hub. A call whose executor has no agent connection fails with
// ExecutorUnavailable: the Gateway never falls back to another node.
type AgentDispatcher struct {
	hub      *agentconn.Hub
	registry *registry.Registry
	timeouts config.Timeouts
}

// NewAgentDispatcher returns a dispatcher over the agent connection hub.
// The registry supplies each capability's bundle hash, entry command, and
// per-operation timeouts.
func NewAgentDispatcher(hub *agentconn.Hub, reg *registry.Registry, timeouts config.Timeouts) *AgentDispatcher {
	return &AgentDispatcher{hub: hub, registry: reg, timeouts: timeouts}
}

// Execute sends one run and waits for its result.
func (d *AgentDispatcher) Execute(ctx context.Context, call *Call) (runtime.Response, error) {
	if call == nil || call.Request == nil {
		return runtime.Response{}, herrors.New(herrors.Internal,
			"the agent dispatcher received no request envelope", nil)
	}
	if d == nil || d.hub == nil {
		return runtime.Response{}, herrors.New(herrors.Internal,
			"the agent connection hub is not available", nil)
	}
	if d.registry == nil || d.registry.Nodes == nil {
		return runtime.Response{}, herrors.New(herrors.Internal,
			"the capability registry is not available", nil)
	}
	req := call.Request
	nodeID := req.Executor.ID
	if nodeID == "" {
		return runtime.Response{}, herrors.New(herrors.Internal,
			"the request envelope has no executor node", nil)
	}
	view, ok := d.registry.Nodes.Get(nodeID)
	if !ok {
		return runtime.Response{}, herrors.New(herrors.InvalidExecutor,
			fmt.Sprintf("executor %q is not a node in nodes.yaml", nodeID),
			map[string]any{"executor": nodeID})
	}
	capability, ok := d.registry.Capability(req.Capability)
	if !ok {
		return runtime.Response{}, herrors.New(herrors.Internal,
			fmt.Sprintf("capability %q is not loaded", req.Capability),
			map[string]any{"capability": req.Capability})
	}
	operation, ok := capability.Operation(req.Operation)
	if !ok {
		return runtime.Response{}, herrors.New(herrors.Internal,
			fmt.Sprintf("capability %q has no operation %q", req.Capability, req.Operation),
			map[string]any{"capability": req.Capability, "operation": req.Operation})
	}
	if !operationIsExec(capability) {
		return runtime.Response{}, herrors.New(herrors.Internal,
			fmt.Sprintf("capability %q has no exec handler to send to a node agent", req.Capability),
			map[string]any{"capability": req.Capability})
	}
	entry := operation.Entry[view.OS]
	if len(entry) == 0 {
		return runtime.Response{}, herrors.New(herrors.ExecutorNotEligible,
			fmt.Sprintf("operation %q of capability %q has no entry command for %s",
				req.Operation, req.Capability, view.OS),
			map[string]any{"capability": req.Capability, "operation": req.Operation, "executor": nodeID})
	}
	envelope, err := marshalRequestEnvelope(req)
	if err != nil {
		return runtime.Response{}, herrors.New(herrors.Internal,
			"the request envelope could not be encoded: "+err.Error(), nil)
	}
	effective := config.ResolveTimeouts(d.timeouts, &config.OperationTimeouts{
		Timeout:     operation.Timeout,
		GracePeriod: operation.GracePeriod,
	})
	return d.hub.Execute(ctx, &agentconn.Call{
		RunID:      req.ID,
		Node:       nodeID,
		Capability: req.Capability,
		Operation:  req.Operation,
		Option:     req.Kind == runtime.KindOptions,
		Bundle:     capability.BundleHash,
		Entry:      entry,
		Request:    envelope,
		// An option provider never runs with administrator rights, even
		// for a privileged operation (docs/handler-contract.md section
		// 4.5).
		Privileged:  operation.Privileged && req.Kind != runtime.KindOptions,
		Password:    passwordOf(call.Elevation),
		GracePeriod: effective.GracePeriod,
	})
}

// operationIsExec reports whether the capability runs its handlers as
// programs on an executor node. An http capability runs on the Gateway
// host, and a builtin capability runs in the Gateway process; neither goes
// to a node agent.
func operationIsExec(capability *registry.Capability) bool {
	return capability != nil && capability.Manifest != nil && capability.Manifest.Handler.Kind == registry.KindExec
}

// agentRequestEnvelope is the request envelope's JSON shape,
// docs/handler-contract.md section 2. The runtime's Request type carries no
// JSON tags because builtin handlers receive it as a Go value, so the
// agent dispatcher renders the documented field names here.
type agentRequestEnvelope struct {
	Contract   string                  `json:"contract"`
	Kind       string                  `json:"kind"`
	ID         string                  `json:"id"`
	Capability string                  `json:"capability"`
	Operation  string                  `json:"operation"`
	Field      string                  `json:"field,omitempty"`
	Arguments  json.RawMessage         `json:"arguments"`
	Nodes      map[string]agentEnvNode `json:"nodes"`
	Executor   agentEnvNode            `json:"executor"`
	Secrets    map[string]string       `json:"secrets"`
	Deadline   string                  `json:"deadline"`
}

// agentEnvNode is one node object of the request envelope,
// docs/handler-contract.md section 2.1.
type agentEnvNode struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	OS         string         `json:"os"`
	Arch       string         `json:"arch"`
	Address    string         `json:"address"`
	Attributes map[string]any `json:"attributes"`
}

// marshalRequestEnvelope renders the handler contract's request envelope.
func marshalRequestEnvelope(req *runtime.Request) (json.RawMessage, error) {
	envelope := agentRequestEnvelope{
		Contract:   req.Contract,
		Kind:       req.Kind,
		ID:         req.ID,
		Capability: req.Capability,
		Operation:  req.Operation,
		Field:      req.Field,
		Arguments:  req.Arguments,
		Nodes:      make(map[string]agentEnvNode, len(req.Nodes)),
		Executor:   agentEnvNodeOf(req.Executor),
		Secrets:    req.Secrets,
		Deadline:   agentDeadline(req.Deadline),
	}
	if envelope.Nodes == nil {
		envelope.Nodes = map[string]agentEnvNode{}
	}
	for id, node := range req.Nodes {
		envelope.Nodes[id] = agentEnvNodeOf(node)
	}
	if envelope.Secrets == nil {
		envelope.Secrets = map[string]string{}
	}
	if envelope.Arguments == nil {
		envelope.Arguments = json.RawMessage("{}")
	}
	return json.Marshal(envelope)
}

// agentEnvNodeOf renders one runtime node as the envelope's node object.
func agentEnvNodeOf(node runtime.Node) agentEnvNode {
	attributes := make(map[string]any, len(node.Attributes))
	for name, value := range node.Attributes {
		attributes[name] = value
	}
	return agentEnvNode{
		ID:         node.ID,
		Name:       node.Name,
		OS:         node.OS,
		Arch:       node.Arch,
		Address:    node.Address,
		Attributes: attributes,
	}
}

// agentDeadline renders the deadline in the protocol's timestamp format.
// A zero deadline stays absent so the agent does not enforce one.
func agentDeadline(deadline time.Time) string {
	if deadline.IsZero() {
		return ""
	}
	return deadline.UTC().Format("2006-01-02T15:04:05.000Z")
}

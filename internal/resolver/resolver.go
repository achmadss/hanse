package resolver

import (
	"fmt"

	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/registry"
)

// Source records how an executor was chosen. docs/architecture.md section 5
// fixes the two wire values.
type Source string

const (
	// SourceExplicit means the request named the executor.
	SourceExplicit Source = "explicit"
	// SourceDefault means a default_executor from routes.yaml chose it.
	SourceDefault Source = "default"
)

// Resolution is the accepted executor of one request.
type Resolution struct {
	// Executor is the node ID the request runs on.
	Executor string
	// Source is how the executor was chosen.
	Source Source
}

// CapabilityLookup finds a loaded capability by ID.
type CapabilityLookup interface {
	// Capability returns one capability by ID. It reports false when no
	// such capability is loaded.
	Capability(id string) (*registry.Capability, bool)
}

// NodeLookup finds a node by ID with its current status.
type NodeLookup interface {
	// Get returns one node by ID. It reports false for an unknown node.
	Get(id string) (registry.NodeView, bool)
}

// Resolver chooses executors against the loaded capabilities and nodes. It
// holds no state of its own, so one value serves every request.
type Resolver struct {
	capabilities CapabilityLookup
	nodes        NodeLookup
}

// New builds a Resolver. The Gateway passes its registry and the same
// registry's node registry: New(reg, reg.Nodes).
func New(capabilities CapabilityLookup, nodes NodeLookup) *Resolver {
	return &Resolver{capabilities: capabilities, nodes: nodes}
}

// Resolve returns the executor of one request. requested is the executor
// the request named, or "" when it named none. The chosen executor is in
// the operation's eligible list and is enabled and, for an exec capability,
// online; this never substitutes another node.
//
// It fails with:
//   - UnknownCapability when the capability is not loaded,
//   - UnknownOperation when the capability has no such operation,
//   - InvalidExecutor when a named or default executor is not a known node,
//   - ExecutorNotEligible when the chosen executor may not run this
//     operation,
//   - NoDefaultExecutor when the request named none and routes.yaml sets
//     none for the operation or its capability,
//   - ExecutorUnavailable when the chosen executor of an exec capability is
//     disabled or offline. An http capability runs in the Gateway process,
//     so its executor needs no connection.
func (r *Resolver) Resolve(capabilityID, operationID, requested string) (Resolution, error) {
	loaded, ok := r.capability(capabilityID)
	if !ok {
		return Resolution{}, herrors.New(herrors.UnknownCapability,
			fmt.Sprintf("capability %q is not loaded", capabilityID),
			map[string]any{"capability": capabilityID})
	}
	if _, ok := loaded.Operation(operationID); !ok {
		return Resolution{}, herrors.New(herrors.UnknownOperation,
			fmt.Sprintf("capability %q has no operation %q", capabilityID, operationID),
			map[string]any{"capability": capabilityID, "operation": operationID})
	}
	// An http capability runs in the Gateway process, which calls the
	// handler endpoint itself; no node agent is involved, so the executor's
	// connection status does not matter. exec capabilities still need their
	// executor online (docs/architecture.md §5).
	needsOnline := loaded.Manifest == nil || loaded.Manifest.Handler.Kind != registry.KindHTTP
	// A capability that routes.yaml does not mention has no routing entry
	// for its operations; its requests keep failing with no eligible
	// executor and no default.
	routing, _ := loaded.RoutingOf(operationID)

	if requested != "" {
		return r.accept(capabilityID, operationID, routing, requested, SourceExplicit, needsOnline)
	}
	if routing.DefaultExecutor == "" {
		return Resolution{}, herrors.New(herrors.NoDefaultExecutor,
			fmt.Sprintf("operation %q of capability %q has no default executor: name an executor in the request",
				operationID, capabilityID),
			map[string]any{"capability": capabilityID, "operation": operationID})
	}
	return r.accept(capabilityID, operationID, routing, routing.DefaultExecutor, SourceDefault, needsOnline)
}

// accept checks one candidate executor and returns it when it may run the
// operation. A rejected candidate is never replaced by another node.
func (r *Resolver) accept(capabilityID, operationID string, routing registry.OperationRouting, executor string, source Source, needsOnline bool) (Resolution, error) {
	view, known := r.node(executor)
	if !known {
		return Resolution{}, herrors.New(herrors.InvalidExecutor,
			fmt.Sprintf("executor %q is not a node in nodes.yaml", executor),
			map[string]any{"capability": capabilityID, "operation": operationID, "executor": executor})
	}
	if !containsExecutor(routing.Executors, executor) {
		return Resolution{}, herrors.New(herrors.ExecutorNotEligible,
			fmt.Sprintf("executor %q is not eligible for operation %q of capability %q",
				executor, operationID, capabilityID),
			map[string]any{"capability": capabilityID, "operation": operationID, "executor": executor})
	}
	if !view.Enabled {
		return Resolution{}, herrors.New(herrors.ExecutorUnavailable,
			fmt.Sprintf("executor %q is disabled", executor),
			map[string]any{"capability": capabilityID, "operation": operationID, "executor": executor})
	}
	if needsOnline && view.Status != registry.StatusOnline {
		return Resolution{}, herrors.New(herrors.ExecutorUnavailable,
			fmt.Sprintf("executor %q is %s", executor, view.Status),
			map[string]any{"capability": capabilityID, "operation": operationID, "executor": executor})
	}
	return Resolution{Executor: executor, Source: source}, nil
}

// capability returns one loaded capability, tolerating a nil resolver or a
// nil lookup so that a partially built Gateway fails with UnknownCapability
// instead of a panic.
func (r *Resolver) capability(id string) (*registry.Capability, bool) {
	if r == nil || r.capabilities == nil {
		return nil, false
	}
	return r.capabilities.Capability(id)
}

// node returns one node with its status, tolerating a nil lookup.
func (r *Resolver) node(id string) (registry.NodeView, bool) {
	if r == nil || r.nodes == nil {
		return registry.NodeView{}, false
	}
	return r.nodes.Get(id)
}

// containsExecutor reports whether the eligible list contains id.
func containsExecutor(ids []string, id string) bool {
	for _, one := range ids {
		if one == id {
			return true
		}
	}
	return false
}

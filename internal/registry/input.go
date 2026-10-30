package registry

import herrors "github.com/achmadss/hanse/internal/errors"

// Input is what the registry needs from the other configuration files. It
// mirrors the parts of nodes.yaml and routes.yaml this package uses, with
// their own validation rules applied here. Once issue #8 (loading and
// validating nodes.yaml and routes.yaml) merges, these types should be
// replaced by, or wrap, the corresponding types from internal/config.
type Input struct {
	// Nodes is every node from nodes.yaml.
	Nodes []Node
	// GatewayHost is the Gateway host node ID from gateway.yaml's `node`.
	// http capabilities may list only this node as executor.
	GatewayHost string
	// Routes is the parsed routes.yaml. An empty Routes behaves like an
	// empty routing table: every capability is unrouted and warns.
	Routes Routes
}

// Node is the non-secret part of one node from nodes.yaml.
type Node struct {
	ID string
	// OS is "linux", "darwin", or "windows".
	OS string
	// Arch is "amd64" or "arm64".
	Arch       string
	Roles      []string
	Attributes map[string]string
	Enabled    bool
}

// Routes is a parsed routes.yaml. An OperationRoute is present in
// Operations when the operation has an entry at its level; otherwise the
// capability's route applies to it.
type Routes struct {
	Capabilities map[string]CapabilityRoute
}

// CapabilityRoute is one capability's routing from routes.yaml.
type CapabilityRoute struct {
	// Wildcard is `"*"`: every enabled node whose OS has an entry command.
	Wildcard bool
	// Executors are the node IDs listed explicitly. Used when
	// Wildcard is false; an empty slice means no eligible executors.
	Executors []string
	// DefaultExecutor is the executor when a request names none.
	DefaultExecutor string
	// Operations maps operation ID to the operation's own routing.
	Operations map[string]OperationRoute
}

// OperationRoute is one operation's routing from routes.yaml.
type OperationRoute struct {
	Wildcard bool
	// Executors are the node IDs listed explicitly.
	Executors []string
	// ExecutorsSet distinguishes "executors: <list>" from an absent field,
	// because an absent field inherits the capability's list.
	ExecutorsSet bool
	// DefaultExecutor is the executor when a request names none.
	DefaultExecutor string
	// DefaultSet distinguishes "default_executor: <node>" from an absent
	// field: an absent field inherits the capability's default.
	DefaultSet bool
}

// validate checks the invariants the registry relies on: node IDs are
// valid and unique, the Gateway host is a known node, capability route IDs
// are valid, and route node IDs are valid. It does not validate
// nodes.yaml or routes.yaml semantics; that stays with issue #8.
func (in Input) validate() error {
	seen := map[string]bool{}
	for i := range in.Nodes {
		n := &in.Nodes[i]
		if !validID(n.ID) {
			return herrors.New(herrors.Internal, "invalid node ID "+quote(n.ID), nil)
		}
		if seen[n.ID] {
			return herrors.New(herrors.Internal, "duplicate node ID "+quote(n.ID), nil)
		}
		seen[n.ID] = true
	}
	if in.GatewayHost != "" {
		if !validID(in.GatewayHost) {
			return herrors.New(herrors.Internal, "invalid Gateway host node ID "+quote(in.GatewayHost), nil)
		}
		if _, ok := in.node(in.GatewayHost); !ok {
			return herrors.New(herrors.Internal,
				"Gateway host node "+quote(in.GatewayHost)+" is not in nodes.yaml", nil)
		}
	}
	for capID, route := range in.Routes.Capabilities {
		if !validID(capID) {
			return herrors.New(herrors.Internal, "invalid capability ID "+quote(capID)+" in routes", nil)
		}
		if !route.Wildcard {
			for _, id := range route.Executors {
				if !validID(id) {
					return herrors.New(herrors.Internal,
						"invalid node ID "+quote(id)+" in routing for capability "+quote(capID), nil)
				}
			}
		}
		if route.DefaultExecutor != "" && !validID(route.DefaultExecutor) {
			return herrors.New(herrors.Internal,
				"invalid default_executor "+quote(route.DefaultExecutor)+" for capability "+quote(capID), nil)
		}
		for opID, op := range route.Operations {
			if !validID(opID) {
				return herrors.New(herrors.Internal,
					"invalid operation ID "+quote(opID)+" in routing for capability "+quote(capID), nil)
			}
			if !op.Wildcard {
				for _, id := range op.Executors {
					if !validID(id) {
						return herrors.New(herrors.Internal,
							"invalid node ID "+quote(id)+" in routing for "+quote(capID+"."+opID), nil)
					}
				}
			}
			if op.DefaultExecutor != "" && !validID(op.DefaultExecutor) {
				return herrors.New(herrors.Internal,
					"invalid default_executor "+quote(op.DefaultExecutor)+" for "+quote(capID+"."+opID), nil)
			}
		}
	}
	return nil
}

// node returns the node with the given ID.
func (in Input) node(id string) (*Node, bool) {
	for i := range in.Nodes {
		if in.Nodes[i].ID == id {
			return &in.Nodes[i], true
		}
	}
	return nil, false
}

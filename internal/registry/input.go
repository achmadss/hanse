package registry

import (
	"errors"
	"strconv"

	"github.com/achmadss/hanse/internal/config"
	herrors "github.com/achmadss/hanse/internal/errors"
)

// Input is what the registry needs from the other configuration files.
// It keeps the registry's own shape for the node and route values so the
// wiring issue can connect it to internal/config; ID checks already go
// through config.ValidID.
type Input struct {
	// Nodes is every node from nodes.yaml.
	Nodes []Node
	// GatewayHost is the Gateway host node ID from gateway.yaml's `node`.
	// http capabilities may list only this node as executor.
	GatewayHost string
	// GatewayHostNoEmbeddedAgent says that the Gateway runs no embedded
	// agent for GatewayHost, so that node is online only while its
	// hanse-node agent is connected. False keeps the embedded-agent rule:
	// the Gateway host is always online (docs/architecture.md §8).
	GatewayHostNoEmbeddedAgent bool
	// Routes is the parsed routes.yaml. An empty Routes behaves like an
	// empty routing table: every capability is unrouted and warns.
	Routes Routes
}

// Node is the non-secret part of one node from nodes.yaml.
type Node struct {
	ID string
	// Name is the human-readable name from nodes.yaml; it defaults to the
	// node ID, as config.finalizeNodes fills it.
	Name string
	// OS is "linux", "darwin", or "windows".
	OS string
	// Arch is "amd64" or "arm64".
	Arch string
	// Address is the host name or IP address other nodes use to reach
	// this node; empty when nodes.yaml sets none.
	Address    string
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
// nodes.yaml or routes.yaml semantics; that stays with issue #8. It
// collects every problem it finds.
func (in Input) validate() error {
	probs := in.validateNodes()
	for capID, route := range in.Routes.Capabilities {
		capField := "routes.yaml: capabilities." + capID
		if !config.ValidID(capID) {
			probs = append(probs, herrors.New(herrors.Internal,
				"routes.yaml: capabilities."+quote(capID)+": invalid capability ID", nil))
		}
		if !route.Wildcard {
			for i, id := range route.Executors {
				if !config.ValidID(id) {
					probs = append(probs, herrors.New(herrors.Internal,
						capField+".executors["+strconv.Itoa(i)+"]: invalid node ID "+quote(id), nil))
				}
			}
		}
		if route.DefaultExecutor != "" && !config.ValidID(route.DefaultExecutor) {
			probs = append(probs, herrors.New(herrors.Internal,
				capField+".default_executor: invalid node ID "+quote(route.DefaultExecutor), nil))
		}
		for opID, op := range route.Operations {
			opField := capField + ".operations." + opID
			if !config.ValidID(opID) {
				probs = append(probs, herrors.New(herrors.Internal,
					capField+".operations."+quote(opID)+": invalid operation ID", nil))
			}
			if !op.Wildcard {
				for i, id := range op.Executors {
					if !config.ValidID(id) {
						probs = append(probs, herrors.New(herrors.Internal,
							opField+".executors["+strconv.Itoa(i)+"]: invalid node ID "+quote(id), nil))
					}
				}
			}
			if op.DefaultExecutor != "" && !config.ValidID(op.DefaultExecutor) {
				probs = append(probs, herrors.New(herrors.Internal,
					opField+".default_executor: invalid node ID "+quote(op.DefaultExecutor), nil))
			}
		}
	}
	return errors.Join(probs...)
}

// validateNodes checks the node part of the input: every node ID is valid
// and unique, and the Gateway host is a known node with a valid ID. It is
// the shared check of validate and NewNodeRegistry.
func (in Input) validateNodes() []error {
	var probs []error
	seen := map[string]bool{}
	for i := range in.Nodes {
		n := &in.Nodes[i]
		if !config.ValidID(n.ID) {
			probs = append(probs, herrors.New(herrors.Internal,
				"nodes: invalid node ID "+quote(n.ID), nil))
		}
		if seen[n.ID] {
			probs = append(probs, herrors.New(herrors.Internal,
				"nodes: duplicate node ID "+quote(n.ID), nil))
		}
		seen[n.ID] = true
	}
	if in.GatewayHost != "" {
		if !config.ValidID(in.GatewayHost) {
			probs = append(probs, herrors.New(herrors.Internal,
				"gateway.yaml: node: invalid node ID "+quote(in.GatewayHost), nil))
		} else if _, ok := in.node(in.GatewayHost); !ok {
			probs = append(probs, herrors.New(herrors.Internal,
				"gateway.yaml: node: Gateway host node "+quote(in.GatewayHost)+" is not in nodes.yaml", nil))
		}
	}
	return probs
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

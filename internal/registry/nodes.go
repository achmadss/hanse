package registry

import (
	"errors"
	"sort"
	"sync"

	herrors "github.com/achmadss/hanse/internal/errors"
)

// NodeStatus is a node's connection state. A node is online while its
// agent is connected to the Gateway and offline otherwise
// (docs/architecture.md section 8.2); nodes.yaml can say nothing about it.
type NodeStatus string

const (
	// StatusOnline means the node's agent connection is up.
	StatusOnline NodeStatus = "online"
	// StatusOffline means the node has no connected agent.
	StatusOffline NodeStatus = "offline"
)

// valid reports whether status is one of the two documented statuses.
func (s NodeStatus) valid() bool { return s == StatusOnline || s == StatusOffline }

// NodeView is one node's configured values from nodes.yaml together with
// its current status. The Node values never change after construction;
// Get and List return a deep copy of the node, so callers cannot mutate
// registry state through the slice and map fields.
type NodeView struct {
	Node
	Status NodeStatus
	// AgentVersion is the release version the node's agent reported when
	// it connected, or "" when no agent has connected since the Gateway
	// started. It changes only with the connection.
	AgentVersion string
}

// NodeCapability is one capability a node can execute, with the
// operation IDs of that capability that routes.yaml makes eligible for it.
type NodeCapability struct {
	Capability string
	Operations []string
}

// nodeState is one node's immutable configuration and its current status.
type nodeState struct {
	node Node
	mu   sync.RWMutex
	// status is guarded by mu.
	status NodeStatus
	// agentVersion is guarded by mu. It is the agent version reported by
	// the current or most recent connection.
	agentVersion string
}

// NodeRegistry holds every node from nodes.yaml, its enabled flag, its
// current online/offline status, and the capabilities and operations its
// routing makes it eligible to execute. It is safe for concurrent use:
// lookups and status updates come from different goroutines, so
// agent connections and API handlers share one instance.
//
// Status is kept in memory only. A Gateway starts with every node
// offline, because no agent is connected yet at startup. The Gateway
// host's embedded agent is an exception: it never disconnects, so its
// node is online from construction on and cannot be set offline. A Gateway
// that sets embedded_agent: false has no such exception: its host node is
// online only while its node agent is connected.
type NodeRegistry struct {
	gatewayHost                string
	gatewayHostNoEmbeddedAgent bool
	// statesMu guards the states map, which a registry reload replaces.
	// Presence bits live in nodeState, which has its own lock.
	statesMu sync.RWMutex
	states   map[string]*nodeState
	// capsMu guards capabilities, which a registry reload replaces.
	capsMu sync.RWMutex
	// capabilities maps a node ID to its executable capabilities, sorted.
	capabilities map[string][]NodeCapability
}

// NewNodeRegistry builds the node registry from nodes.yaml and gateway.yaml
// values plus the loaded capabilities. The routing of every capability must
// already be joined with routes.yaml, as Load does first: the executable
// capabilities come from those executor lists. The node IDs are validated:
// they must be valid and unique, and the Gateway host must be a known node.
func NewNodeRegistry(in Input, capabilities []*Capability) (*NodeRegistry, error) {
	if err := errors.Join(in.validateNodes()...); err != nil {
		return nil, err
	}
	n := &NodeRegistry{
		gatewayHost:                in.GatewayHost,
		gatewayHostNoEmbeddedAgent: in.GatewayHostNoEmbeddedAgent,
		states:                     make(map[string]*nodeState, len(in.Nodes)),
		capabilities:               map[string][]NodeCapability{},
	}
	for _, node := range in.Nodes {
		n.states[node.ID] = &nodeState{node: node, status: StatusOffline}
	}
	// Every node can execute every operation that lists it.
	opsByNode := map[string]map[string]map[string]bool{}
	for _, c := range capabilities {
		for opID, routing := range c.Routing {
			for _, id := range routing.Executors {
				ops, ok := opsByNode[id]
				if !ok {
					ops = map[string]map[string]bool{}
					opsByNode[id] = ops
				}
				capOps, ok := ops[c.ID]
				if !ok {
					capOps = map[string]bool{}
					ops[c.ID] = capOps
				}
				capOps[opID] = true
			}
		}
	}
	for id, ops := range opsByNode {
		n.capabilities[id] = nodeCapabilitiesSorted(ops)
	}
	if n.gatewayHost != "" && !n.gatewayHostNoEmbeddedAgent {
		// The Gateway host's embedded agent is always online
		// (docs/architecture.md section 8).
		host := n.states[n.gatewayHost]
		host.mu.Lock()
		host.status = StatusOnline
		host.mu.Unlock()
	}
	return n, nil
}

// nodeCapabilitiesSorted renders one node's capability map sorted.
func nodeCapabilitiesSorted(ops map[string]map[string]bool) []NodeCapability {
	capIDs := make([]string, 0, len(ops))
	for id := range ops {
		capIDs = append(capIDs, id)
	}
	sort.Strings(capIDs)
	out := make([]NodeCapability, 0, len(capIDs))
	for _, capID := range capIDs {
		opIDs := make([]string, 0, len(ops[capID]))
		for opID := range ops[capID] {
			opIDs = append(opIDs, opID)
		}
		sort.Strings(opIDs)
		out = append(out, NodeCapability{Capability: capID, Operations: opIDs})
	}
	return out
}

// Get returns one node by ID with its current status. The Node's slice
// and map fields are copied, so a caller cannot mutate registry state.
func (n *NodeRegistry) Get(id string) (NodeView, bool) {
	if n == nil {
		return NodeView{}, false
	}
	state := n.state(id)
	if state == nil {
		return NodeView{}, false
	}
	state.mu.RLock()
	view := NodeView{Node: copyNode(state.node), Status: state.status, AgentVersion: state.agentVersion}
	state.mu.RUnlock()
	return view, true
}

// List returns every node with its current status, sorted by node ID.
// Every Node's slice and map fields are copied, so a caller cannot
// mutate registry state.
func (n *NodeRegistry) List() []NodeView {
	if n == nil {
		return nil
	}
	n.statesMu.RLock()
	ids := make([]string, 0, len(n.states))
	states := make(map[string]*nodeState, len(n.states))
	for id, state := range n.states {
		ids = append(ids, id)
		states[id] = state
	}
	n.statesMu.RUnlock()
	sort.Strings(ids)
	out := make([]NodeView, 0, len(ids))
	for _, id := range ids {
		state := states[id]
		state.mu.RLock()
		out = append(out, NodeView{Node: copyNode(state.node), Status: state.status, AgentVersion: state.agentVersion})
		state.mu.RUnlock()
	}
	return out
}

// state returns the state of one node, or nil for an unknown ID or a nil
// registry.
func (n *NodeRegistry) state(id string) *nodeState {
	if n == nil {
		return nil
	}
	n.statesMu.RLock()
	defer n.statesMu.RUnlock()
	return n.states[id]
}

// Status returns the current status of one node.
func (n *NodeRegistry) Status(id string) (NodeStatus, bool) {
	state := n.state(id)
	if state == nil {
		return "", false
	}
	state.mu.RLock()
	status := state.status
	state.mu.RUnlock()
	return status, true
}

// Enabled reports whether id is a known and enabled node
// (docs/configuration.md section 4.1). Unknown nodes are not enabled.
func (n *NodeRegistry) Enabled(id string) bool {
	state := n.state(id)
	return state != nil && state.node.Enabled
}

// GatewayHost returns the Gateway host node ID from gateway.yaml's `node`,
// or "" when none is configured. http capabilities route to it in every
// layout.
func (n *NodeRegistry) GatewayHost() string {
	if n == nil {
		return ""
	}
	return n.gatewayHost
}

// SetAgentPresence reports the agent connection state and the agent
// version of one node. Agent connections call it when they connect and
// when they are lost (docs/architecture.md section 8.2); it is the same
// check as SetStatus plus the version a hello reported.
func (n *NodeRegistry) SetAgentPresence(id string, status NodeStatus, agentVersion string) error {
	if err := n.checkStatus(id, status); err != nil {
		return err
	}
	state := n.state(id)
	state.mu.Lock()
	state.status = status
	state.agentVersion = agentVersion
	state.mu.Unlock()
	return nil
}

// SetStatus reports that the node's agent connection changed. Agent
// connections set a node online when they connect and offline when the
// connection is lost. It rejects an unknown node, an invalid status, and
// the Gateway host node, whose embedded agent keeps it online.
func (n *NodeRegistry) SetStatus(id string, status NodeStatus) error {
	if err := n.checkStatus(id, status); err != nil {
		return err
	}
	state := n.state(id)
	state.mu.Lock()
	state.status = status
	state.mu.Unlock()
	return nil
}

// checkStatus validates a status update for one node.
func (n *NodeRegistry) checkStatus(id string, status NodeStatus) error {
	if n == nil {
		return herrors.New(herrors.Internal,
			"nodes: the node registry is not available", nil)
	}
	if !status.valid() {
		return herrors.New(herrors.Internal,
			"nodes: invalid node status "+quote(string(status)), nil)
	}
	if id == n.gatewayHost && !n.gatewayHostNoEmbeddedAgent {
		return herrors.New(herrors.Internal,
			"nodes: node "+quote(id)+" is the Gateway host: its status is always online and managed by the embedded agent", nil)
	}
	if n.state(id) == nil {
		return herrors.New(herrors.Internal, "nodes: unknown node "+quote(id), nil)
	}
	return nil
}

// Capabilities returns the capabilities the node can execute, sorted by
// capability ID, with each capability's operation IDs sorted. The result
// is a deep copy, so a caller cannot mutate registry state. A node that
// runs nothing, such as a target without an agent, returns an empty slice.
func (n *NodeRegistry) Capabilities(id string) ([]NodeCapability, bool) {
	if n == nil {
		return nil, false
	}
	if n.state(id) == nil {
		return nil, false
	}
	n.capsMu.RLock()
	defer n.capsMu.RUnlock()
	caps, ok := n.capabilities[id]
	if !ok {
		// A known node can execute nothing.
		return []NodeCapability{}, true
	}
	return copyCapabilities(caps), true
}

// nodeIDs returns every node ID, in an unspecified order.
func (n *NodeRegistry) nodeIDs() []string {
	if n == nil {
		return nil
	}
	n.statesMu.RLock()
	defer n.statesMu.RUnlock()
	ids := make([]string, 0, len(n.states))
	for id := range n.states {
		ids = append(ids, id)
	}
	return ids
}

// copyNode deep-copies one node's slice and map fields.
func copyNode(n Node) Node {
	out := n
	out.Roles = append([]string(nil), n.Roles...)
	if n.Attributes != nil {
		out.Attributes = make(map[string]string, len(n.Attributes))
		for k, v := range n.Attributes {
			out.Attributes[k] = v
		}
	}
	return out
}

// copyCapabilities deep-copies one node's capability list.
func copyCapabilities(caps []NodeCapability) []NodeCapability {
	out := make([]NodeCapability, len(caps))
	for i, c := range caps {
		out[i] = NodeCapability{
			Capability: c.Capability,
			Operations: append([]string(nil), c.Operations...),
		}
	}
	return out
}

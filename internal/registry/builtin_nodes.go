package registry

import (
	"fmt"

	herrors "github.com/achmadss/hanse/internal/errors"
)

// addBuiltinCapability adds the executable operations of one builtin
// capability to the node registry's capability mapping.
//
// NewNodeRegistry runs inside Load, before RegisterBuiltin can add a
// builtin capability, so the mapping would otherwise never list one. The
// write happens at startup only, before the Gateway or a test harness
// serves requests, so reads during operation always see the complete
// mapping. A nil registry — for example registry.New's harness registry,
// which has no nodes — accepts nothing and adds nothing.
func (n *NodeRegistry) addBuiltinCapability(c *Capability) error {
	if n == nil || c == nil {
		return nil
	}
	// The node set is read before capsMu is taken, so the lock order stays
	// statesMu then capsMu, the same order a reload uses.
	known := make(map[string]bool, len(n.states))
	for _, id := range n.nodeIDs() {
		known[id] = true
	}
	isKnown := func(id string) bool { return known[id] }
	n.capsMu.Lock()
	defer n.capsMu.Unlock()
	return n.addBuiltinCapabilityLocked(c, isKnown)
}

// addBuiltinCapabilityLocked is addBuiltinCapability with capsMu held. known
// reports whether a node ID is configured. A registry reload calls it while
// it replaces the mapping, against the node set it is about to install.
func (n *NodeRegistry) addBuiltinCapabilityLocked(c *Capability, known func(string) bool) error {
	if n == nil || c == nil {
		return nil
	}
	// nodeOps keeps the same shape nodeCapabilitiesSorted expects,
	// pre-merged over everything the node already has.
	nodeOps := map[string]map[string]map[string]bool{}
	for nodeID := range n.capabilities {
		for _, nc := range n.capabilities[nodeID] {
			ops, ok := nodeOps[nodeID]
			if !ok {
				ops = map[string]map[string]bool{}
				nodeOps[nodeID] = ops
			}
			sets, ok := ops[nc.Capability]
			if !ok {
				sets = map[string]bool{}
				ops[nc.Capability] = sets
			}
			for _, opID := range nc.Operations {
				sets[opID] = true
			}
		}
	}
	for opID, routing := range c.Routing {
		for _, nodeID := range routing.Executors {
			if !known(nodeID) {
				return herrors.New(herrors.Internal,
					fmt.Sprintf("nodes: builtin capability %q routes operation %q to unknown node %q",
						c.ID, opID, nodeID), nil)
			}
			caps, ok := nodeOps[nodeID]
			if !ok {
				caps = map[string]map[string]bool{}
				nodeOps[nodeID] = caps
			}
			sets, ok := caps[c.ID]
			if !ok {
				sets = map[string]bool{}
				caps[c.ID] = sets
			}
			sets[opID] = true
		}
	}
	for nodeID, caps := range nodeOps {
		n.capabilities[nodeID] = nodeCapabilitiesSorted(caps)
	}
	return nil
}

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
	// nodeOps keeps the same shape nodeCapabilitiesSorted expects,
	// pre-merged over everything the node already has.
	nodeOps := map[string]map[string]map[string]bool{}
	for _, nodeID := range n.listNodeIDs() {
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
			if _, ok := n.states[nodeID]; !ok {
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

// listNodeIDs returns every node ID, in an unspecified order.
func (n *NodeRegistry) listNodeIDs() []string {
	ids := make([]string, 0, len(n.states))
	for id := range n.states {
		ids = append(ids, id)
	}
	return ids
}

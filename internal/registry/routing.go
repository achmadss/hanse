package registry

import (
	"fmt"
	"sort"

	herrors "github.com/achmadss/hanse/internal/errors"
)

// joinRoutes produces the effective routing of every operation by joining
// the manifests with routes.yaml (docs/configuration.md sections 5, 9 and
// 10). It rejects:
//   - routing that names an operation the manifest does not define,
//   - a default executor that is not in the eligible list of its level,
//   - an operation list that is not a subset of the capability's list,
//   - any explicitly listed node whose OS has no entry command, and any
//     unknown node ID; `"*"` skips nodes without an entry command,
//   - an http capability that lists any executor other than the Gateway
//     host node (`"*"` means that node).
//
// A capability that routes.yaml does not mention warns; it has no
// eligible executors.
func joinRoutes(reg *Registry, in Input) ([]Warning, error) {
	var warnings []Warning
	for _, c := range reg.Capabilities() {
		route, routed := in.Routes.Capabilities[c.ID]
		if !routed {
			warnings = append(warnings, Warning{
				Capability: c.ID,
				Message:    "routes.yaml does not mention this capability: it has no eligible executors",
			})
			continue
		}
		for _, opID := range opIDsSorted(route) {
			if _, ok := c.Operations[opID]; !ok {
				return nil, herrors.New(herrors.Internal,
					fmt.Sprintf("routes.yaml names operation %q for capability %q, which the manifest does not define",
						opID, c.ID), nil)
			}
		}
		eligible, err := capabilityExecutors(c.ID, c, route, in)
		if err != nil {
			return nil, err
		}
		if err := checkCapabilityDefault(c.ID, route, eligible); err != nil {
			return nil, err
		}
		for _, opID := range c.OperationIDs() {
			executors, def, err := operationRouting(c.ID, opID, eligible, route)
			if err != nil {
				return nil, err
			}
			c.Routing[opID] = OperationRouting{
				Executors:       executors,
				DefaultExecutor: def,
			}
		}
	}
	return warnings, nil
}

// opIDsSorted returns the operation IDs of one route, sorted so that the
// error for an unknown operation is deterministic.
func opIDsSorted(r CapabilityRoute) []string {
	ids := make([]string, 0, len(r.Operations))
	for id := range r.Operations {
		ids = append(ids, id)
	}
	sortStrings(ids)
	return ids
}

// capabilityExecutors expands the capability-level executor list.
func capabilityExecutors(capID string, c *Capability, route CapabilityRoute, in Input) ([]string, error) {
	if c.Manifest.Handler.Kind == KindHTTP {
		// The Gateway makes http handler calls itself, section 5.2: only
		// the Gateway host node may run them.
		gateway := in.GatewayHost
		if gateway == "" {
			return nil, herrors.New(herrors.Internal,
				fmt.Sprintf("capability %q is an http capability but the Gateway host node is not configured", capID), nil)
		}
		if !route.Wildcard {
			for _, id := range route.Executors {
				if id != gateway {
					return nil, herrors.New(herrors.Internal,
						fmt.Sprintf("capability %q is an http capability: executor %q is not the Gateway host node %q",
							capID, id, gateway), nil)
				}
			}
		}
		return []string{gateway}, nil
	}
	entry := c.Manifest.Handler.Entry
	if route.Wildcard {
		// Every enabled node whose OS has an entry command, section 5.1.
		var out []string
		for _, node := range in.nodesSorted() {
			if node.Enabled && hasEntryCommand(entry, node.OS) {
				out = append(out, node.ID)
			}
		}
		return out, nil
	}
	out := make([]string, 0, len(route.Executors))
	for _, id := range route.Executors {
		node, ok := in.node(id)
		if !ok {
			return nil, herrors.New(herrors.Internal,
				fmt.Sprintf("capability %q routes executor %q, which nodes.yaml does not define", capID, id), nil)
		}
		if !hasEntryCommand(entry, node.OS) {
			return nil, herrors.New(herrors.Internal,
				fmt.Sprintf("capability %q routes executor %q, whose OS %q has no entry command in the manifest",
					capID, id, node.OS), nil)
		}
		out = append(out, id)
	}
	return out, nil
}

// hasEntryCommand reports whether an entry map has a command for the OS.
func hasEntryCommand(entry map[string][]string, osname string) bool {
	cmd, ok := entry[osname]
	return ok && len(cmd) > 0
}

// operationRouting computes one operation's executor list and default
// executor. The lists of section 5.2 apply: an operation's list must be a
// subset of the capability's, and its default must be in its own list.
func operationRouting(capID, opID string, capEligible []string, route CapabilityRoute) ([]string, string, error) {
	executors := capEligible
	opRoute, hasOp := route.Operations[opID]
	if hasOp && !opRoute.Wildcard && opRoute.ExecutorsSet {
		byID := make(map[string]bool, len(capEligible))
		for _, id := range capEligible {
			byID[id] = true
		}
		narrowed := make([]string, 0, len(opRoute.Executors))
		for _, id := range opRoute.Executors {
			if !byID[id] {
				return nil, "", herrors.New(herrors.Internal,
					fmt.Sprintf("operation %q of capability %q routes executor %q, which is not in the capability's executor list",
						opID, capID, id), nil)
			}
			narrowed = append(narrowed, id)
		}
		executors = narrowed
	}
	def := route.DefaultExecutor
	if hasOp && opRoute.DefaultSet {
		def = opRoute.DefaultExecutor
	}
	if def != "" && !containsID(executors, def) {
		return nil, "", herrors.New(herrors.Internal,
			fmt.Sprintf("default executor %q of operation %q of capability %q is not in the operation's executor list",
				def, opID, capID), nil)
	}
	return executors, def, nil
}

// checkCapabilityDefault applies the section 5.2 rule: a capability
// default executor belongs to the capability's eligible list.
func checkCapabilityDefault(capID string, route CapabilityRoute, eligible []string) error {
	if route.DefaultExecutor != "" && !containsID(eligible, route.DefaultExecutor) {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("default executor %q of capability %q is not in the capability's executor list",
				route.DefaultExecutor, capID), nil)
	}
	return nil
}

// containsID reports whether the list of node IDs contains id.
func containsID(ids []string, id string) bool {
	for _, one := range ids {
		if one == id {
			return true
		}
	}
	return false
}

// nodesSorted returns the input nodes sorted by ID.
func (in Input) nodesSorted() []*Node {
	out := make([]*Node, 0, len(in.Nodes))
	for i := range in.Nodes {
		out = append(out, &in.Nodes[i])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

package registry

import (
	"fmt"
	"sort"

	herrors "github.com/achmadss/hanse/internal/errors"
)

// routeErr builds an error that names routes.yaml and the field path of
// the problem, such as routes.yaml: capabilities.pc.operations.down.
// default_executor.
func routeErr(field, format string, args ...any) error {
	return herrors.New(herrors.Internal,
		fmt.Sprintf("routes.yaml: %s: %s", field, fmt.Sprintf(format, args...)), nil)
}

// opRouteFields names the routes.yaml fields of one operation.
type opRouteFields struct {
	executors, def string
}

func opRouteFieldsOf(capID, opID string) opRouteFields {
	return opRouteFields{
		executors: "capabilities." + capID + ".operations." + opID + ".executors",
		def:       "capabilities." + capID + ".operations." + opID + ".default_executor",
	}
}

// joinRoutes produces the effective routing of every operation by joining
// the manifests with routes.yaml (docs/configuration.md sections 5, 9 and
// 10). It rejects, collecting every problem it finds:
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
func joinRoutes(reg *Registry, in Input) ([]Warning, []error) {
	var warnings []Warning
	var probs []error
	for _, c := range reg.Capabilities() {
		route, routed := in.Routes.Capabilities[c.ID]
		if !routed {
			warnings = append(warnings, Warning{
				Capability: c.ID,
				Message:    "routes.yaml does not mention this capability: it has no eligible executors",
			})
			continue
		}
		capField := "capabilities." + c.ID
		for _, opID := range opIDsSorted(route) {
			if _, ok := c.Operations[opID]; !ok {
				probs = append(probs, routeErr(capField+".operations."+opID,
					"the manifest does not define this operation"))
			}
		}
		eligible, execErrs := capabilityExecutors(c.ID, c, route, in)
		probs = append(probs, execErrs...)
		if defaultErr := checkCapabilityDefault(c.ID, route, eligible); defaultErr != nil {
			probs = append(probs, defaultErr)
		}
		// Without a capability-level executor list the operation-level
		// subset and default checks would only repeat the capability
		// error, so they are skipped for this capability.
		if len(execErrs) > 0 {
			continue
		}
		for _, opID := range c.OperationIDs() {
			executors, def, err := operationRouting(c.ID, opID, eligible, route)
			if err != nil {
				probs = append(probs, err)
				continue
			}
			c.Routing[opID] = OperationRouting{
				Executors:       executors,
				DefaultExecutor: def,
			}
		}
	}
	// Every operation of every capability keeps an (empty) routing entry
	// from loading; when problems remain, the Gateway will not start and
	// the registry is not returned.
	return warnings, probs
}

// opIDsSorted returns the operation IDs of one route, sorted so that
// error collection order is deterministic.
func opIDsSorted(r CapabilityRoute) []string {
	ids := make([]string, 0, len(r.Operations))
	for id := range r.Operations {
		ids = append(ids, id)
	}
	sortStrings(ids)
	return ids
}

// capabilityExecutors expands the capability-level executor list. A nil
// list with an error marks the expansion as failed; an empty list without
// an error simply means no executor is eligible.
func capabilityExecutors(capID string, c *Capability, route CapabilityRoute, in Input) ([]string, []error) {
	if c.Manifest.Handler.Kind == KindHTTP {
		// The Gateway makes http handler calls itself, section 5.2: only
		// the Gateway host node may run them. An explicit empty list
		// grants nothing, exactly as for exec capabilities.
		if in.GatewayHost == "" {
			return nil, []error{routeErr(capField(capID, "executors"),
				"capability %q is an http capability but the Gateway host node is not configured", capID)}
		}
		if route.Wildcard {
			return []string{in.GatewayHost}, nil
		}
		executors := make([]string, 0, len(route.Executors))
		for _, id := range route.Executors {
			if id != in.GatewayHost {
				return nil, []error{routeErr(capField(capID, "executors"),
					"capability %q is an http capability: executor %q is not the Gateway host node %q",
					capID, id, in.GatewayHost)}
			}
			executors = append(executors, id)
		}
		return executors, nil
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
	executors := make([]string, 0, len(route.Executors))
	var probs []error
	for _, id := range route.Executors {
		node, ok := in.node(id)
		if !ok {
			probs = append(probs, routeErr(capField(capID, "executors"),
				"executor %q is not a node in nodes.yaml", id))
			continue
		}
		if !hasEntryCommand(entry, node.OS) {
			probs = append(probs, routeErr(capField(capID, "executors"),
				"executor %q has OS %q, which has no entry command in the manifest for capability %q",
				id, node.OS, capID))
			continue
		}
		executors = append(executors, id)
	}
	return executors, probs
}

// capField renders a capability-level routes.yaml field path. executors
// is the field that carries routing problems in every case today.
func capField(capID, field string) string {
	return "capabilities." + capID + "." + field
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
	fields := opRouteFieldsOf(capID, opID)
	opRoute, hasOp := route.Operations[opID]
	if hasOp && !opRoute.Wildcard && opRoute.ExecutorsSet {
		byID := make(map[string]bool, len(capEligible))
		for _, id := range capEligible {
			byID[id] = true
		}
		narrowed := make([]string, 0, len(opRoute.Executors))
		for _, id := range opRoute.Executors {
			if !byID[id] {
				return nil, "", routeErr(fields.executors,
					"executor %q is not in the executor list of capability %q", id, capID)
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
		return nil, "", routeErr(fields.def,
			"default executor %q is not in the executor list of operation %q of capability %q",
			def, opID, capID)
	}
	return executors, def, nil
}

// checkCapabilityDefault applies the section 5.2 rule: a capability
// default executor belongs to the capability's eligible list.
func checkCapabilityDefault(capID string, route CapabilityRoute, eligible []string) error {
	if route.DefaultExecutor != "" && !containsID(eligible, route.DefaultExecutor) {
		return routeErr(capField(capID, "default_executor"),
			"default executor %q is not in the executor list of capability %q",
			route.DefaultExecutor, capID)
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

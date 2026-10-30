package config

// CapabilityRoutes is one capability's routing in routes.yaml (docs
// /configuration.md §5.1).
type CapabilityRoutes struct {
	// Executors are the nodes that may run the capability's operations.
	// "*" means every enabled node whose OS has an entry command in the
	// manifest. The manifest-dependent part of "*" is checked with the
	// manifests loaded, not here.
	Executors ExecutorList
	// DefaultExecutor is the executor when a request names none.
	DefaultExecutor string `yaml:"default_executor"`
	// Operations maps the operation ID to its routing.
	Operations map[string]*OperationRoutes `yaml:",omitempty"`
}

// OperationRoutes is one operation's routing.
type OperationRoutes struct {
	// Executors narrows the capability's list for one operation. It
	// cannot add nodes. "*" at operation level means the capability's
	// whole list.
	Executors *ExecutorList `yaml:",omitempty"`
	// DefaultExecutor is the default for one operation. Default: the
	// capability's default.
	DefaultExecutor string `yaml:"default_executor"`
}

// RoutesConfig is the parsed routes.yaml (docs/configuration.md §5).
type RoutesConfig struct {
	// Version is the file format version; it must be 1.
	Version int
	// Capabilities maps the capability ID to its routing.
	Capabilities map[string]*CapabilityRoutes
}

// effectiveList returns the eligible node list for an operation: the
// operation's list when it is set, otherwise the capability's list.
func (c *CapabilityRoutes) effectiveList(op *OperationRoutes) ExecutorList {
	if op != nil && op.Executors != nil {
		if op.Executors.Star {
			return c.Executors // "*" at operation level: the capability whole list
		}
		return ExecutorList{Nodes: op.Executors.Nodes}
	}
	return c.Executors
}

// checkRoutes validates routes.yaml alone. Name is the file's path as
// given on the command line. The per-node existence and enabled checks
// run in checkRouting, which needs nodes.yaml.
func checkRoutes(r *RoutesConfig, file string, problems *Problems) {
	if r.Version != 1 {
		*problems = append(*problems, Problem{file, "version", "must be 1"})
	}
	for capID, cap := range r.Capabilities {
		if !ValidID(capID) {
			*problems = append(*problems, Problem{file, "capabilities." + capID,
				"is not a valid capability ID: lowercase letters, digits, \"_\" and \"-\", starting with a letter, at most 63 characters"})
		}
		checkList(&cap.Executors, problems, file, "capabilities."+capID+".executors", false)
		checkNodeRef(cap.DefaultExecutor, problems, file, "capabilities."+capID+".default_executor")
		for opID, op := range cap.Operations {
			if !ValidID(opID) {
				*problems = append(*problems, Problem{file, "capabilities." + capID + ".operations." + opID,
					"is not a valid operation ID: lowercase letters, digits, \"_\" and \"-\", starting with a letter, at most 63 characters"})
			}
			if op.Executors != nil {
				if op.Executors.Star {
					continue // subset trivially: the capability's whole list
				}
				if cap.Executors.Star {
					continue // every node is a subset of "*"
				}
				checkSubset(op.Executors.Nodes, cap.Executors.Nodes, problems, file,
					"capabilities."+capID+".operations."+opID+".executors")
			}
			checkNodeRef(op.DefaultExecutor, problems, file,
				"capabilities."+capID+".operations."+opID+".default_executor")
		}
	}
}

// checkSubset reports every item in subset that is not in whole.
func checkSubset(subset, whole []string, problems *Problems, file, field string) {
	inWhole := make(map[string]bool, len(whole))
	for _, id := range whole {
		inWhole[id] = true
	}
	for _, id := range subset {
		if id == "*" {
			*problems = append(*problems, Problem{file, field, "must list node IDs; \"*\" alone is the form for all nodes"})
			continue
		}
		if !inWhole[id] {
			*problems = append(*problems, Problem{file, field,
				"is not a subset of the capability's executors: node \"" + id + "\" is not in the capability's list"})
		}
	}
}

// checkNodeRef validates a node reference syntax: empty is "not set", a
// valid ID is fine, "*" is not a node.
func checkNodeRef(id string, problems *Problems, file, field string) {
	switch {
	case id == "":
	case id == "*":
		*problems = append(*problems, Problem{file, field, "names one node, not \"*\""})
	case !ValidID(id):
		*problems = append(*problems, Problem{file, field, "is not a valid node ID: lowercase letters, digits, \"_\" and \"-\", starting with a letter"})
	}
}

// checkList validates an executor list's syntax and default executor.
// The namesInWhole argument reports whether membership can be checked
// locally (false for "*"-shaped lists).
func checkList(e *ExecutorList, problems *Problems, file, field string, _ bool) {
	for _, id := range e.Nodes {
		if id == "*" {
			*problems = append(*problems, Problem{file, field, "must list node IDs; \"*\" alone is the form for all nodes"})
		} else if !ValidID(id) {
			*problems = append(*problems, Problem{file, field, "contains an invalid node ID \"" + id + "\": lowercase letters, digits, \"_\" and \"-\", starting with a letter"})
		}
	}
}

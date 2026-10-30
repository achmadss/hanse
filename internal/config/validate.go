package config

// checkRouting validates what routes.yaml says about nodes: every
// referenced node exists, enabled, and every default executor is in its
// level's eligible list (docs/configuration.md §5.2).
func checkRouting(r *RoutesConfig, nodes *NodesConfig, file string, problems *Problems) {
	for capID, cap := range r.Capabilities {
		base := "capabilities." + capID
		checkRefs(&cap.Executors, nodes, problems, file, base+".executors")
		if cap.DefaultExecutor != "" {
			checkDefault(&cap.Executors, cap.DefaultExecutor, nodes, problems, file, base+".default_executor")
		}
		for opID, op := range cap.Operations {
			opBase := base + ".operations." + opID
			if op.Executors != nil && !op.Executors.Star {
				// The subset check already ran; here only existence and
				// enabled matter.
				checkRefs(&ExecutorList{Nodes: op.Executors.Nodes}, nodes, problems, file, opBase+".executors")
			}
			if op.DefaultExecutor != "" {
				list := cap.effectiveList(op)
				checkDefault(&list, op.DefaultExecutor, nodes, problems, file, opBase+".default_executor")
			}
		}
	}
}

// checkRefs reports every list item that is not a known, enabled node.
// A node listed explicitly but disabled is a problem: a disabled node
// runs nothing and cannot be an executor (docs/configuration.md §4.1).
func checkRefs(e *ExecutorList, nodes *NodesConfig, problems *Problems, file, field string) {
	for _, id := range e.Nodes {
		if !nodes.Has(id) {
			*problems = append(*problems, Problem{file, field, "node \"" + id + "\" does not exist"})
			continue
		}
		if !nodes.Enabled(id) {
			*problems = append(*problems, Problem{file, field, "node \"" + id + "\" is disabled"})
		}
	}
}

// checkDefault checks a default executor against its level's eligible
// list: membership in an explicit list, or an existing enabled node when
// the list is "*" (docs/configuration.md §5.2).
func checkDefault(list *ExecutorList, def string, nodes *NodesConfig, problems *Problems, file, field string) {
	if list.Star {
		if !nodes.Has(def) {
			*problems = append(*problems, Problem{file, field, "node \"" + def + "\" does not exist"})
		} else if !nodes.Enabled(def) {
			*problems = append(*problems, Problem{file, field, "node \"" + def + "\" is disabled, so it is not in \"*\""})
		}
		return
	}
	for _, id := range list.Nodes {
		if id == def {
			return
		}
	}
	*problems = append(*problems, Problem{file, field, "node \"" + def + "\" is not in executors"})
}

// checkGatewayNodes checks the gateway.yaml node reference: it must name
// an enabled node in nodes.yaml (docs/configuration.md §3.2).
func checkGatewayNodes(g *GatewayConfig, nodes *NodesConfig, file string, problems *Problems) {
	if g.Node == "" || !ValidID(g.Node) {
		return // the syntax problem was reported already
	}
	if !nodes.Has(g.Node) {
		*problems = append(*problems, Problem{file, "node", "node \"" + g.Node + "\" does not exist in the nodes file"})
	} else if !nodes.Enabled(g.Node) {
		*problems = append(*problems, Problem{file, "node", "node \"" + g.Node + "\" is disabled, so it cannot be the Gateway host"})
	}
}

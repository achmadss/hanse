package validation

import (
	"fmt"

	"github.com/achmadss/hanse/internal/registry"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// nodeFormat is the one JSON Schema format the Gateway adds
// (docs/configuration.md section 6.3).
const nodeFormat = "hanse:node"

// MatchNode reports whether node passes f, docs/configuration.md
// section 6.4. A node passes when it satisfies every field the filter
// sets; a filter with no fields accepts every node. The options endpoint
// uses this to list the nodes a `hanse:node` argument accepts.
func MatchNode(node registry.NodeView, f registry.NodeFilter) bool {
	return failedField(node, f) == ""
}

// failedField names the first filter field the node does not pass, or ""
// when it passes every set field. The order matches the documentation
// table.
func failedField(node registry.NodeView, f registry.NodeFilter) string {
	if len(f.OS) > 0 && !containsString(f.OS, node.OS) {
		return "os"
	}
	if len(f.Arch) > 0 && !containsString(f.Arch, node.Arch) {
		return "arch"
	}
	if len(f.Roles) > 0 && !sharesString(f.Roles, node.Roles) {
		return "roles"
	}
	if len(f.Attributes) > 0 && !hasAttributes(node.Attributes, f.Attributes) {
		return "attributes"
	}
	return ""
}

// containsString reports whether list holds value.
func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// sharesString reports whether the two lists have a value in common.
func sharesString(want, got []string) bool {
	for _, item := range want {
		if containsString(got, item) {
			return true
		}
	}
	return false
}

// hasAttributes reports whether attributes holds every requested name. An
// attribute with an empty value still counts as present.
func hasAttributes(attributes map[string]string, names []string) bool {
	for _, name := range names {
		if _, ok := attributes[name]; !ok {
			return false
		}
	}
	return true
}

// nodeVisitor walks the compiled schema beside the decoded arguments and
// checks every value the schema marks with the `hanse:node` format. It
// applies the filter of the top-level argument that holds the value.
type nodeVisitor struct {
	nodes    *registry.NodeRegistry
	filters  map[string]registry.NodeFilter
	problems []Problem
	// seen stops a recursive schema from being walked forever.
	seen map[nodeVisit]bool
}

// nodeVisit is one schema and instance location already walked.
type nodeVisit struct {
	schema *jsonschema.Schema
	path   string
}

// nodeProblems returns the `hanse:node` problems of one argument set.
func nodeProblems(op *registry.Operation, nodes *registry.NodeRegistry, value any) []Problem {
	visitor := &nodeVisitor{
		nodes:   nodes,
		filters: op.NodeFilters,
		seen:    map[nodeVisit]bool{},
	}
	visitor.walk(op.Schema, value, nil)
	return visitor.problems
}

// walk checks one instance value against one schema. The handler contract
// section 2.1 allows a `hanse:node` argument anywhere in the arguments,
// including inside arrays and nested objects, so every subschema is
// visited; $ref, allOf, anyOf, oneOf, if, then, and else are followed.
func (v *nodeVisitor) walk(sch *jsonschema.Schema, value any, path []string) {
	if sch == nil {
		return
	}
	visit := nodeVisit{schema: sch, path: pointer(path)}
	if v.seen[visit] {
		return
	}
	v.seen[visit] = true

	v.checkFormat(sch, value, path)
	v.walkObject(sch, value, path)
	v.walkArray(sch, value, path)
	v.walkBranches(sch, value, path)
}

// checkFormat applies the `hanse:node` rule to one value when the schema
// marks it with the format.
func (v *nodeVisitor) checkFormat(sch *jsonschema.Schema, value any, path []string) {
	if sch.Format == nil || sch.Format.Name != nodeFormat {
		return
	}
	id, ok := value.(string)
	if !ok {
		// The format assertion of the schema reports the wrong type.
		return
	}
	if message := v.nodeMessage(id, path); message != "" {
		v.problems = append(v.problems, Problem{
			Path:    pointer(path),
			Rule:    "format",
			Message: message,
		})
	}
}

// nodeMessage reports why id is not acceptable, or "" when it is. The
// message never repeats the value.
func (v *nodeVisitor) nodeMessage(id string, path []string) string {
	node, known := v.nodes.Get(id)
	if !known {
		return "must name a known, enabled node"
	}
	if !node.Enabled {
		return "must name an enabled node"
	}
	if len(path) == 0 {
		return ""
	}
	filter, filtered := v.filters[path[0]]
	if !filtered {
		return ""
	}
	if field := failedField(node, filter); field != "" {
		return fmt.Sprintf("must name a node that passes the argument's %s filter", field)
	}
	return ""
}

// walkObject visits the schemas of an object value.
func (v *nodeVisitor) walkObject(sch *jsonschema.Schema, value any, path []string) {
	object, ok := value.(map[string]any)
	if !ok {
		return
	}
	for name, child := range sch.Properties {
		item, present := object[name]
		if !present {
			continue
		}
		v.walk(child, item, appendSegment(path, name))
	}
	patternMatched := map[string]bool{}
	for pattern, child := range sch.PatternProperties {
		for name, item := range object {
			if !pattern.MatchString(name) {
				continue
			}
			patternMatched[name] = true
			v.walk(child, item, appendSegment(path, name))
		}
	}
	if additional, ok := sch.AdditionalProperties.(*jsonschema.Schema); ok {
		for name, item := range object {
			if _, named := sch.Properties[name]; named || patternMatched[name] {
				continue
			}
			v.walk(additional, item, appendSegment(path, name))
		}
	}
	for _, child := range sch.DependentSchemas {
		v.walk(child, value, path)
	}
}

// walkArray visits the schemas of an array value.
func (v *nodeVisitor) walkArray(sch *jsonschema.Schema, value any, path []string) {
	array, ok := value.([]any)
	if !ok {
		return
	}
	for i, item := range array {
		if i < len(sch.PrefixItems) {
			v.walk(sch.PrefixItems[i], item, appendSegment(path, indexSegment(i)))
		}
	}
	if sch.Items2020 != nil {
		for i, item := range array {
			if i < len(sch.PrefixItems) {
				continue
			}
			v.walk(sch.Items2020, item, appendSegment(path, indexSegment(i)))
		}
		return
	}
	switch items := sch.Items.(type) {
	case *jsonschema.Schema:
		for i, item := range array {
			if i < len(sch.PrefixItems) {
				continue
			}
			v.walk(items, item, appendSegment(path, indexSegment(i)))
		}
	case []*jsonschema.Schema:
		for i, child := range items {
			if i >= len(array) {
				break
			}
			v.walk(child, array[i], appendSegment(path, indexSegment(i)))
		}
	}
}

// walkBranches follows the schema combinators that can hold a subschema
// applying to the same value.
func (v *nodeVisitor) walkBranches(sch *jsonschema.Schema, value any, path []string) {
	if sch.Ref != nil {
		v.walk(sch.Ref, value, path)
	}
	for _, child := range sch.AllOf {
		v.walk(child, value, path)
	}
	for _, child := range sch.AnyOf {
		v.walk(child, value, path)
	}
	for _, child := range sch.OneOf {
		v.walk(child, value, path)
	}
	v.walk(sch.If, value, path)
	v.walk(sch.Then, value, path)
	v.walk(sch.Else, value, path)
}

// appendSegment returns a new path with one segment added.
func appendSegment(path []string, segment string) []string {
	out := make([]string, len(path), len(path)+1)
	copy(out, path)
	return append(out, segment)
}

// indexSegment renders an array index as a JSON pointer segment.
func indexSegment(i int) string { return fmt.Sprintf("%d", i) }

package registry

import (
	"fmt"
	"sort"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// buildOperation assembles the effective operation of one manifest entry:
// it compiles the argument schema and applies the remaining checks that
// need the compiled schema (concurrency keys, options, node filters).
func buildOperation(manifestPath string, m *Manifest, opID string, spec *OperationSpec) (*Operation, error) {
	schema, err := compileArgumentSchema(manifestPath, opField(opID, "arguments"), spec.Arguments)
	if err != nil {
		return nil, err
	}
	if err := validateOptions(manifestPath, opID, spec, schema); err != nil {
		return nil, err
	}
	filters, err := validateNodeFilters(manifestPath, opID, spec, schema)
	if err != nil {
		return nil, err
	}
	concurrency, err := validateConcurrency(manifestPath, opID, spec, schema)
	if err != nil {
		return nil, err
	}
	mode := spec.Mode
	if mode == "" {
		mode = ModeSync
	}
	op := &Operation{
		ID:          opID,
		Description: spec.Description,
		Mode:        mode,
		Privileged:  spec.Privileged,
		Secrets:     append([]string(nil), spec.Secrets...),
		Concurrency: concurrency,
		Schema:      schema,
		NodeFilters: filters,
	}
	timeout, timeoutErr := parseOptionalDuration(spec.Timeout)
	if timeoutErr != nil {
		return nil, manifestErr(manifestPath, opField(opID, "timeout"), "%s", timeoutErr)
	}
	grace, graceErr := parseOptionalDuration(spec.GracePeriod)
	if graceErr != nil {
		return nil, manifestErr(manifestPath, opField(opID, "grace_period"), "%s", graceErr)
	}
	op.Timeout = timeout
	op.GracePeriod = grace
	if m.Handler.Kind == KindExec {
		op.Entry = mergedEntry(m, spec)
	} else {
		op.URL = spec.URL
		if op.URL == "" {
			op.URL = m.Handler.URL
		}
	}
	return op, nil
}

// mergedEntry applies the operation's entry.<os> override over the
// handler's per-OS commands (section 6.2): for every OS the operation
// named, its list wins; other OSes fall back to the handler's.
func mergedEntry(m *Manifest, spec *OperationSpec) map[string][]string {
	merged := make(map[string][]string, len(m.Handler.Entry)+len(spec.Entry))
	for osname, cmd := range m.Handler.Entry {
		merged[osname] = append([]string(nil), cmd...)
	}
	for osname, cmd := range spec.Entry {
		merged[osname] = append([]string(nil), cmd...)
	}
	return merged
}

// validateOptions checks that every options entry names an argument that
// exists in the schema (section 10) and carries the provider value.
func validateOptions(manifestPath string, opID string, spec *OperationSpec, schema *jsonschema.Schema) error {
	names := make([]string, 0, len(spec.Options))
	for name := range spec.Options {
		names = append(names, name)
	}
	sortStrings(names)
	for _, name := range names {
		if name == "" {
			return manifestErr(manifestPath, opField(opID, "options"), "an options entry needs an argument name")
		}
		if _, ok := schemaProperty(schema, name); !ok {
			return manifestErr(manifestPath, opField(opID, "options."+name),
				"option argument %q is not in the argument schema", name)
		}
		if spec.Options[name] == "" {
			return manifestErr(manifestPath, opField(opID, "options."+name),
				"the options entry needs the provider value")
		}
	}
	return nil
}

// validateNodeFilters checks that every node filter belongs to a
// `hanse:node` argument and that its fields follow section 6.4.
func validateNodeFilters(manifestPath string, opID string, spec *OperationSpec, schema *jsonschema.Schema) (map[string]NodeFilter, error) {
	filters := make(map[string]NodeFilter, len(spec.NodeFilters))
	names := make([]string, 0, len(spec.NodeFilters))
	for name := range spec.NodeFilters {
		names = append(names, name)
	}
	sortStrings(names)
	for _, name := range names {
		if name == "" {
			return nil, manifestErr(manifestPath, opField(opID, "node_filters"), "a node filter entry needs an argument name")
		}
		prop, ok := schemaProperty(schema, name)
		if !ok {
			return nil, manifestErr(manifestPath, opField(opID, "node_filters."+name),
				"node filter argument %q is not in the argument schema", name)
		}
		if !typesContain(prop.Types, "string") || prop.Format == nil || prop.Format.Name != nodeFormatName {
			return nil, manifestErr(manifestPath, opField(opID, "node_filters."+name),
				"node filters apply only to a string argument with format %s, and %q is not one", nodeFormatName, name)
		}
		filter := spec.NodeFilters[name]
		for _, osname := range filter.OS {
			if !pureOS(osname) {
				return nil, manifestErr(manifestPath, opField(opID, "node_filters."+name+".os"),
					"node OS must be linux, darwin, or windows, got %q", osname)
			}
		}
		for _, arch := range filter.Arch {
			if !pureArch(arch) {
				return nil, manifestErr(manifestPath, opField(opID, "node_filters."+name+".arch"),
					"node arch must be amd64 or arm64, got %q", arch)
			}
		}
		if err := namesNonEmpty(filter.Roles); err != nil {
			return nil, manifestErr(manifestPath, opField(opID, "node_filters."+name+".roles"), "%s", err)
		}
		if err := namesNonEmpty(filter.Attributes); err != nil {
			return nil, manifestErr(manifestPath, opField(opID, "node_filters."+name+".attributes"), "%s", err)
		}
		filters[name] = filter
	}
	return filters, nil
}

// validateConcurrency checks the run limit and the concurrency key
// arguments of one operation (section 6.2).
func validateConcurrency(manifestPath string, opID string, spec *OperationSpec, schema *jsonschema.Schema) (*Concurrency, error) {
	if spec.Concurrency == nil {
		return nil, nil
	}
	c := spec.Concurrency
	limit := 0
	if c.Limit != nil {
		if *c.Limit < 1 {
			return nil, manifestErr(manifestPath, opField(opID, "concurrency.limit"),
				"concurrency.limit must be at least 1, got %d", *c.Limit)
		}
		limit = *c.Limit
	}
	for i, name := range c.Key {
		if name == "" {
			return nil, manifestErr(manifestPath, opField(opID, fmt.Sprintf("concurrency.key[%d]", i)),
				"a concurrency key entry needs an argument name")
		}
		if _, ok := schemaProperty(schema, name); !ok {
			return nil, manifestErr(manifestPath, opField(opID, "concurrency.key"),
				"concurrency key argument %q is not in the argument schema", name)
		}
	}
	return &Concurrency{Limit: limit, Key: append([]string(nil), c.Key...)}, nil
}

// namesNonEmpty requires every name to be non-empty.
func namesNonEmpty(names []string) error {
	for _, n := range names {
		if n == "" {
			return fmt.Errorf("names must be non-empty")
		}
	}
	return nil
}

// sortStrings sorts a string slice in place.
func sortStrings(s []string) {
	sort.Strings(s)
}

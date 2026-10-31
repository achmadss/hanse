package cli

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/achmadss/hanse/internal/client"
)

// The CLI ships no shell completion mechanism of its own, so completion is
// a hidden command: a shell script calls `hanse __complete <words...>` and
// prints the candidate lines it gets back. The last word is the one being
// completed, which is the convention every completion script uses.
//
//	hanse __complete run ""            # capabilities
//	hanse __complete run pc ""         # the operations of pc
//	hanse __complete run pc up --      # the operation's flags
//	hanse __complete run pc up --node ""  # the valid nodes of that argument
//
// A failed completion prints nothing and exits 0: an unreachable Gateway
// must never break the user's shell.

// commandNames lists the commands the top level completes.
func commandNames() []string {
	return []string{
		"capabilities", "describe", "help", "login", "nodes", "operations",
		"options", "run", "schema", "validate", "version",
	}
}

// completeCommand implements the hidden __complete command.
func (a *app) completeCommand(ctx context.Context, args []string) error {
	words := args
	if len(words) == 0 {
		words = []string{""}
	}
	candidates, err := a.completionCandidates(ctx, words)
	if err != nil {
		return nil
	}
	for _, candidate := range candidates {
		if _, err := fmt.Fprintln(a.opts.Stdout, candidate); err != nil {
			return err
		}
	}
	return nil
}

// completionCandidates returns the candidates for the word being completed.
func (a *app) completionCandidates(ctx context.Context, words []string) ([]string, error) {
	current := words[len(words)-1]
	if len(words) == 1 {
		return filterPrefix(commandNames(), current), nil
	}
	if words[0] != "run" {
		return nil, nil
	}
	c, _, err := a.open(ctx)
	if err != nil {
		return nil, err
	}
	switch len(words) {
	case 2:
		capabilities, err := c.Capabilities(ctx)
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(capabilities.Capabilities))
		for _, capability := range capabilities.Capabilities {
			ids = append(ids, capability.ID)
		}
		return filterPrefix(ids, current), nil
	case 3:
		capability, err := c.Capability(ctx, words[1])
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(capability.Operations))
		for _, operation := range capability.Operations {
			ids = append(ids, operation.ID)
		}
		return filterPrefix(ids, current), nil
	}
	describe, err := c.Describe(ctx, words[1], words[2])
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(current, "-") {
		if index := strings.IndexByte(current, '='); index >= 0 {
			return a.argumentCandidates(ctx, c, describe, strings.TrimLeft(current[:index], "-"), current[index+1:])
		}
		return filterPrefix(runFlagNames(describe), current), nil
	}
	if len(words) >= 2 {
		previous := words[len(words)-2]
		if strings.HasPrefix(previous, "--") && !strings.Contains(previous, "=") {
			return a.argumentCandidates(ctx, c, describe, strings.TrimLeft(previous, "-"), current)
		}
	}
	return nil, nil
}

// runFlagNames returns every flag a run invocation accepts, with the two
// leading dashes the shell sees.
func runFlagNames(describe client.Describe) []string {
	rf, err := newRunFlags(describe)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(rf.arguments)+6)
	rf.fs.VisitAll(func(f *flag.Flag) {
		if f.Name == "password" && !describe.Privileged {
			return
		}
		names = append(names, "--"+f.Name)
	})
	names = append(names, "--help")
	sort.Strings(names)
	return names
}

// argumentCandidates returns the valid values of one argument: the schema's
// enum, a boolean's two values, or the values the Gateway offers for an
// argument with an option provider or a `hanse:node` format.
func (a *app) argumentCandidates(ctx context.Context, c *client.Client, describe client.Describe, field, prefix string) ([]string, error) {
	if field == "executor" {
		ids := make([]string, 0, len(describe.Executors))
		for _, executor := range describe.Executors {
			ids = append(ids, executor.ID)
		}
		return filterPrefix(ids, prefix), nil
	}
	property := schemaProperties(describe.Schema)[field]
	kind := schemaKind(property)
	if kind == argArray {
		kind = arrayElementKind(property)
		property = itemsOf(property)
	}
	if kind == argBoolean {
		return filterPrefix([]string{"false", "true"}, prefix), nil
	}
	if enum := schemaEnum(property); len(enum) > 0 {
		return filterPrefix(enum, prefix), nil
	}
	format, _ := property["format"].(string)
	if format != "hanse:node" && !hasOptionProvider(describe, field) {
		return nil, nil
	}
	nodes, err := a.nodeCandidates(ctx, c, describe, field)
	if err != nil {
		return nil, err
	}
	return filterPrefix(nodes, prefix), nil
}

// hasOptionProvider reports whether the describe response names an option
// provider for one argument.
func hasOptionProvider(describe client.Describe, field string) bool {
	for _, option := range describe.Options {
		if option.Field == field {
			return true
		}
	}
	return false
}

// itemsOf returns the schema of an array's items, or the schema itself when
// it declares no items.
func itemsOf(property map[string]any) map[string]any {
	if items, ok := property["items"].(map[string]any); ok {
		return items
	}
	return property
}

// nodeCandidates lists the node IDs one argument accepts. The options
// endpoint already filters the nodes (docs/configuration.md section 6.4);
// when it has no provider behind it, the node list is filtered here.
func (a *app) nodeCandidates(ctx context.Context, c *client.Client, describe client.Describe, field string) ([]string, error) {
	options, err := c.Options(ctx, describe.Capability, describe.Operation, field)
	if err == nil {
		ids := make([]string, 0, len(options.Options))
		for _, option := range options.Options {
			ids = append(ids, option.Value)
		}
		sort.Strings(ids)
		return ids, nil
	}
	nodes, err := c.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	filter := describe.NodeFilters[field]
	ids := make([]string, 0, len(nodes.Nodes))
	for _, node := range nodes.Nodes {
		if !node.Enabled || !nodePassesFilter(node, filter) {
			continue
		}
		ids = append(ids, node.ID)
	}
	return ids, nil
}

// nodePassesFilter reports whether a node passes every field the filter
// sets (docs/configuration.md section 6.4).
func nodePassesFilter(node client.Node, filter client.DescribeNodeFilter) bool {
	if len(filter.OS) > 0 && !containsString(filter.OS, node.OS) {
		return false
	}
	if len(filter.Arch) > 0 && !containsString(filter.Arch, node.Arch) {
		return false
	}
	if len(filter.Roles) > 0 {
		matches := false
		for _, role := range node.Roles {
			if containsString(filter.Roles, role) {
				matches = true
				break
			}
		}
		if !matches {
			return false
		}
	}
	for _, name := range filter.Attributes {
		if _, ok := node.Attributes[name]; !ok {
			return false
		}
	}
	return true
}

// filterPrefix keeps the candidates that start with prefix, in order.
func filterPrefix(candidates []string, prefix string) []string {
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if strings.HasPrefix(candidate, prefix) {
			out = append(out, candidate)
		}
	}
	return out
}

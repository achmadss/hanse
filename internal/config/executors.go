package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// ExecutorList is the executor-list type used in routes.yaml (docs
// /configuration.md §5.1): either a list of node IDs or the string "*".
type ExecutorList struct {
	// Star is true when the configured value is the string "*".
	Star bool
	// Nodes holds the named node IDs. It is empty when Star is true or
	// when the field is unset (an empty eligible list).
	Nodes []string
}

// IsEmpty reports whether the list names no node and is not "*".
func (e ExecutorList) IsEmpty() bool { return !e.Star && len(e.Nodes) == 0 }

func (e ExecutorList) String() string {
	if e.Star {
		return "*"
	}
	return fmt.Sprint(e.Nodes)
}

// UnmarshalYAML decodes either a sequence of node IDs or the scalar "*".
func (e *ExecutorList) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var s string
		if err := node.Decode(&s); err != nil {
			return err
		}
		switch s {
		case "*":
			e.Star = true
		case "":
			return fmt.Errorf("executor list must be \"*\" or a list of node IDs, not an empty value")
		default:
			e.Nodes = []string{s}
		}
		return nil
	case yaml.SequenceNode:
		var nodes []string
		if err := node.Decode(&nodes); err != nil {
			return err
		}
		e.Nodes = nodes
		return nil
	default:
		return fmt.Errorf("executor list must be \"*\" or a list of node IDs")
	}
}

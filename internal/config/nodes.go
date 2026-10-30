package config

import "path/filepath"

// NodeConfig is one node in nodes.yaml (docs/configuration.md §4).
type NodeConfig struct {
	// Name is a human-readable name. Default: the node ID.
	Name string
	// OS is one of linux, darwin, or windows.
	OS string `yaml:"os"`
	// Arch is amd64 or arm64.
	Arch string
	// Roles are free-form labels used by manifest node filters.
	Roles []string
	// Address is the host name or IP address other nodes use to reach
	// this node.
	Address string
	// Enabled is nil unless the file sets it. Default: enabled. A
	// disabled node runs nothing, cannot be a target, and cannot connect.
	Enabled *bool
	// Attributes are free-form values handlers receive. They must not
	// hold secrets.
	Attributes map[string]any
}

// IsEnabled reports whether the node is enabled. The nil default means
// enabled (docs/configuration.md §4.1).
func (n NodeConfig) IsEnabled() bool {
	return n.Enabled == nil || *n.Enabled
}

// NodesConfig is the parsed nodes.yaml (docs/configuration.md §4).
type NodesConfig struct {
	// Version is the file format version; it must be 1.
	Version int
	// Nodes maps the node ID to its settings.
	Nodes map[string]NodeConfig
}

// Has reports whether id is a known node.
func (n *NodesConfig) Has(id string) bool {
	_, ok := n.Nodes[id]
	return ok
}

// Enabled reports whether id is a known and enabled node.
func (n *NodesConfig) Enabled(id string) bool {
	node, ok := n.Nodes[id]
	return ok && node.IsEnabled()
}

// finalizeNodes fills the documented defaults of every node.
func finalizeNodes(n *NodesConfig) {
	for id, node := range n.Nodes {
		if node.Name == "" {
			node.Name = id
		}
		n.Nodes[id] = node
	}
}

// checkNodes validates nodes.yaml: every ID is valid and every OS and
// architecture is one the agent protocol supports.
func checkNodes(n *NodesConfig, file string, problems *Problems) {
	if n.Version != 1 {
		*problems = append(*problems, Problem{file, "version", "must be 1"})
	}
	for id, node := range n.Nodes {
		if !ValidID(id) {
			*problems = append(*problems, Problem{file, "nodes." + id,
				"is not a valid node ID: lowercase letters, digits, \"_\" and \"-\", starting with a letter, at most 63 characters"})
		}
		switch node.OS {
		case "linux", "darwin", "windows":
		case "":
			*problems = append(*problems, Problem{file, "nodes." + id + ".os", "is required"})
		default:
			*problems = append(*problems, Problem{file, "nodes." + id + ".os", "must be one of linux, darwin, or windows"})
		}
		switch node.Arch {
		case "amd64", "arm64":
		case "":
			*problems = append(*problems, Problem{file, "nodes." + id + ".arch", "is required"})
		default:
			*problems = append(*problems, Problem{file, "nodes." + id + ".arch", "must be amd64 or arm64"})
		}
	}
}

// resolvePath turns p into an absolute path. A relative path resolves
// from baseDir, the directory of the file that contains it (docs
// /configuration.md §2); an absolute path is kept and cleaned.
func resolvePath(baseDir, p string) string {
	if p == "" {
		return p
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(baseDir, p)
}

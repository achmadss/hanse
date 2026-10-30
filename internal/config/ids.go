package config

import "regexp"

// maxIDLength matches docs/configuration.md §2: IDs of nodes, capabilities,
// and operations are at most 63 characters long.
const maxIDLength = 63

// idPattern matches docs/configuration.md §2: IDs of nodes, capabilities,
// and operations match ^[a-z][a-z0-9_-]*$.
var idPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// ValidID reports whether id is a valid node, capability, or operation ID:
// it matches ^[a-z][a-z0-9_-]*$ and is at most 63 characters long.
func ValidID(id string) bool {
	return len(id) <= maxIDLength && idPattern.MatchString(id)
}

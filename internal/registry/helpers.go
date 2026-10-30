package registry

import (
	"strconv"
)

// osValues and archValues are the node values the documentation allows.
// Node files belong to issue #8, but manifests compare entry keys and
// node filter values against them.
var (
	osValues   = map[string]bool{"linux": true, "darwin": true, "windows": true}
	archValues = map[string]bool{"amd64": true, "arm64": true}
)

// pureOS reports whether os is one of the documented node OS values.
func pureOS(os string) bool { return osValues[os] }

// pureArch reports whether arch is one of the documented node arch values.
func pureArch(arch string) bool { return archValues[arch] }

// quote renders s for an error message.
func quote(s string) string { return strconv.Quote(s) }

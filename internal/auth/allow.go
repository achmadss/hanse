package auth

import (
	"strings"

	"github.com/achmadss/hanse/internal/config"
)

// ValidAllowEntry reports whether allow is a valid allow-list entry:
// "<capability>" for every operation of the capability, or
// "<capability>.<operation>" for that operation alone. Both parts are
// IDs (docs/configuration.md §2). Capabilities and operations share the
// ID grammar, so ValidAllowEntry needs no distinction of its own.
func ValidAllowEntry(entry string) bool {
	capID, opID, ok := strings.Cut(entry, ".")
	if !ok {
		return config.ValidID(entry)
	}
	return config.ValidID(capID) && config.ValidID(opID)
}

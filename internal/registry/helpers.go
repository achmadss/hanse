package registry

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// validID reports whether s matches the ID rule from docs/configuration.md
// section 2: `^[a-z][a-z0-9_-]*$`, at most 63 characters. Once issue #8
// merges, this should call internal/config's ID check instead.
func validID(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		d := s[i]
		switch {
		case d >= 'a' && d <= 'z':
		case d >= '0' && d <= '9':
			if i == 0 {
				return false
			}
		case d == '_' || d == '-':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// osValues and archValues are the node values the documentation allows.
// Node files belong to issue #8, but manifests compare entry keys and node
// filter values against them.
var (
	osValues   = map[string]bool{"linux": true, "darwin": true, "windows": true}
	archValues = map[string]bool{"amd64": true, "arm64": true}
)

// pureOS reports whether os is one of the documented node OS values.
func pureOS(os string) bool { return osValues[os] }

// pureArch reports whether arch is one of the documented node arch values.
func pureArch(arch string) bool { return archValues[arch] }

// parseDuration parses the duration format from docs/configuration.md
// section 2: a number followed by one unit from ms, s, m, h, or d.
// Compound forms like "1h30m" and negative values are errors. Once issue
// #8 merges, this should call internal/config's ParseDuration instead.
func parseDuration(s string) (time.Duration, error) {
	fail := func() (time.Duration, error) {
		return 0, fmt.Errorf("invalid duration %q: want a number with one unit ms, s, m, h, or d", s)
	}
	unit, value := "", ""
	switch {
	case strings.HasSuffix(s, "ms"):
		unit, value = "ms", s[:len(s)-2]
	case strings.HasSuffix(s, "s"):
		unit, value = "s", s[:len(s)-1]
	case strings.HasSuffix(s, "m"):
		unit, value = "m", s[:len(s)-1]
	case strings.HasSuffix(s, "h"):
		unit, value = "h", s[:len(s)-1]
	case strings.HasSuffix(s, "d"):
		unit, value = "d", s[:len(s)-1]
	default:
		return fail()
	}
	if value == "" {
		return fail()
	}
	n, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return fail()
	}
	if n < 0 {
		return fail()
	}
	seconds := map[string]float64{
		"ms": 0.001,
		"s":  1,
		"m":  60,
		"h":  3600,
		"d":  86400,
	}[unit]
	d := time.Duration(n * seconds * float64(time.Second))
	if d < 0 {
		return fail()
	}
	return d, nil
}

// quote renders s for an error message.
func quote(s string) string { return strconv.Quote(s) }

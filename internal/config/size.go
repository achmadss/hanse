package config

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Size is a configuration size parsed from values like 16KiB (docs
// /configuration.md §2). The value is a number of bytes.
type Size struct {
	// Value is the configured size in bytes.
	Value int64
}

// ParseSize parses a size value: a number followed by one of the units
// B, KiB, MiB, or GiB.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	for _, unit := range []struct {
		name  string
		scale int64
	}{
		{"GiB", 1 << 30},
		{"MiB", 1 << 20},
		{"KiB", 1 << 10},
		{"B", 1},
	} {
		if !strings.HasSuffix(s, unit.name) {
			continue
		}
		num, err := strconv.ParseInt(strings.TrimSuffix(s, unit.name), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("size %q needs one of the units B, KiB, MiB, or GiB, and the number must be an integer", s)
		}
		if num > (1<<63-1)/unit.scale || num < 0 {
			return 0, fmt.Errorf("size %q is out of range", s)
		}
		return num * unit.scale, nil
	}
	return 0, fmt.Errorf("size %q needs one of the units B, KiB, MiB, or GiB", s)
}

func (s Size) String() string {
	switch {
	case s.Value != 0 && s.Value%(1<<30) == 0:
		return fmt.Sprintf("%dGiB", s.Value/(1<<30))
	case s.Value != 0 && s.Value%(1<<20) == 0:
		return fmt.Sprintf("%dMiB", s.Value/(1<<20))
	case s.Value != 0 && s.Value%(1<<10) == 0:
		return fmt.Sprintf("%dKiB", s.Value/(1<<10))
	default:
		return fmt.Sprintf("%dB", s.Value)
	}
}

// UnmarshalYAML decodes a size scalar.
func (s *Size) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("size must be a string such as 16KiB, not %s", node.Tag)
	}
	v, err := ParseSize(raw)
	if err != nil {
		return err
	}
	s.Value = v
	return nil
}

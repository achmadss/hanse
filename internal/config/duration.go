package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a configuration duration parsed from values like 500ms,
// 10s, 5m, 30d (docs/configuration.md §2). It is a time.Duration that
// decodes strictly from a YAML scalar with one of the supported units.
type Duration struct {
	// Value is the configured duration. Zero means "decode never ran or
	// the field is unset"; defaulted fields keep zero until finalize()
	// fills them.
	Value time.Duration
}

// ParseDuration parses a duration value: a number followed by one of the
// units ms, s, m, h, or d. Plain numbers and other units are errors.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	unitLen := 0
	for _, unit := range []string{"ms", "s", "m", "h", "d"} {
		if strings.HasSuffix(s, unit) {
			unitLen = len(unit)
			break
		}
	}
	if unitLen == 0 {
		return 0, fmt.Errorf("duration %q needs one of the units ms, s, m, h, or d", s)
	}
	num, err := strconv.ParseFloat(s[:len(s)-unitLen], 64)
	if err != nil {
		return 0, fmt.Errorf("duration %q: %v", s, err)
	}
	if num != float64(int64(num)) {
		return 0, fmt.Errorf("duration %q: fractional values are not supported", s)
	}
	// d is a day because the docs define it for durations such as 30d.
	// time.Duration cannot hold more than ~292 years, overflow is the
	// caller's concern.
	const day = 24 * time.Hour
	raw := int64(num)
	switch unitLen {
	case 2: // ms
		return MulDuration(raw, int64(time.Millisecond)), nil
	case 1:
		switch s[len(s)-1] {
		case 's':
			return MulDuration(raw, int64(time.Second)), nil
		case 'm':
			return MulDuration(raw, int64(time.Minute)), nil
		case 'h':
			return MulDuration(raw, int64(time.Hour)), nil
		case 'd':
			return MulDuration(raw, int64(day)), nil
		}
	}
	return 0, fmt.Errorf("duration %q needs one of the units ms, s, m, h, or d", s)
}

// MulDuration multiplies a scalar by a unit without overflowing to
// infinity; an overflow yields the maximum duration, which later
// validation rejects.
func MulDuration(scalar, unit int64) time.Duration {
	if scalar > int64((1<<63-1)/unit) {
		return 1<<63 - 1
	}
	return time.Duration(scalar * unit)
}

func (d Duration) String() string { return d.Value.String() }

// UnmarshalYAML decodes a duration scalar.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string such as 10s, not %s", node.Tag)
	}
	v, err := ParseDuration(s)
	if err != nil {
		return err
	}
	d.Value = v
	return nil
}

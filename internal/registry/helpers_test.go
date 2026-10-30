package registry

import (
	"strings"
	"testing"
	"time"
)

func TestValidID(t *testing.T) {
	ids63 := strings.Repeat("a", 63)
	ids64 := strings.Repeat("a", 64)
	cases := []struct {
		in   string
		want bool
	}{
		{"pc", true},
		{"gateway", true},
		{"node-a", true},
		{"a", true},
		{"a_1-b", true},
		{"a--b", true},
		{"a_", true},
		{ids63, true},
		{"1abc", false},
		{"", false},
		{"UPPER", false},
		{"-a", false},
		{"café", false},
		{ids64, false},
	}
	for _, c := range cases {
		if got := validID(c.in); got != c.want {
			t.Errorf("validID(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"500ms", 500 * time.Millisecond},
		{"10s", 10 * time.Second},
		{"5m", 5 * time.Minute},
		{"3h", 3 * time.Hour},
		{"30d", 30 * 24 * time.Hour},
		{"1d", 24 * time.Hour},
		{"0s", 0},
	}
	for _, c := range cases {
		got, err := parseDuration(c.in)
		if err != nil || got != c.want {
			t.Errorf("parseDuration(%q) = (%v, %v), want %v", c.in, got, err, c.want)
		}
	}
	bad := []string{"", "s", "10", "10x", "1h30m", "-10s", "10 S", "10s ", "0.5d2"}
	for _, in := range bad {
		if got, err := parseDuration(in); err == nil {
			t.Errorf("parseDuration(%q) = %v, want error", in, got)
		}
	}
}

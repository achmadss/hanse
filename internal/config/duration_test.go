package config

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestValidID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want bool
	}{
		{name: "plain letter", id: "a", want: true},
		{name: "letter digits dash underscore", id: "node-1_a", want: true},
		{name: "word", id: "gateway", want: true},
		{name: "starts with letter then dash", id: "a-b", want: true},
		{name: "empty", id: "", want: false},
		{name: "starts with digit", id: "1a", want: false},
		{name: "starts with dash", id: "-a", want: false},
		{name: "starts with underscore", id: "_a", want: false},
		{name: "uppercase", id: "Node", want: false},
		{name: "space", id: "n o", want: false},
		{name: "dot", id: "pc.up", want: false},
		{name: "slash", id: "a/b", want: false},
		{name: "64 characters", id: "a" + strings.Repeat("b", 63), want: false},
		{name: "63 characters", id: "a" + strings.Repeat("b", 62), want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidID(tc.id); got != tc.want {
				t.Errorf("ValidID(%q) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}

func TestValidIDIsDocumentedRegex(t *testing.T) {
	// The pattern is a contract with docs/configuration.md §2. Keep it
	// anchored and exact: ^[a-z][a-z0-9_-]*$ with a length cap of 63.
	if got := idPattern.String(); got != `^[a-z][a-z0-9_-]*$` {
		t.Errorf("idPattern = %q, want ^[a-z][a-z0-9_-]*$", got)
	}
	var re = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	if !re.MatchString("a-9_z") {
		t.Fatal("regex does not match its own documented example set")
	}
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		{name: "ms", value: "500ms", want: 500 * time.Millisecond},
		{name: "s", value: "10s", want: 10 * time.Second},
		{name: "m", value: "5m", want: 5 * time.Minute},
		{name: "h", value: "2h", want: 2 * time.Hour},
		{name: "d", value: "30d", want: 30 * 24 * time.Hour},
		{name: "1d", value: "1d", want: 24 * time.Hour},
		{name: "empty", value: "", wantErr: true},
		{name: "no unit", value: "10", wantErr: true},
		{name: "unknown unit", value: "5w", wantErr: true},
		{name: "unit only", value: "s", wantErr: true},
		{name: "fractional", value: "1.5m", wantErr: true},
		{name: "garbage", value: "soon", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseDuration(tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseDuration(%q) = %v, want an error", tc.value, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDuration(%q) = %v", tc.value, err)
			}
			if got != tc.want {
				t.Errorf("ParseDuration(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestDurationUnmarshalYAML(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		want    time.Duration
		wantErr string
	}{
		{name: "documented examples", yaml: "500ms", want: 500 * time.Millisecond},
		{name: "days", yaml: "30d", want: 30 * 24 * time.Hour},
		{name: "plain number", yaml: "10", wantErr: "needs one of the units"},
		{name: "bad unit", yaml: "5x", wantErr: "needs one of the units"},
		{name: "boolean scalar", yaml: "true", wantErr: "needs one of the units"},
		{name: "sequence", yaml: "[10s]", wantErr: "duration must be a string"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var d Duration
			err := yaml.Unmarshal([]byte(tc.yaml), &d)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("decoded %q without an error", tc.yaml)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("yaml %q: %v", tc.yaml, err)
			}
			if d.Value != tc.want {
				t.Errorf("decoded %v, want %v", d.Value, tc.want)
			}
		})
	}
}

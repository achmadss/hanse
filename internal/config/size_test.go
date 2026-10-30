package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestParseSize(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    int64
		wantErr bool
	}{
		{name: "B", value: "512B", want: 512},
		{name: "KiB", value: "16KiB", want: 16 << 10},
		{name: "MiB", value: "1MiB", want: 1 << 20},
		{name: "GiB", value: "2GiB", want: 2 << 30},
		{name: "no unit", value: "16", wantErr: true},
		{name: "decimal units unsupported", value: "1KB", wantErr: true},
		{name: "unknown unit", value: "1TiB", wantErr: true},
		{name: "inner space", value: "1 KiB", wantErr: true},
		{name: "negative", value: "-1MiB", wantErr: true},
		{name: "empty", value: "", wantErr: true},
		{name: "unit only", value: "KiB", wantErr: true},
		{name: "overflow", value: "9000000000GiB", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSize(tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseSize(%q) = %v, want an error", tc.value, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSize(%q) = %v", tc.value, err)
			}
			if got != tc.want {
				t.Errorf("ParseSize(%q) = %d, want %d", tc.value, got, tc.want)
			}
		})
	}
}

func TestSizeString(t *testing.T) {
	tests := []struct {
		value int64
		want  string
	}{
		{1 << 20, "1MiB"},
		{16 << 10, "16KiB"},
		{2 << 30, "2GiB"},
		{512, "512B"},
		{0, "0B"},
	}
	for _, tc := range tests {
		if got := (Size{Value: tc.value}).String(); got != tc.want {
			t.Errorf("Size(%d).String() = %q, want %q", tc.value, got, tc.want)
		}
	}
}

func TestSizeUnmarshalYAML(t *testing.T) {
	var s Size
	if err := yaml.Unmarshal([]byte("16KiB"), &s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s.Value != 16<<10 {
		t.Errorf("decoded %d, want %d", s.Value, 16<<10)
	}
	if err := yaml.Unmarshal([]byte("1KB"), &s); err == nil || !strings.Contains(err.Error(), "needs one of the units") {
		t.Errorf("error = %v, want the units message", err)
	}
	if err := yaml.Unmarshal([]byte("1024"), &s); err == nil || !strings.Contains(err.Error(), "needs one of the units") {
		t.Errorf("error = %v, want the units message", err)
	}
}

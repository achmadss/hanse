package config

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDefaultTimeouts(t *testing.T) {
	d := DefaultTimeouts()
	if d.Operation != 5*time.Minute {
		t.Errorf("operation = %v, want 5m", d.Operation)
	}
	if d.Options != 10*time.Second {
		t.Errorf("options = %v, want 10s", d.Options)
	}
	if d.GracePeriod != 10*time.Second {
		t.Errorf("grace period = %v, want 10s", d.GracePeriod)
	}
}

func TestResolveTimeouts(t *testing.T) {
	operation := 3 * time.Minute
	grace := 4 * time.Second
	zero := time.Duration(0)
	negative := -time.Second

	tests := []struct {
		name     string
		override *OperationTimeouts
		want     Timeouts
	}{
		{
			name:     "no override keeps defaults",
			override: nil,
			want:     DefaultTimeouts(),
		},
		{
			name:     "full override replaces operation and grace period",
			override: &OperationTimeouts{Timeout: &operation, GracePeriod: &grace},
			want:     Timeouts{Operation: 3 * time.Minute, Options: 10 * time.Second, GracePeriod: 4 * time.Second},
		},
		{
			name:     "operation only override keeps grace period default",
			override: &OperationTimeouts{Timeout: &operation},
			want:     Timeouts{Operation: 3 * time.Minute, Options: 10 * time.Second, GracePeriod: 10 * time.Second},
		},
		{
			name:     "grace period only override keeps operation default",
			override: &OperationTimeouts{GracePeriod: &grace},
			want:     Timeouts{Operation: 5 * time.Minute, Options: 10 * time.Second, GracePeriod: 4 * time.Second},
		},
		{
			name:     "zero values fall back to defaults",
			override: &OperationTimeouts{Timeout: &zero, GracePeriod: &zero},
			want:     DefaultTimeouts(),
		},
		{
			name:     "negative values fall back to defaults",
			override: &OperationTimeouts{Timeout: &negative, GracePeriod: &negative},
			want:     DefaultTimeouts(),
		},
		{
			name:     "custom options default is preserved",
			override: nil,
			want:     Timeouts{Operation: 5 * time.Minute, Options: 2 * time.Second, GracePeriod: 10 * time.Second},
		},
	}
	for _, tc := range tests[:6] {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveTimeouts(DefaultTimeouts(), tc.override)
			if got != tc.want {
				t.Errorf("ResolveTimeouts = %+v, want %+v", got, tc.want)
			}
		})
	}
	// Custom configured defaults pass through unchanged.
	t.Run(tests[6].name, func(t *testing.T) {
		got := ResolveTimeouts(Timeouts{Operation: 5 * time.Minute, Options: 2 * time.Second, GracePeriod: 10 * time.Second}, nil)
		if got != tests[6].want {
			t.Errorf("ResolveTimeouts = %+v, want %+v", got, tests[6].want)
		}
	})
}

func TestHandlerDeadline(t *testing.T) {
	// RFC 3339 input with an offset; the result must be UTC.
	start, err := time.Parse(time.RFC3339, "2026-10-30T09:15:00+02:00")
	if err != nil {
		t.Fatal(err)
	}
	d := HandlerDeadline(start, 5*time.Minute)
	want, err := time.Parse(time.RFC3339, "2026-10-30T07:20:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if !d.Equal(want) {
		t.Errorf("deadline = %v, want %v", d.UTC(), want.UTC())
	}
	if d.Location() != time.UTC {
		t.Errorf("deadline location = %v, want UTC", d.Location())
	}

	b, err := json.Marshal(map[string]string{"deadline": d.Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"deadline":"2026-10-30T07:20:00Z"}` {
		t.Errorf("JSON deadline = %s", b)
	}
}

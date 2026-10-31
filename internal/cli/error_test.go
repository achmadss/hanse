package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	herrors "github.com/achmadss/hanse/internal/errors"
)

// TestWriteError covers an error without details, with scalar details, and
// with nested details. The detail lines are sorted by key and a nested value
// is compact JSON.
func TestWriteError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "no details",
			err:  herrors.New(herrors.Timeout, "", nil),
			want: "hanse: timeout: operation timed out\n",
		},
		{
			name: "scalar details",
			err: herrors.New(herrors.InvalidExecutor, "node-z is unknown", map[string]any{
				"zone":    "loft",
				"count":   json.Number("2"),
				"enabled": true,
			}),
			want: "hanse: invalid_executor: node-z is unknown\n" +
				"  count: 2\n" +
				"  enabled: true\n" +
				"  zone: loft\n",
		},
		{
			name: "nested details",
			err: herrors.New(herrors.HandlerFailed, "not_a_directory: nope", map[string]any{
				"operation_id": "op_01J9Z3",
				"details":      map[string]any{"path": "/tmp/x", "attempts": json.Number("3")},
			}),
			want: "hanse: handler_failed: not_a_directory: nope\n" +
				"  details: {\"attempts\":3,\"path\":\"/tmp/x\"}\n" +
				"  operation_id: op_01J9Z3\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := writeError(&buf, tc.err); err != nil {
				t.Fatalf("writeError: %v", err)
			}
			if got := buf.String(); got != tc.want {
				t.Errorf("writeError =\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

// TestWriteErrorIsStable requires the same error to print the same text on
// every run, whatever order the map yields its keys in.
func TestWriteErrorIsStable(t *testing.T) {
	err := handlerFailure("op_01J9Z3", "not_a_directory", "the file is not a directory",
		map[string]any{"attempts": json.Number("3")})
	var first string
	for i := 0; i < 50; i++ {
		var buf bytes.Buffer
		if writeErr := writeError(&buf, err); writeErr != nil {
			t.Fatalf("writeError: %v", writeErr)
		}
		if i == 0 {
			first = buf.String()
			continue
		}
		if got := buf.String(); got != first {
			t.Fatalf("run %d printed %q, first run printed %q", i, got, first)
		}
	}
	// The handler's own code and message lead the line, so the detail lines
	// do not repeat them.
	if want := "hanse: handler_failed: not_a_directory: the file is not a directory\n" +
		"  details: {\"attempts\":3}\n" +
		"  operation_id: op_01J9Z3\n"; first != want {
		t.Errorf("writeError =\n%s\nwant:\n%s", first, want)
	}
}

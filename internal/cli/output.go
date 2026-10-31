package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/achmadss/hanse/internal/client"
	herrors "github.com/achmadss/hanse/internal/errors"
)

// format is a command's output format.
type format string

const (
	// formatText is the default human-readable output.
	formatText format = "text"
	// formatJSON is deterministic JSON with no colors.
	formatJSON format = "json"
)

// parseFormat validates the --format value.
func parseFormat(value string) (format, error) {
	switch format(strings.ToLower(strings.TrimSpace(value))) {
	case formatText:
		return formatText, nil
	case formatJSON:
		return formatJSON, nil
	default:
		return "", usagef("unknown output format %q: use text or json", value)
	}
}

// writeJSON encodes v as indented JSON with a trailing newline. Keys keep
// struct declaration order and map keys are sorted by encoding/json, so
// the output is deterministic. HTML escaping is off so schema patterns and
// descriptions keep their text unchanged.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// writeJSONLine encodes v as one JSON object on one line (NDJSON), which a
// script or an AI agent can read while a log streams.
func writeJSONLine(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// writeSchemaJSON prints a decoded schema. The schema is decoded into
// maps, which encoding/json orders by key, so the output is stable.
func writeSchemaJSON(w io.Writer, schema any) error {
	return writeJSON(w, schema)
}

// newTable starts a text table with tab-separated columns.
func newTable(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
}

// oneLine flattens a value for a table cell: text from the Gateway must not
// break the table or inject terminal control characters, including escape
// sequences.
func oneLine(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return ' '
		case r < 0x20 || r == 0x7f:
			return -1
		default:
			return r
		}
	}, s)
	return strings.TrimSpace(s)
}

// joinIDs returns the operation IDs of a capability as one cell.
func joinIDs(operations []client.Operation) string {
	ids := make([]string, 0, len(operations))
	for _, op := range operations {
		ids = append(ids, op.ID)
	}
	return strings.Join(ids, ", ")
}

// writeError prints one error for people. The first line is
// "hanse: <code>: <message>". Every detail value that the command has not
// reported yet follows on its own line as "  <key>: <value>", sorted by
// key; a nested value prints as compact JSON. The line is the same on every
// run (docs/architecture.md sections 12 and 14).
func writeError(w io.Writer, err error) error {
	e := herrors.From(err)
	if _, writeErr := fmt.Fprintf(w, "hanse: %s: %s\n", e.Code().Name(), oneLine(e.Message)); writeErr != nil {
		return writeErr
	}
	printed := printedDetailKeys(e)
	for _, key := range sortedKeys(e.Details) {
		if printed[key] {
			continue
		}
		if _, writeErr := fmt.Fprintf(w, "  %s: %s\n", key, detailValue(e.Details[key])); writeErr != nil {
			return writeErr
		}
	}
	return nil
}

// printedDetailKeys returns the detail keys that the command has already
// reported for e, so writeError does not repeat them. A handler failure
// leads with the handler's own code and message.
func printedDetailKeys(e *herrors.Error) map[string]bool {
	printed := map[string]bool{}
	if errors.Is(e, herrors.HandlerFailed) {
		printed["code"] = true
		printed["message"] = true
	}
	return printed
}

// detailValue renders one detail value on one line. A nested value prints
// as compact JSON, whose encoder sorts map keys, so the text is stable.
func detailValue(v any) string {
	switch value := v.(type) {
	case nil:
		return "null"
	case string:
		return oneLine(value)
	case bool, json.Number,
		float32, float64,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		return fmt.Sprint(value)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return oneLine(fmt.Sprint(v))
	}
	return strings.TrimSpace(buf.String())
}

// sortedKeys returns map keys in ascending order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// describeBool renders a boolean for text output.
func describeBool(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

// writeKeyValue prints aligned "key: value" lines.
func writeKeyValue(w io.Writer, rows [][2]string) error {
	tw := newTable(w)
	for _, row := range rows {
		if _, err := fmt.Fprintf(tw, "%s\t%s\n", row[0]+":", oneLine(row[1])); err != nil {
			return err
		}
	}
	return tw.Flush()
}

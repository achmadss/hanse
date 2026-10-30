package operations

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/achmadss/hanse/internal/registry"
)

// redactedPlaceholder replaces every secret value in stored arguments,
// results, summaries, and failures. It matches internal/errors.
const redactedPlaceholder = "[redacted]"

// redactArguments returns the request arguments with every secret value
// removed. An argument is secret when its schema marks it `writeOnly: true`
// or when its name is one of the operation's declared secrets. A value that
// contains the elevation password is redacted too, so the password can
// never reach the history database (docs/architecture.md sections 8.5 and
// 9.6). The return is nil when the arguments cannot be decoded; the caller
// then stores no arguments rather than unredacted ones.
func redactArguments(op *registry.Operation, raw json.RawMessage, password string) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil
	}
	names := map[string]bool{}
	var schema *jsonschema.Schema
	if op != nil {
		for _, name := range op.Secrets {
			names[name] = true
		}
		schema = op.Schema
	}
	encoded, err := json.Marshal(redactValue(value, schema, names, password))
	if err != nil {
		return nil
	}
	return encoded
}

// redactValue walks one decoded JSON value and redacts the parts the schema
// or the operation's secret list marks. Values the schema does not describe
// are still walked, so a password inside a nested object is not missed.
func redactValue(value any, schema *jsonschema.Schema, secretNames map[string]bool, password string) any {
	if schema != nil && schema.WriteOnly {
		return redactedPlaceholder
	}
	switch v := value.(type) {
	case string:
		return redactText(v, password)
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			if secretNames[key] {
				out[key] = redactedPlaceholder
				continue
			}
			out[key] = redactValue(item, propertySchema(schema, key), secretNames, password)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = redactValue(item, itemSchema(schema, i), secretNames, password)
		}
		return out
	default:
		return v
	}
}

// propertySchema returns the schema of one object property, or nil when the
// schema does not describe it.
func propertySchema(schema *jsonschema.Schema, name string) *jsonschema.Schema {
	if schema == nil {
		return nil
	}
	if child, ok := schema.Properties[name]; ok {
		return child
	}
	if child, ok := schema.AdditionalProperties.(*jsonschema.Schema); ok {
		return child
	}
	return nil
}

// itemSchema returns the schema of one array element, or nil when the schema
// does not describe it.
func itemSchema(schema *jsonschema.Schema, index int) *jsonschema.Schema {
	if schema == nil {
		return nil
	}
	if schema.Items2020 != nil {
		return schema.Items2020
	}
	switch items := schema.Items.(type) {
	case *jsonschema.Schema:
		return items
	case []*jsonschema.Schema:
		if index < len(items) {
			return items[index]
		}
	}
	if index < len(schema.PrefixItems) {
		return schema.PrefixItems[index]
	}
	return nil
}

// redactText replaces every occurrence of password in s.
func redactText(s, password string) string {
	if password == "" || !strings.Contains(s, password) {
		return s
	}
	return strings.ReplaceAll(s, password, redactedPlaceholder)
}

// redactAny walks a decoded JSON value and redacts every occurrence of
// password in its strings. It keeps the value's shape.
func redactAny(value any, password string) any {
	switch v := value.(type) {
	case string:
		return redactText(v, password)
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = redactAny(item, password)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = redactAny(item, password)
		}
		return out
	default:
		return v
	}
}

// redactResult redacts the password from a handler result. Invalid JSON,
// which a handler can still produce, is redacted as text so nothing
// unredacted is stored.
func redactResult(raw json.RawMessage, password string) json.RawMessage {
	if len(raw) == 0 || password == "" {
		return raw
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return json.RawMessage(redactText(string(raw), password))
	}
	encoded, err := json.Marshal(redactAny(value, password))
	if err != nil {
		return json.RawMessage(redactText(string(raw), password))
	}
	return encoded
}

// redactFailure returns a copy of f with the password removed from its
// message and details.
func redactFailure(f *Failure, password string) *Failure {
	if f == nil || password == "" {
		return f
	}
	out := &Failure{Code: f.Code, Message: redactText(f.Message, password)}
	if f.Details != nil {
		out.Details = make(map[string]any, len(f.Details))
		for key, value := range f.Details {
			out.Details[key] = redactAny(value, password)
		}
	}
	return out
}

// passwordOf returns the elevation password, or "" when the request had no
// elevation.
func passwordOf(elevation *Elevation) string {
	if elevation == nil {
		return ""
	}
	return elevation.Password
}

// logRedactions returns the values to remove from the log lines of one run:
// every secret argument value and the elevation password
// (docs/architecture.md sections 10 and 15). The longest value comes first,
// so a secret that contains another secret leaves no fragment behind.
func logRedactions(op *registry.Operation, raw json.RawMessage, password string) []string {
	values := secretValues(op, raw)
	if password != "" {
		values = append(values, password)
	}
	return redactionTerms(values)
}

// secretValues returns the string values of the arguments the operation or
// its schema marks secret. A value the schema describes as writeOnly is a
// secret wherever it appears; a name in the operation's secret list makes
// the argument of that name a secret.
func secretValues(op *registry.Operation, raw json.RawMessage) []string {
	trimmed := bytes.TrimSpace(raw)
	if op == nil || len(trimmed) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil
	}
	names := map[string]bool{}
	for _, name := range op.Secrets {
		names[name] = true
	}
	var out []string
	collectSecretValues(value, op.Schema, names, &out)
	return out
}

// collectSecretValues walks one decoded JSON value and appends the strings
// that the schema or the operation's secret list marks.
func collectSecretValues(value any, schema *jsonschema.Schema, secretNames map[string]bool, out *[]string) {
	if schema != nil && schema.WriteOnly {
		*out = append(*out, stringsIn(value)...)
		return
	}
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			if secretNames[key] {
				*out = append(*out, stringsIn(item)...)
			}
			collectSecretValues(item, propertySchema(schema, key), secretNames, out)
		}
	case []any:
		for i, item := range v {
			collectSecretValues(item, itemSchema(schema, i), secretNames, out)
		}
	}
}

// stringsIn returns every string inside a decoded JSON value.
func stringsIn(value any) []string {
	switch v := value.(type) {
	case string:
		return []string{v}
	case map[string]any:
		var out []string
		for _, item := range v {
			out = append(out, stringsIn(item)...)
		}
		return out
	case []any:
		var out []string
		for _, item := range v {
			out = append(out, stringsIn(item)...)
		}
		return out
	default:
		return nil
	}
}

// redactionTerms returns the distinct non-empty terms, longest first.
func redactionTerms(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i] < out[j]
	})
	return out
}

// redactLine replaces every redaction term in one log line. Empty terms and
// empty lines pass through unchanged.
func redactLine(line string, terms []string) string {
	for _, term := range terms {
		if term == "" {
			continue
		}
		line = strings.ReplaceAll(line, term, redactedPlaceholder)
	}
	return line
}

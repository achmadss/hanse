package operations

import (
	"bytes"
	"encoding/json"
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

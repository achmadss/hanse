package registry

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// nodeFormatName is the one JSON Schema format the Gateway adds,
// docs/configuration.md section 6.3.
const nodeFormatName = "hanse:node"

// denyLoader refuses every resource the compiler has not been handed
// explicitly. Argument schemas must be self-contained: a remote $ref would
// otherwise fetch from the network at startup.
type denyLoader struct{}

func (denyLoader) Load(rawURL string) (any, error) {
	return nil, fmt.Errorf("reading %q is not allowed: an argument schema must be self-contained", rawURL)
}

// compileArgumentSchema compiles one operation's `arguments` value, a JSON
// Schema in YAML form, once at load time. The compilation dialect is JSON
// Schema draft 2020-12 and the top level must be `type: object`.
func compileArgumentSchema(manifestPath, field string, raw any) (*jsonschema.Schema, error) {
	if raw == nil {
		return nil, manifestErr(manifestPath, field, "arguments is required")
	}
	// yaml.v3 already decoded the mapping into map[string]any, which
	// json.Marshal turns into valid JSON. UnmarshalJSON preserves number
	// precision the validator needs.
	blob, err := json.Marshal(raw)
	if err != nil {
		return nil, manifestErr(manifestPath, field, "arguments are not a JSON value: %s", err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(blob))
	if err != nil {
		return nil, manifestErr(manifestPath, field, "arguments are not valid JSON Schema: %s", err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseLoader(denyLoader{})
	c.RegisterFormat(&jsonschema.Format{
		Name: nodeFormatName,
		// A `hanse:node` value must be a string that names a node. The
		// registry only checks the shape; the per-request check that the
		// node exists and passes the filter is a later issue.
		Validate: func(v any) error {
			if _, ok := v.(string); !ok {
				return fmt.Errorf("a %s argument must be a string", nodeFormatName)
			}
			return nil
		},
	})
	// Format assertions are annotations by default in draft 2020-12:
	// turning them on keeps later request validation honest.
	c.AssertFormat()
	location := "http://hanse.local/schemas/argument.json"
	if err := c.AddResource(location, doc); err != nil {
		return nil, manifestErr(manifestPath, field, "arguments could not be registered: %s", err)
	}
	sch, err := c.Compile(location)
	if err != nil {
		return nil, manifestErr(manifestPath, field, "arguments do not compile: %s", err)
	}
	if !schemaIsObject(sch) {
		return nil, manifestErr(manifestPath, field, "the top level of arguments must be type: object")
	}
	return sch, nil
}

// schemaIsObject reports whether the compiled schema has exactly the
// "object" type, the rule of section 10: `type: [object, "null"]` is not
// accepted.
func schemaIsObject(sch *jsonschema.Schema) bool {
	if sch == nil || sch.Bool != nil || sch.Types == nil {
		return false
	}
	got := sch.Types.ToStrings()
	return len(got) == 1 && got[0] == "object"
}

// typesContain reports whether a compiled schema's Types value includes
// typ.
func typesContain(t *jsonschema.Types, typ string) bool {
	if t == nil {
		return false
	}
	for _, got := range t.ToStrings() {
		if got == typ {
			return true
		}
	}
	return false
}

// schemaProperty returns the compiled schema of one top-level argument.
func schemaProperty(sch *jsonschema.Schema, name string) (*jsonschema.Schema, bool) {
	if sch == nil || sch.Properties == nil {
		return nil, false
	}
	prop, ok := sch.Properties[name]
	return prop, ok
}

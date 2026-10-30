package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	herrors "github.com/achmadss/hanse/internal/errors"
)

// builtinTestSchema compiles one object-valued argument schema the way a
// manifest's `arguments` value is compiled.
func builtinTestSchema(t *testing.T, text string) *jsonschema.Schema {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		t.Fatalf("test schema does not decode: %v", err)
	}
	schema, err := compileArgumentSchema("builtin_test", "arguments", raw)
	if err != nil {
		t.Fatalf("compileArgumentSchema: %v", err)
	}
	return schema
}

// builtinTestCapability builds one fresh builtin capability, the shape the
// echo test harness uses.
func builtinTestCapability(t *testing.T) *Capability {
	t.Helper()
	return &Capability{
		ID:   "echo",
		Name: "Echo",
		Manifest: &Manifest{
			Version: 1,
			ID:      "echo",
			Name:    "Echo",
			Handler: HandlerSpec{Kind: KindBuiltin},
		},
		Operations: map[string]*Operation{
			"say":   {ID: "say", Mode: ModeSync, Schema: builtinTestSchema(t, `{"type":"object"}`)},
			"sleep": {ID: "sleep", Mode: ModeAsync, Schema: builtinTestSchema(t, `{"type":"object"}`)},
		},
		Routing: map[string]OperationRouting{
			"say":   {Executors: []string{"gateway-host"}, DefaultExecutor: "gateway-host"},
			"sleep": {Executors: []string{"gateway-host"}, DefaultExecutor: "gateway-host"},
		},
	}
}

func codeOf(t *testing.T, err error, want *herrors.Code) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var e *herrors.Error
	if !errors.As(err, &e) {
		t.Fatalf("expected an *herrors.Error, got %T: %v", err, err)
	}
	if e.Code() != want {
		t.Errorf("expected code %s, got %s", want.Name(), e.Code().Name())
	}
}

func TestRegisterBuiltinAndLookup(t *testing.T) {
	r := New()
	capability := builtinTestCapability(t)
	if err := r.RegisterBuiltin(capability); err != nil {
		t.Fatalf("RegisterBuiltin: %v", err)
	}
	got, ok := r.Capability("echo")
	if !ok || got != capability {
		t.Fatalf("Capability(echo) returned %v, %v", got, ok)
	}
	if len(r.Capabilities()) != 1 {
		t.Fatalf("Capabilities() has %d entries", len(r.Capabilities()))
	}
	say, ok := got.Operation("say")
	if !ok || say.Mode != ModeSync {
		t.Fatalf("Operation(say) returned %v, %v", say, ok)
	}
	sleep, ok := got.Operation("sleep")
	if !ok || sleep.Mode != ModeAsync {
		t.Fatalf("Operation(sleep) returned %v, %v", sleep, ok)
	}
	if ids := got.OperationIDs(); len(ids) != 2 || ids[0] != "say" || ids[1] != "sleep" {
		t.Fatalf("OperationIDs() returned %v", ids)
	}
	routing, ok := got.RoutingOf("sleep")
	if !ok || routing.DefaultExecutor != "gateway-host" {
		t.Fatalf("RoutingOf(sleep) returned %v, %v", routing, ok)
	}
}

func TestRegisterBuiltinRejects(t *testing.T) {
	t.Run("nil registry", func(t *testing.T) {
		var r *Registry
		codeOf(t, r.RegisterBuiltin(builtinTestCapability(t)), herrors.Internal)
	})
	t.Run("nil capability", func(t *testing.T) {
		codeOf(t, New().RegisterBuiltin(nil), herrors.Internal)
	})
	t.Run("invalid capability ID", func(t *testing.T) {
		capability := builtinTestCapability(t)
		capability.ID = "Echo"
		codeOf(t, New().RegisterBuiltin(capability), herrors.Internal)
	})
	t.Run("no operations", func(t *testing.T) {
		capability := builtinTestCapability(t)
		capability.Operations = map[string]*Operation{}
		codeOf(t, New().RegisterBuiltin(capability), herrors.Internal)
	})
	t.Run("nil operation", func(t *testing.T) {
		capability := builtinTestCapability(t)
		capability.Operations["ghost"] = nil
		codeOf(t, New().RegisterBuiltin(capability), herrors.Internal)
	})
	t.Run("operation ID mismatch", func(t *testing.T) {
		capability := builtinTestCapability(t)
		capability.Operations = map[string]*Operation{
			"say": {ID: "shout", Mode: ModeSync, Schema: capability.Operations["say"].Schema},
		}
		codeOf(t, New().RegisterBuiltin(capability), herrors.Internal)
	})
	t.Run("invalid operation ID", func(t *testing.T) {
		capability := builtinTestCapability(t)
		capability.Operations = map[string]*Operation{
			"2say": {ID: "2say", Mode: ModeSync, Schema: capability.Operations["say"].Schema},
		}
		codeOf(t, New().RegisterBuiltin(capability), herrors.Internal)
	})
	t.Run("nil schema", func(t *testing.T) {
		capability := builtinTestCapability(t)
		capability.Operations["say"].Schema = nil
		codeOf(t, New().RegisterBuiltin(capability), herrors.Internal)
	})
	t.Run("schema is not an object", func(t *testing.T) {
		capability := builtinTestCapability(t)
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader([]byte(`{"type":"string"}`)))
		if err != nil {
			t.Fatalf("test schema: %v", err)
		}
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		const location = "http://hanse.local/schemas/argument.json"
		if err := c.AddResource(location, doc); err != nil {
			t.Fatalf("test schema: %v", err)
		}
		schema, err := c.Compile(location)
		if err != nil {
			t.Fatalf("test schema: %v", err)
		}
		capability.Operations["say"].Schema = schema
		codeOf(t, New().RegisterBuiltin(capability), herrors.Internal)
	})
	t.Run("duplicate capability", func(t *testing.T) {
		r := New()
		if err := r.RegisterBuiltin(builtinTestCapability(t)); err != nil {
			t.Fatalf("first RegisterBuiltin: %v", err)
		}
		codeOf(t, r.RegisterBuiltin(builtinTestCapability(t)), herrors.Internal)
	})
	t.Run("zero-value registry works", func(t *testing.T) {
		// A zero-value Registry has a nil map; RegisterBuiltin
		// initializes it instead of panicking.
		r := &Registry{}
		if err := r.RegisterBuiltin(builtinTestCapability(t)); err != nil {
			t.Fatalf("RegisterBuiltin on a zero-value registry: %v", err)
		}
		if got := len(r.Capabilities()); got != 1 {
			t.Fatalf("a zero-value registry holds %d capabilities after one registration", got)
		}
	})
}

func TestNewReturnsWriteableRegistry(t *testing.T) {
	r := New()
	if got := r.Capabilities(); len(got) != 0 {
		t.Fatalf("a fresh registry holds %d capabilities", len(got))
	}
	// Capabilities and CapabilityOf keep working on the fresh registry.
	if _, ok := r.Capability("echo"); ok {
		t.Fatal("a fresh registry knows capabilities")
	}
}

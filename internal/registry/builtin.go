package registry

import (
	"fmt"
	"sort"

	"github.com/achmadss/hanse/internal/config"
	herrors "github.com/achmadss/hanse/internal/errors"
)

// KindBuiltin is the handler kind of an operation whose Go handler is
// compiled into the Gateway binary, handler contract section 10. The
// handler contract reserves it for core Gateway operations: a capability
// package cannot declare it, validate.go rejects the kind in a manifest,
// and these capabilities enter the registry only through RegisterBuiltin.
const KindBuiltin = "builtin"

// New returns an empty registry. Its intended use is a registry that a
// caller fills through RegisterBuiltin; scan-based callers use Load.
func New() *Registry {
	return &Registry{capabilities: map[string]*Capability{}}
}

// RegisterBuiltin adds one builtin capability: a capability whose handlers
// are Go code compiled into the Gateway binary, handler contract section
// 10. Such a capability never comes from a manifest directory; the Gateway
// core or a test harness builds it in code and calls this hook, because
// `builtin` is reserved for core Gateway operations.
//
// The checks mirror what Load would enforce on a manifest-derived
// capability: the IDs must be valid, the operation set non-empty, and every
// operation a valid ID with a compiled object argument schema.
func (r *Registry) RegisterBuiltin(c *Capability) error {
	if r == nil {
		return herrors.New(herrors.Internal, "cannot register a builtin capability into a nil registry", nil)
	}
	if c == nil {
		return herrors.New(herrors.Internal, "cannot register a nil builtin capability", nil)
	}
	if !config.ValidID(c.ID) {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("builtin capability ID %q is not a valid capability ID", c.ID), nil)
	}
	if len(c.Operations) == 0 {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("builtin capability %q has no operations", c.ID), nil)
	}
	ids := make([]string, 0, len(c.Operations))
	for id := range c.Operations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		op := c.Operations[id]
		if op == nil {
			return herrors.New(herrors.Internal,
				fmt.Sprintf("builtin capability %q: operation %q is nil", c.ID, id), nil)
		}
		if id != op.ID {
			return herrors.New(herrors.Internal,
				fmt.Sprintf("builtin capability %q: operation %q names ID %q", c.ID, id, op.ID), nil)
		}
		if !config.ValidID(id) {
			return herrors.New(herrors.Internal,
				fmt.Sprintf("builtin capability %q: operation ID %q is not a valid operation ID", c.ID, id), nil)
		}
		if op.Schema == nil {
			return herrors.New(herrors.Internal,
				fmt.Sprintf("builtin capability %q: operation %q has no argument schema", c.ID, id), nil)
		}
		if !schemaIsObject(op.Schema) {
			return herrors.New(herrors.Internal,
				fmt.Sprintf("builtin capability %q: operation %q argument schema is not type: object", c.ID, id), nil)
		}
		// Discovery serves the manifest's raw argument schema, so a
		// builtin operation without one would publish an empty schema
		// that accepts arguments the Gateway rejects.
		if c.Manifest == nil || c.Manifest.Operations == nil || c.Manifest.Operations[id] == nil ||
			c.Manifest.Operations[id].Arguments == nil {
			return herrors.New(herrors.Internal,
				fmt.Sprintf("builtin capability %q: operation %q has no raw argument schema in Manifest.Operations", c.ID, id), nil)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.capabilities == nil {
		r.capabilities = map[string]*Capability{}
	}
	if _, dup := r.capabilities[c.ID]; dup {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("builtin capability %q is registered twice", c.ID), nil)
	}
	if err := r.Nodes.addBuiltinCapability(c); err != nil {
		return err
	}
	r.capabilities[c.ID] = c
	return nil
}

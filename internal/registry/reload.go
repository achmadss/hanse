package registry

import (
	"fmt"

	herrors "github.com/achmadss/hanse/internal/errors"
)

// Reload rescans the configured capability directories and swaps the
// loaded set into this registry without a Gateway restart. It reuses the
// options the initial Load received, so it sees the same directories,
// nodes, and routes.
//
// The swap is atomic for readers: an operation that already looked up a
// capability keeps that snapshot for the rest of its run, and a reader
// either sees the old set or the new one. Builtin capabilities are kept,
// because they are compiled into the Gateway and do not come from disk.
// A package that tries to take a builtin capability's ID is refused.
//
// Reload returns the non-fatal warnings of the new set, the way Load does.
func (r *Registry) Reload() ([]Warning, error) {
	if r == nil {
		return nil, herrors.New(herrors.Internal, "the capability registry is not available", nil)
	}
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()

	fresh, warnings, err := Load(r.opts)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	merged := make(map[string]*Capability, len(fresh.capabilities))
	for id, c := range fresh.capabilities {
		merged[id] = c
	}
	var builtins []*Capability
	for id, c := range r.capabilities {
		if !isBuiltin(c) {
			continue
		}
		if _, replaced := merged[id]; replaced {
			r.mu.Unlock()
			return nil, herrors.New(herrors.Internal,
				fmt.Sprintf("capability %q is built into the Gateway and cannot be replaced by a package", id), nil)
		}
		merged[id] = c
		builtins = append(builtins, c)
	}
	r.capabilities = merged
	r.mu.Unlock()

	if r.Nodes != nil {
		r.Nodes.replaceCapabilities(fresh.Nodes, builtins)
	}
	return warnings, nil
}

// isBuiltin reports whether c came from RegisterBuiltin rather than from a
// manifest directory.
func isBuiltin(c *Capability) bool {
	return c != nil && c.Manifest != nil && c.Manifest.Handler.Kind == KindBuiltin
}

// replaceCapabilities swaps the node-to-capabilities mapping a reload
// produces into this node registry, so nodes keep their IDs, enabled flags,
// and current status. Builtin capabilities are added back on top.
func (n *NodeRegistry) replaceCapabilities(base *NodeRegistry, builtins []*Capability) {
	if n == nil {
		return
	}
	caps := map[string][]NodeCapability{}
	if base != nil {
		for id, entries := range base.capabilities {
			caps[id] = copyCapabilities(entries)
		}
	}
	n.capsMu.Lock()
	defer n.capsMu.Unlock()
	n.capabilities = caps
	for _, c := range builtins {
		_ = n.addBuiltinCapabilityLocked(c)
	}
}

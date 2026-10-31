package registry

import (
	"fmt"
	"sort"

	herrors "github.com/achmadss/hanse/internal/errors"
)

// Reload re-reads the Gateway configuration and swaps the validated result
// into this registry without a Gateway restart
// (docs/architecture.md section 11.2). It calls Options.ReloadInput, when
// set, to read nodes.yaml and routes.yaml again, then rescans the
// capability directories, validates everything, and installs the new
// state. On any problem it reports every problem and leaves the running
// configuration untouched.
//
// The swap is atomic for readers: an operation that already looked up a
// capability keeps that snapshot for the rest of its run, and a reader
// either sees the old set or the new one. Builtin capabilities are kept,
// because they are compiled into the Gateway and do not come from disk.
// A package that tries to take a builtin capability's ID is refused.
//
// Nodes that remain configured keep their presence (status and last agent
// version). A node removed from nodes.yaml leaves the registry, and its ID
// reaches SetNodeRemovedHook. Running operations are not disturbed: their
// calls keep the snapshot they resolved against.
//
// Reload returns the non-fatal warnings of the new set, the way Load does.
func (r *Registry) Reload() ([]Warning, error) {
	if r == nil {
		return nil, herrors.New(herrors.Internal, "the capability registry is not available", nil)
	}
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()

	in := r.opts.Input
	if r.opts.ReloadInput != nil {
		fresh, err := r.opts.ReloadInput()
		if err != nil {
			return nil, err
		}
		in = fresh
	}

	fresh, warnings, err := Load(Options{Dirs: r.opts.Dirs, Input: in})
	if err != nil {
		return nil, err
	}

	// Merge the old builtins into the fresh set before touching the live
	// registry, so a builtin that the new routing cannot place fails the
	// reload instead of leaving half the state new.
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
	hook := r.nodeRemoved
	r.mu.Unlock()

	if fresh.Nodes != nil {
		for _, c := range builtins {
			if err := fresh.Nodes.addBuiltinCapability(c); err != nil {
				return nil, err
			}
		}
	}

	r.mu.Lock()
	r.capabilities = merged
	r.mu.Unlock()

	var removed []string
	if r.Nodes != nil {
		removed = r.Nodes.replaceState(fresh.Nodes)
	}
	if hook != nil {
		for _, id := range removed {
			hook(id)
		}
	}
	return warnings, nil
}

// SetNodeRemovedHook installs the callback a reload calls with the ID of
// every node it removed from nodes.yaml, after the new state is installed.
// The Gateway uses it to close that node's agent connection.
func (r *Registry) SetNodeRemovedHook(fn func(nodeID string)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.nodeRemoved = fn
	r.mu.Unlock()
}

// isBuiltin reports whether c came from RegisterBuiltin rather than from a
// manifest directory.
func isBuiltin(c *Capability) bool {
	return c != nil && c.Manifest != nil && c.Manifest.Handler.Kind == KindBuiltin
}

// replaceState installs a reload's node set and node-to-capability mapping
// into this node registry. Nodes that remain configured keep their status
// and last agent version; new nodes start offline, except the Gateway host
// with an embedded agent, which is always online. The returned IDs are the
// nodes the reload removed, sorted so a caller reports them in a stable
// order.
//
// The node map and the capability mapping are swapped under both locks, in
// the order statesMu then capsMu, which is the order every reader uses.
func (n *NodeRegistry) replaceState(fresh *NodeRegistry) []string {
	if n == nil || fresh == nil {
		return nil
	}
	n.statesMu.Lock()
	n.capsMu.Lock()

	var removed []string
	for id := range n.states {
		if _, ok := fresh.states[id]; !ok {
			removed = append(removed, id)
		}
	}
	sort.Strings(removed)

	next := make(map[string]*nodeState, len(fresh.states))
	for id, state := range fresh.states {
		if old, ok := n.states[id]; ok {
			// Presence belongs to the connection, not to nodes.yaml, so
			// it follows the node ID across the reload.
			old.mu.RLock()
			state.status = old.status
			state.agentVersion = old.agentVersion
			old.mu.RUnlock()
		}
		next[id] = state
	}
	if n.gatewayHost != "" && !n.gatewayHostNoEmbeddedAgent {
		if host, ok := next[n.gatewayHost]; ok {
			// The embedded agent never disconnects
			// (docs/architecture.md section 8).
			host.mu.Lock()
			host.status = StatusOnline
			host.mu.Unlock()
		}
	}
	n.states = next
	n.capabilities = fresh.capabilities

	n.capsMu.Unlock()
	n.statesMu.Unlock()
	return removed
}

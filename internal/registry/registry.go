package registry

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/achmadss/hanse/internal/bundle"
	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// manifestFile is the file that marks a capability directory.
const manifestFile = "manifest.yaml"

// Options is everything Load needs: the directories to scan and the other
// configuration values the registry reads.
type Options struct {
	// Dirs are the capability directories from gateway.yaml's
	// paths.capabilities. Every subdirectory that contains a
	// manifest.yaml is one capability. A missing or empty list loads no
	// capabilities.
	Dirs []string
	// Input carries nodes.yaml and routes.yaml values. Until issue #8
	// merges, callers build those values themselves.
	Input Input
}

// Capability is one loaded capability package: its manifest, the compiled
// argument schemas, the bundle hash, and its effective routing.
type Capability struct {
	ID          string
	Name        string
	Description string
	// Dir is the capability directory the manifest was read from.
	Dir        string
	BundleHash string
	// Manifest is the decoded manifest.yaml.
	Manifest *Manifest
	// Operations maps operation ID to the effective operation: manifest
	// values with per-operation overrides applied and schemas compiled.
	Operations map[string]*Operation
	// Routing maps operation ID to its effective executors and default.
	Routing map[string]OperationRouting
}

// Operation is one operation with its manifest values resolved.
type Operation struct {
	ID          string
	Description string
	// Mode is "sync" or "async".
	Mode string
	// Timeout and GracePeriod are the manifest's per-operation values;
	// nil means the configured Gateway default applies.
	Timeout     *time.Duration
	GracePeriod *time.Duration
	// Privileged is the manifest's privileged flag; exec operations only.
	Privileged bool
	// Secrets are the secret names the handler receives.
	Secrets     []string
	Concurrency *Concurrency
	// Schema is the compiled argument schema, JSON Schema draft 2020-12.
	Schema *jsonschema.Schema
	// NodeFilters map a `hanse:node` argument name to its filter.
	NodeFilters map[string]NodeFilter
	// Entry is the effective entry command per OS, exec handlers only:
	// the operation's entry.<os> overrides the handler's for that OS.
	Entry map[string][]string
	// URL is the effective handler URL, http handlers only: the
	// operation's url overrides the handler's.
	URL string
}

// Concurrency are the run-limiting settings of one operation.
type Concurrency struct {
	// Limit is the most runs of this operation at the same time;
	// 0 means unlimited.
	Limit int
	// Key names the arguments runs are counted separately for.
	Key []string
}

// OperationRouting is the effective routing of one operation.
type OperationRouting struct {
	// Executors are the node IDs that may run the operation. Wildcard
	// expansion is sorted by node ID; explicit lists keep routes.yaml
	// order.
	Executors []string
	// DefaultExecutor is the node a request runs on when it names none;
	// empty when routes.yaml sets none.
	DefaultExecutor string
}

// Warning is a non-fatal problem Load reports, such as a capability that
// routes.yaml does not mention.
type Warning struct {
	Capability string
	Message    string
}

// Registry is the set of loaded capabilities and the node registry built
// from them. Nodes is nil only on a failed Load; every method on
// NodeRegistry tolerates a nil receiver.
//
// A Registry is also the stable handle that Reload updates in place: every
// pointer to it keeps working, and a capability already looked up stays
// valid for the rest of that operation. The capability map is guarded by
// mu so a reload and the readers that share the handle never race.
type Registry struct {
	mu           sync.RWMutex
	capabilities map[string]*Capability
	// Nodes holds every node from nodes.yaml with its current status
	// and the capabilities it can execute.
	Nodes *NodeRegistry
	// opts are the Load options Reload reuses to scan the same
	// directories with the same nodes and routes.
	opts Options
	// reloadMu serializes reloads, so two concurrent requests cannot
	// interleave their scans.
	reloadMu sync.Mutex
}

// Capability returns one capability by ID.
func (r *Registry) Capability(id string) (*Capability, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.capabilities[id]
	return c, ok
}

// Capabilities returns every loaded capability sorted by ID.
func (r *Registry) Capabilities() []*Capability {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	out := make([]*Capability, 0, len(r.capabilities))
	for _, c := range r.capabilities {
		out = append(out, c)
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Operation returns one operation of the capability.
func (c *Capability) Operation(id string) (*Operation, bool) {
	if c == nil {
		return nil, false
	}
	op, ok := c.Operations[id]
	return op, ok
}

// OperationIDs returns the capability's operation IDs sorted.
func (c *Capability) OperationIDs() []string {
	if c == nil {
		return nil
	}
	ids := make([]string, 0, len(c.Operations))
	for id := range c.Operations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// RoutingOf returns the effective routing of one operation.
func (c *Capability) RoutingOf(id string) (OperationRouting, bool) {
	if c == nil {
		return OperationRouting{}, false
	}
	r, ok := c.Routing[id]
	return r, ok
}

// Load scans the capability directories in opts.Dirs, validates every
// manifest, compiles every argument schema, and joins the manifests with
// the routing from opts.Input. It collects every problem it finds — an
// invalid package never stops the scan — and reports them together, per
// docs/configuration.md section 10: any problem still makes Load fail.
// Warnings report non-fatal problems.
func Load(opts Options) (*Registry, []Warning, error) {
	var probs []error
	inputValid := true
	if verr := opts.Input.validate(); verr != nil {
		probs = append(probs, verr)
		inputValid = false
	}
	reg := &Registry{
		capabilities: map[string]*Capability{},
		opts: Options{
			Dirs:  append([]string(nil), opts.Dirs...),
			Input: opts.Input,
		},
	}
	for _, dir := range opts.Dirs {
		dir = strings.TrimRight(dir, string(filepath.Separator))
		if err := checkCapabilityRoot(dir); err != nil {
			probs = append(probs, err)
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			probs = append(probs, herrors.New(herrors.Internal,
				fmt.Sprintf("capability directory %s: %s", errPath(dir), err), nil))
			continue
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			pkgDir := filepath.Join(dir, entry.Name())
			if entry.Type()&os.ModeSymlink != 0 {
				// Complain only when the link points at a capability
				// package; a symlink deeper inside a package makes the
				// bundle walk below report it with the package path.
				if _, statErr := os.Stat(filepath.Join(pkgDir, manifestFile)); statErr == nil {
					probs = append(probs, herrors.New(herrors.Internal,
						fmt.Sprintf("capability package %s is a symbolic link", errPath(pkgDir)), nil))
					continue
				}
			}
			if !entry.IsDir() {
				continue
			}
			if _, statErr := os.Lstat(filepath.Join(pkgDir, manifestFile)); statErr != nil {
				continue // not a capability package
			}
			pkg, err := loadCapability(pkgDir)
			if err != nil {
				probs = append(probs, err)
				continue
			}
			if _, dup := reg.capabilities[pkg.ID]; dup {
				probs = append(probs, herrors.New(herrors.Internal,
					fmt.Sprintf("capability %q is declared twice: in %s and in %s",
						pkg.ID, errPath(reg.capabilities[pkg.ID].Dir), errPath(pkg.Dir)), nil))
				continue
			}
			reg.capabilities[pkg.ID] = pkg
		}
	}
	var warnings []Warning
	if inputValid {
		routeWarnings, routeProbs := joinRoutes(reg, opts.Input)
		if len(routeProbs) > 0 {
			probs = append(probs, routeProbs...)
		}
		warnings = routeWarnings
		nodes, err := NewNodeRegistry(opts.Input, reg.Capabilities())
		if err != nil {
			probs = append(probs, err)
		}
		// The registry is returned only when everything loaded, so the
		// field is set for the success path and its error still fails
		// the Load.
		if len(probs) == 0 {
			reg.Nodes = nodes
		}
	}
	if len(probs) > 0 {
		// A problem stops the Gateway from starting; the warnings of a
		// failed start are not interesting.
		return nil, nil, errors.Join(probs...)
	}
	return reg, warnings, nil
}

// loadCapability processes one capability directory completely: symbolic
// link check, manifest decode and validation, schema compilation, and the
// bundle hash.
func loadCapability(dir string) (*Capability, error) {
	manifestPath := filepath.Join(dir, manifestFile)

	files, err := bundle.Files(dir)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, herrors.New(herrors.Internal,
			fmt.Sprintf("%s: %s", errPath(manifestPath), err), nil)
	}
	var m Manifest
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // unknown manifest keys are errors
	if err := dec.Decode(&m); err != nil {
		return nil, herrors.New(herrors.Internal,
			fmt.Sprintf("%s: manifest is invalid: %s", errPath(manifestPath), err), nil)
	}
	if m.Operations == nil {
		return nil, manifestErr(manifestPath, "operations", "operations is required")
	}
	if err := validateManifest(manifestPath, &m); err != nil {
		return nil, err
	}

	loaded := &Capability{
		ID:          m.ID,
		Name:        m.Name,
		Description: m.Description,
		Dir:         dir,
		Manifest:    &m,
		Operations:  map[string]*Operation{},
		Routing:     map[string]OperationRouting{},
	}
	hash, err := bundle.Hash(dir, files)
	if err != nil {
		return nil, err
	}
	loaded.BundleHash = hash
	ids := m.OperationIDs()
	for _, id := range ids {
		op, err := buildOperation(manifestPath, &m, id, m.Operations[id])
		if err != nil {
			return nil, err
		}
		op.ID = id
		loaded.Operations[id] = op
		// Every operation starts with empty routing; joinRoutes fills it.
		loaded.Routing[id] = OperationRouting{}
	}
	return loaded, nil
}

// checkCapabilityRoot rejects a missing or symlinked scan directory.
func checkCapabilityRoot(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("capability directory %s: %s", errPath(dir), err), nil)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("capability directory %s is not a directory", errPath(dir)), nil)
	}
	return nil
}

// errPath renders a filesystem path for an error message.
func errPath(p string) string { return fmt.Sprintf("%q", filepath.ToSlash(p)) }

// Package capinstall installs and removes capability packages on the
// Gateway host (issue #34). A package is a directory with a manifest.yaml
// and its handler files (docs/architecture.md section 7.1). Install copies
// a local package or clones a git repository, validates the manifest with
// the same rules the Gateway uses at startup, pins the git commit, and moves
// the finished directory into place atomically. Remove deletes one
// installed package after the same checks.
package capinstall

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/achmadss/hanse/internal/config"
	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/registry"
	"gopkg.in/yaml.v3"
)

// pinFile records the source of a git install inside the installed
// directory. It holds no secret: a git URL and a commit SHA are public.
const pinFile = ".hanse-source.json"

// Operation is one operation a package adds, as the install preview shows
// it.
type Operation struct {
	ID          string
	Description string
	Privileged  bool
}

// Package is a fetched and validated capability package that is ready to
// install. Prepare returns it; Install moves it to its final directory.
type Package struct {
	// ID, Name, and Description come from the package's manifest.
	ID          string
	Name        string
	Description string
	// Source and Ref describe where the package came from. They are empty
	// for a local directory copy.
	Source string
	Ref    string
	// SHA is the full commit SHA of a git install, empty otherwise.
	SHA string
	// Operations are the operations the package adds, sorted by ID.
	Operations []Operation

	dir  string
	root string
}

// Dir returns the temporary directory that holds the validated package.
func (p *Package) Dir() string { return p.dir }

// Cleanup removes the temporary directory. It is safe to call more than
// once.
func (p *Package) Cleanup() {
	if p != nil && p.root != "" {
		_ = os.RemoveAll(p.root)
	}
}

// sourcePin is the JSON shape of the pin file.
type sourcePin struct {
	Source string `json:"source"`
	Ref    string `json:"ref,omitempty"`
	Commit string `json:"commit"`
}

// Prepare fetches a package from a git URL, a local git repository, or a
// local directory, validates its manifest, and returns it without changing
// the install target. The caller shows the preview, confirms, and calls
// Install. Ref is optional: a branch, tag, or commit SHA of a git source.
func Prepare(source, ref string) (*Package, error) {
	source = strings.TrimSpace(source)
	root, err := os.MkdirTemp("", "hanse-capinstall-")
	if err != nil {
		return nil, herrors.New(herrors.Internal, "cannot create a temporary directory: "+err.Error(), nil)
	}
	pkg := &Package{Source: source, Ref: ref, root: root, dir: filepath.Join(root, "package")}

	spec, err := classifySource(source)
	if err != nil {
		pkg.Cleanup()
		return nil, err
	}
	if err := validateRef(ref); err != nil {
		pkg.Cleanup()
		return nil, err
	}
	switch spec.kind {
	case sourceGit:
		commit, err := cloneGit(spec.location, ref, pkg.dir)
		if err != nil {
			pkg.Cleanup()
			return nil, err
		}
		pkg.SHA = commit
		pkg.Source = spec.location
	case sourceCopy:
		if ref != "" {
			pkg.Cleanup()
			return nil, herrors.New(herrors.InvalidArguments,
				fmt.Sprintf("%s is a local directory, not a git repository: --ref is used only with a git source", spec.location), nil)
		}
		if err := copyDir(spec.location, pkg.dir); err != nil {
			pkg.Cleanup()
			return nil, err
		}
		pkg.Source = ""
	}

	capability, err := inspect(pkg.dir)
	if err != nil {
		pkg.Cleanup()
		return nil, err
	}
	pkg.ID = capability.ID
	pkg.Name = capability.Name
	pkg.Description = capability.Description
	pkg.Operations = make([]Operation, 0, len(capability.Operations))
	for _, id := range capability.OperationIDs() {
		op := capability.Operations[id]
		if op == nil {
			continue
		}
		pkg.Operations = append(pkg.Operations, Operation{
			ID:          op.ID,
			Description: op.Description,
			Privileged:  op.Privileged,
		})
	}
	return pkg, nil
}

// inspect validates the fetched package with the same loader the Gateway
// uses at startup and returns its capability. The package is the only entry
// of its temporary root, so the loader sees exactly one package.
func inspect(dir string) (*registry.Capability, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.yaml"))
	if err != nil {
		return nil, herrors.New(herrors.InvalidArguments,
			fmt.Sprintf("%s: the package has no manifest.yaml", dir), nil)
	}
	var manifest registry.Manifest
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&manifest); err == nil && manifest.Handler.Kind == registry.KindBuiltin {
		return nil, herrors.New(herrors.InvalidArguments,
			fmt.Sprintf("%s: handler.kind %q is reserved for capabilities built into the Gateway; a package must use %q or %q",
				dir, registry.KindBuiltin, registry.KindExec, registry.KindHTTP), nil)
	}

	loaded, _, err := registry.Load(registry.Options{Dirs: []string{filepath.Dir(dir)}})
	if err != nil {
		return nil, err
	}
	caps := loaded.Capabilities()
	if len(caps) != 1 {
		return nil, herrors.New(herrors.InvalidArguments,
			fmt.Sprintf("%s: the package must hold exactly one manifest.yaml", dir), nil)
	}
	return caps[0], nil
}

// Install installs the prepared package into targetDir. The final directory
// is built under a temporary name in the same parent and renamed into
// place, so a reader never sees a partial package. It refuses an existing
// capability ID and suggests removing it first.
func (p *Package) Install(targetDir string) (string, error) {
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return "", herrors.New(herrors.Internal,
			fmt.Sprintf("cannot create %s: %s", targetDir, err), nil)
	}
	dest := filepath.Join(targetDir, p.ID)
	if _, err := os.Lstat(dest); err == nil {
		return "", Duplicate(p.ID)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", herrors.New(herrors.Internal,
			fmt.Sprintf("cannot check %s: %s", dest, err), nil)
	}

	staging, err := os.MkdirTemp(targetDir, ".hanse-install-")
	if err != nil {
		return "", herrors.New(herrors.Internal,
			fmt.Sprintf("cannot create a staging directory in %s: %s", targetDir, err), nil)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	staged := filepath.Join(staging, p.ID)
	if err := copyDir(p.dir, staged); err != nil {
		return "", err
	}
	if p.SHA != "" {
		if err := writePin(staged, sourcePin{Source: p.Source, Ref: p.Ref, Commit: p.SHA}); err != nil {
			return "", err
		}
	}
	if err := os.Rename(staged, dest); err != nil {
		return "", herrors.New(herrors.Internal,
			fmt.Sprintf("cannot move the package into %s: %s", dest, err), nil)
	}
	return dest, nil
}

// writePin writes the source pin of a git install inside the package
// directory.
func writePin(dir string, pin sourcePin) error {
	data, err := json.MarshalIndent(pin, "", "  ")
	if err != nil {
		return herrors.New(herrors.Internal, "cannot encode the source pin: "+err.Error(), nil)
	}
	data = append(data, '\n')
	path := filepath.Join(dir, pinFile)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("cannot write %s: %s", path, err), nil)
	}
	return nil
}

// Duplicate is the error of installing a capability whose ID exists
// already. It names the remove command, because that is the next step.
func Duplicate(id string) error {
	return herrors.New(herrors.OperationConflict,
		fmt.Sprintf("capability %q is already installed; remove it first with \"hanse capabilities remove %s\"", id, id),
		map[string]any{"capability": id})
}

// Find returns the directory of the installed capability with this ID. Dirs
// are the configured paths.capabilities entries. A package counts only when
// its directory holds a manifest.yaml whose ID matches, which rejects a
// mistyped or unrelated directory.
func Find(id string, dirs []string) (string, bool, error) {
	if !config.ValidID(id) {
		return "", false, herrors.New(herrors.InvalidArguments,
			fmt.Sprintf("%q is not a valid capability ID", id), nil)
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return "", false, herrors.New(herrors.Internal,
				fmt.Sprintf("cannot read %s: %s", dir, err), nil)
		}
		for _, entry := range entries {
			if !entry.IsDir() || entry.Type()&fs.ModeSymlink != 0 {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			got, err := manifestID(filepath.Join(path, "manifest.yaml"))
			if err != nil {
				continue
			}
			if got == id {
				return path, true, nil
			}
		}
	}
	return "", false, nil
}

// Locate returns the directory of an installed capability, or the
// UnknownCapability error.
func Locate(id string, dirs []string) (string, error) {
	path, ok, err := Find(id, dirs)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", herrors.New(herrors.UnknownCapability,
			fmt.Sprintf("capability %q is not installed in %s", id, strings.Join(dirs, ", ")),
			map[string]any{"capability": id})
	}
	return path, nil
}

// Remove deletes an installed capability directory. It checks again that
// the directory holds a manifest.yaml with this ID before deleting anything.
// It returns the removed directory.
func Remove(id string, dirs []string) (string, error) {
	path, err := Locate(id, dirs)
	if err != nil {
		return "", err
	}
	got, err := manifestID(filepath.Join(path, "manifest.yaml"))
	if err != nil || got != id {
		return "", herrors.New(herrors.Internal,
			fmt.Sprintf("%s: the directory does not hold the manifest of capability %q", path, id), nil)
	}
	if err := os.RemoveAll(path); err != nil {
		return "", herrors.New(herrors.Internal,
			fmt.Sprintf("cannot remove %s: %s", path, err), nil)
	}
	return path, nil
}

// manifestID reads the id field of one manifest. It leaves full validation
// to the registry; this helper only identifies installed packages.
func manifestID(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var manifest struct {
		ID string `yaml:"id"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return "", err
	}
	if manifest.ID == "" {
		return "", errors.New("the manifest has no id")
	}
	return manifest.ID, nil
}

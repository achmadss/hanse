package api

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/registry"
)

// BundleSource serves one capability's handler files as the gzip-compressed
// tar archive of docs/agent-protocol.md section 5.2. The capability's
// content hash stays the Gateway's business: the source receives the
// capability ID the registry resolved the hash to.
//
// Issue #22 replaces the default, which reads the capability directory,
// with the bundle store that also keeps the content-addressed copies. It
// plugs in with WithBundleSource.
type BundleSource interface {
	// OpenBundle opens the archive of one capability. The caller closes it.
	OpenBundle(ctx context.Context, capabilityID string) (io.ReadCloser, error)
}

// dirBundleSource serves a capability's files straight from its directory.
// The hash check of the agent protects the transfer; this source only
// packages the files.
type dirBundleSource struct {
	reg *registry.Registry
}

// WithBundleSource sets the bundle source of the agent download route.
func WithBundleSource(source BundleSource) Option {
	return func(s *server) { s.bundles = source }
}

// OpenBundle writes the bundle archive of one capability to a temporary
// file and returns it. The archive is small, and a temporary file keeps the
// response from failing after the headers are written.
func (d *dirBundleSource) OpenBundle(ctx context.Context, capabilityID string) (io.ReadCloser, error) {
	if d == nil || d.reg == nil {
		return nil, herrors.New(herrors.Internal,
			"the capability registry is not available", nil)
	}
	capability, ok := d.reg.Capability(capabilityID)
	if !ok {
		return nil, herrors.New(herrors.Internal,
			fmt.Sprintf("capability %q is not loaded", capabilityID), nil)
	}
	file, err := os.CreateTemp("", "hanse-bundle-*.tar.gz")
	if err != nil {
		return nil, herrors.New(herrors.Internal,
			"cannot create the bundle archive: "+err.Error(), nil)
	}
	name := file.Name()
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(name)
	}
	if err := writeBundleArchive(ctx, file, capability.Dir); err != nil {
		cleanup()
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, herrors.New(herrors.Internal,
			"cannot read the bundle archive: "+err.Error(), nil)
	}
	// The temporary file is deleted when the response closes it, through
	// the wrapper below.
	return &tempFileReader{File: file, name: name}, nil
}

// tempFileReader removes its temporary file when it closes.
type tempFileReader struct {
	*os.File
	name string
}

// Close closes and removes the temporary archive.
func (t *tempFileReader) Close() error {
	err := t.File.Close()
	_ = os.Remove(t.name)
	return err
}

// writeBundleArchive writes the gzip tar archive of one capability
// directory. It lists regular files in byte order of their relative path,
// rejects symbolic links, and keeps the executable bit the bundle hash
// covers.
func writeBundleArchive(ctx context.Context, w io.Writer, root string) error {
	files, err := bundleFileList(root)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, rel := range files {
		if err := ctx.Err(); err != nil {
			return herrors.New(herrors.Internal, "the bundle download was cancelled", nil)
		}
		if err := writeBundleFile(tw, root, rel); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return herrors.New(herrors.Internal,
			"cannot finish the bundle archive: "+err.Error(), nil)
	}
	if err := gz.Close(); err != nil {
		return herrors.New(herrors.Internal,
			"cannot finish the bundle archive: "+err.Error(), nil)
	}
	return nil
}

// bundleFileList lists every regular file of a capability directory,
// relative, sorted by byte order, with `/` as the separator.
func bundleFileList(root string) ([]string, error) {
	var files []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s: the capability directory contains a symbolic link", filepath.ToSlash(path))
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s: the capability directory contains a file that is not regular", filepath.ToSlash(path))
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, herrors.New(herrors.Internal, err.Error(), nil)
	}
	sort.Slice(files, func(i, j int) bool { return strings.Compare(files[i], files[j]) < 0 })
	return files, nil
}

// writeBundleFile adds one file to the archive.
func writeBundleFile(tw *tar.Writer, root, rel string) error {
	path := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Stat(path)
	if err != nil {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("%s: %s", filepath.ToSlash(path), err), nil)
	}
	header := &tar.Header{
		Name:    rel,
		Mode:    int64(0o644),
		Size:    info.Size(),
		ModTime: info.ModTime().UTC(),
		Format:  tar.FormatPAX,
	}
	if info.Mode().Perm()&0o111 != 0 {
		header.Mode = 0o755
	}
	if err := tw.WriteHeader(header); err != nil {
		return herrors.New(herrors.Internal,
			"cannot write the bundle archive: "+err.Error(), nil)
	}
	file, err := os.Open(path)
	if err != nil {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("%s: %s", filepath.ToSlash(path), err), nil)
	}
	defer func() { _ = file.Close() }()
	if _, err := io.Copy(tw, file); err != nil {
		return herrors.New(herrors.Internal,
			"cannot write the bundle archive: "+err.Error(), nil)
	}
	return nil
}

// getBundle answers GET /v1/agent/bundles/{hash}. The node credential
// authenticates it, so like POST /v1/agent/join it sits outside the
// user-token middleware. The Gateway serves only a bundle of a capability
// that the credential's node is an eligible executor for
// (docs/agent-protocol.md section 5.2).
func (s *server) getBundle(w http.ResponseWriter, r *http.Request) {
	if s.agents == nil {
		writeError(w, herrors.New(herrors.Internal,
			"the agent connection hub is not available", nil))
		return
	}
	credential, ok := bearerCredential(r)
	if !ok {
		writeError(w, herrors.New(herrors.AuthenticationFailed, "", nil))
		return
	}
	nodeID, err := s.agents.Authenticate(credential)
	if err != nil {
		writeError(w, err)
		return
	}
	if s.reg == nil {
		writeError(w, errNoRegistry())
		return
	}
	capability, ok := s.capabilityByBundle(r.PathValue("hash"))
	if !ok {
		writeError(w, herrors.New(herrors.InvalidRequest,
			"the bundle hash is not a loaded capability's bundle", nil))
		return
	}
	if !s.nodeMayExecute(nodeID, capability.ID) {
		writeError(w, herrors.New(herrors.Forbidden,
			fmt.Sprintf("node %q is not an eligible executor of capability %q", nodeID, capability.ID),
			map[string]any{"node": nodeID, "capability": capability.ID}))
		return
	}
	source := s.bundles
	if source == nil {
		source = &dirBundleSource{reg: s.reg}
	}
	archive, err := source.OpenBundle(r.Context(), capability.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	defer func() { _ = archive.Close() }()
	w.Header().Set("Content-Type", "application/gzip")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, archive)
}

// capabilityByBundle returns the loaded capability with the given bundle
// hash.
func (s *server) capabilityByBundle(hash string) (*registry.Capability, bool) {
	for _, capability := range s.reg.Capabilities() {
		if capability.BundleHash == hash {
			return capability, true
		}
	}
	return nil, false
}

// nodeMayExecute reports whether the node's routing makes it an eligible
// executor of at least one operation of the capability. The node filters
// of a manifest apply to the target nodes that arguments name, not to the
// executor, so eligibility is the routing list plus the enabled flag.
func (s *server) nodeMayExecute(nodeID, capabilityID string) bool {
	if s.reg == nil || s.reg.Nodes == nil || !s.reg.Nodes.Enabled(nodeID) {
		return false
	}
	capabilities, ok := s.reg.Nodes.Capabilities(nodeID)
	if !ok {
		return false
	}
	for _, capability := range capabilities {
		if capability.Capability == capabilityID && len(capability.Operations) > 0 {
			return true
		}
	}
	return false
}

// bearerCredential reads a node credential from the Authorization header.
func bearerCredential(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

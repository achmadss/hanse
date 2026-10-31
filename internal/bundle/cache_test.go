package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	herrors "github.com/achmadss/hanse/internal/errors"
)

// sourceDir writes a small capability tree and returns its directory.
func sourceDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("version: 1\n"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "handler.sh"), []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatalf("write handler: %v", err)
	}
	return dir
}

// archiveOf packages a directory as the Gateway's gzip tar.
func archiveOf(t *testing.T, dir string) []byte {
	t.Helper()
	files, err := Files(dir)
	if err != nil {
		t.Fatalf("list files: %v", err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		mode := int64(0o644)
		if info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err == nil && info.Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: rel, Mode: mode, Size: int64(len(data)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("write body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

// TestCacheDownloadsOnceAndVerifies proves a bundle is fetched once for
// concurrent runs, re-fetched after local damage, and verified.
func TestCacheDownloadsOnceAndVerifies(t *testing.T) {
	source := sourceDir(t)
	hash, err := HashDir(source)
	if err != nil {
		t.Fatalf("hash source: %v", err)
	}
	archive := archiveOf(t, source)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer hnc_test-credential" {
			t.Errorf("Authorization %q", got)
		}
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(archive)
	}))
	defer server.Close()
	cache := &Cache{
		Dir:        filepath.Join(t.TempDir(), "cache"),
		GatewayURL: server.URL,
		Credential: "hnc_test-credential",
	}
	var wg sync.WaitGroup
	dirs := make([]string, 6)
	errs := make([]error, 6)
	for i := range dirs {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			dirs[index], errs[index] = cache.Ensure(context.Background(), hash)
		}(i)
	}
	wg.Wait()
	for i := range dirs {
		if errs[i] != nil {
			t.Fatalf("ensure %d: %v", i, errs[i])
		}
		if dirs[i] != dirs[0] {
			t.Errorf("ensure %d returned %q, want %q", i, dirs[i], dirs[0])
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("downloads %d, want 1", got)
	}

	// Damage the cached copy: the next Ensure must fetch it again.
	if err := os.WriteFile(filepath.Join(dirs[0], "handler.sh"), []byte("damaged\n"), 0o755); err != nil {
		t.Fatalf("damage cache: %v", err)
	}
	if _, err := cache.Ensure(context.Background(), hash); err != nil {
		t.Fatalf("ensure after damage: %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("downloads after damage %d, want 2", got)
	}
}

// TestCacheRejectsWrongHash answers handler_integrity when the archive does
// not match the requested hash.
func TestCacheRejectsWrongHash(t *testing.T) {
	source := sourceDir(t)
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "other.txt"), []byte("other\n"), 0o644); err != nil {
		t.Fatalf("write other: %v", err)
	}
	wrongHash, err := HashDir(other)
	if err != nil {
		t.Fatalf("hash other: %v", err)
	}
	archive := archiveOf(t, source)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	}))
	defer server.Close()
	cache := &Cache{
		Dir:        filepath.Join(t.TempDir(), "cache"),
		GatewayURL: server.URL,
		Credential: "hnc_test-credential",
	}
	_, err = cache.Ensure(context.Background(), wrongHash)
	if !errors.Is(err, herrors.HandlerIntegrity) {
		t.Fatalf("error %v, want handler_integrity", err)
	}
}

// TestCacheReportsUnavailable covers a failed download.
func TestCacheReportsUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer server.Close()
	cache := &Cache{
		Dir:        filepath.Join(t.TempDir(), "cache"),
		GatewayURL: server.URL,
		Credential: "hnc_test-credential",
	}
	hash := "sha256:" + fmt.Sprintf("%064x", 1)
	_, err := cache.Ensure(context.Background(), hash)
	if !errors.Is(err, herrors.BundleUnavailable) {
		t.Fatalf("error %v, want bundle_unavailable", err)
	}
}

// TestCacheRefusesRedirects proves the node credential is never resent to
// a redirect target, with the default client and with an injected one.
func TestCacheRefusesRedirects(t *testing.T) {
	cases := map[string]*http.Client{
		"default client":  nil,
		"injected client": {},
	}
	for name, injected := range cases {
		t.Run(name, func(t *testing.T) {
			var reached atomic.Int64
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached.Add(1)
				_, _ = w.Write([]byte("the credential must not reach this server"))
			}))
			defer target.Close()
			redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+"/bundle", http.StatusTemporaryRedirect)
			}))
			defer redirect.Close()
			cache := &Cache{
				Dir:        filepath.Join(t.TempDir(), "cache"),
				GatewayURL: redirect.URL,
				Credential: "hnc_test-credential",
				Client:     injected,
			}
			hash := "sha256:" + fmt.Sprintf("%064x", 2)
			_, err := cache.Ensure(context.Background(), hash)
			if !errors.Is(err, herrors.BundleUnavailable) {
				t.Fatalf("error %v, want bundle_unavailable", err)
			}
			if got := reached.Load(); got != 0 {
				t.Fatalf("the redirect target received %d requests, want 0", got)
			}
		})
	}
}

// archiveHash computes the bundle hash of an archive from the modes its
// tar headers record, exactly as the Gateway does on Linux and macOS
// (docs/agent-protocol.md section 5.1).
func archiveHash(t *testing.T, entries []archiveEntry) string {
	t.Helper()
	sorted := append([]archiveEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].name < sorted[j].name })
	lines := sha256.New()
	for _, entry := range sorted {
		mode := "644"
		if entry.mode&0o111 != 0 {
			mode = "755"
		}
		content := sha256.Sum256([]byte(entry.body))
		if _, err := fmt.Fprintf(lines, "%s\x00%s\x00%s\n", entry.name, mode, hex.EncodeToString(content[:])); err != nil {
			t.Fatalf("hash write: %v", err)
		}
	}
	return "sha256:" + hex.EncodeToString(lines.Sum(nil))
}

// TestCacheVerifiesLinuxBundleModes pins issue 88: a bundle whose archive
// records an executable handler as 755 must download and verify from the
// cache on every OS, including Windows, where the unpacked file reports
// mode 644. Before the fix the agent re-hashed the files on disk and
// rejected the bundle with handler_integrity.
func TestCacheVerifiesLinuxBundleModes(t *testing.T) {
	entries := []archiveEntry{
		{name: "handler.sh", body: "#!/bin/sh\necho ok\n", mode: 0o755},
		{name: "manifest.yaml", body: "version: 1\n", mode: 0o644},
	}
	archive, err := io.ReadAll(makeArchive(t, entries...))
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	hash := archiveHash(t, entries)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(archive)
	}))
	defer server.Close()
	cache := &Cache{
		Dir:        filepath.Join(t.TempDir(), "cache"),
		GatewayURL: server.URL,
		Credential: "hnc_test-credential",
	}
	dir, err := cache.Ensure(context.Background(), hash)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("downloads %d, want 1", got)
	}

	// The cached copy must verify from the recorded modes alone, without
	// a download: on Windows the file system cannot report the 755.
	again, err := cache.Ensure(context.Background(), hash)
	if err != nil {
		t.Fatalf("ensure from cache: %v", err)
	}
	if again != dir {
		t.Errorf("ensure returned %q, want %q", again, dir)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("downloads after the cached check %d, want 1", got)
	}

	// The mode list sits beside the bundle directory, so it is not part of
	// the hash.
	hex := strings.TrimPrefix(hash, "sha256:")
	if _, err := os.Stat(filepath.Join(cache.Dir, hex+modesSuffix)); err != nil {
		t.Fatalf("mode list: %v", err)
	}
	files, err := Files(dir)
	if err != nil {
		t.Fatalf("list cache entry: %v", err)
	}
	if got := strings.Join(files, ","); got != "handler.sh,manifest.yaml" {
		t.Fatalf("cache entry holds %q", got)
	}

	// A damaged cached copy is downloaded again and verifies again.
	if err := os.WriteFile(filepath.Join(dir, "handler.sh"), []byte("damaged\n"), 0o755); err != nil {
		t.Fatalf("damage cache: %v", err)
	}
	if _, err := cache.Ensure(context.Background(), hash); err != nil {
		t.Fatalf("ensure after damage: %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("downloads after damage %d, want 2", got)
	}
}

// TestCacheVerifiesEntryWithoutModeList covers a cache entry that an older
// agent wrote, with no mode list beside it: the agent falls back to the
// modes on disk and needs no download.
func TestCacheVerifiesEntryWithoutModeList(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "manifest.yaml"), []byte("version: 1\n"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	hash, err := HashDir(source)
	if err != nil {
		t.Fatalf("hash source: %v", err)
	}
	hex := strings.TrimPrefix(hash, "sha256:")
	dir := filepath.Join(t.TempDir(), "cache")
	entry := filepath.Join(dir, hex)
	if err := os.MkdirAll(entry, 0o700); err != nil {
		t.Fatalf("create cache entry: %v", err)
	}
	if err := os.WriteFile(filepath.Join(entry, "manifest.yaml"), []byte("version: 1\n"), 0o644); err != nil {
		t.Fatalf("write cache entry: %v", err)
	}
	cache := &Cache{Dir: dir, GatewayURL: "https://gateway.example.com", Credential: "hnc_test-credential"}
	got, err := cache.Ensure(context.Background(), hash)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if got != entry {
		t.Errorf("ensure returned %q, want %q", got, entry)
	}
}

package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// archiveEntry is one entry of a test archive.
type archiveEntry struct {
	name     string
	typeflag byte
	body     string
	mode     int64
	linkname string
}

// makeArchive builds a gzip-compressed tar archive.
func makeArchive(t *testing.T, entries ...archiveEntry) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		header := &tar.Header{
			Name:     entry.name,
			Typeflag: entry.typeflag,
			Mode:     entry.mode,
			Size:     int64(len(entry.body)),
			Linkname: entry.linkname,
		}
		if header.Typeflag == 0 {
			header.Typeflag = tar.TypeReg
		}
		if header.Mode == 0 {
			header.Mode = 0o644
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("write header %q: %v", entry.name, err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(entry.body)); err != nil {
				t.Fatalf("write body %q: %v", entry.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return bytes.NewReader(buf.Bytes())
}

// TestUnpackAcceptsRegularFiles covers the normal archive shape.
func TestUnpackAcceptsRegularFiles(t *testing.T) {
	dest := t.TempDir()
	archive := makeArchive(t,
		archiveEntry{name: "manifest.yaml", body: "version: 1\n"},
		archiveEntry{name: "bin/handler.sh", body: "#!/bin/sh\n", mode: 0o755},
	)
	if err := Unpack(archive, dest, DefaultMaxUnpackedBytes); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dest, "bin", "handler.sh"))
	if err != nil {
		t.Fatalf("read unpacked file: %v", err)
	}
	if string(data) != "#!/bin/sh\n" {
		t.Errorf("content %q", data)
	}
}

// TestUnpackRejectsUnsafeArchives covers the entries section 5.2 forbids.
func TestUnpackRejectsUnsafeArchives(t *testing.T) {
	cases := map[string]*bytes.Reader{
		"parent":           makeArchive(t, archiveEntry{name: "../escape", body: "x"}),
		"absolute":         makeArchive(t, archiveEntry{name: "/etc/passwd", body: "x"}),
		"windows absolute": makeArchive(t, archiveEntry{name: `C:\Windows\x`, body: "x"}),
		"symlink": makeArchive(t, archiveEntry{
			name: "link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd",
		}),
		"hard link": makeArchive(t, archiveEntry{
			name: "link", typeflag: tar.TypeLink, linkname: "handler.sh",
		}),
		"device": makeArchive(t, archiveEntry{
			name: "dev", typeflag: tar.TypeChar,
		}),
		"fifo": makeArchive(t, archiveEntry{
			name: "pipe", typeflag: tar.TypeFifo,
		}),
	}
	for name, archive := range cases {
		t.Run(name, func(t *testing.T) {
			err := Unpack(archive, t.TempDir(), DefaultMaxUnpackedBytes)
			if !errors.Is(err, ErrUnsafeArchive) {
				t.Fatalf("unpack error %v, want ErrUnsafeArchive", err)
			}
		})
	}
}

// TestUnpackRejectsOversizedArchive caps the unpacked total.
func TestUnpackRejectsOversizedArchive(t *testing.T) {
	archive := makeArchive(t,
		archiveEntry{name: "one", body: "12345"},
		archiveEntry{name: "two", body: "12345"},
	)
	err := Unpack(archive, t.TempDir(), 8)
	if !errors.Is(err, ErrUnsafeArchive) {
		t.Fatalf("unpack error %v, want ErrUnsafeArchive", err)
	}
}

// TestCachePaths checks hash parsing without touching the network.
func TestCachePaths(t *testing.T) {
	hex, err := cachePath("sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("cachePath: %v", err)
	}
	if hex != "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" {
		t.Errorf("cache path %q", hex)
	}
	if _, err := cachePath("sha256:short"); err == nil {
		t.Error("cachePath accepted a short hash")
	}
	if _, err := cachePath("../escape"); err == nil {
		t.Error("cachePath accepted a path traversal")
	}
}

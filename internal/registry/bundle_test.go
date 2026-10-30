package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBundleHashMatchesHandComputedValue(t *testing.T) {
	// Hand-computed value for this exact tree with python3 hashing
	// `${path}\x00${mode}\x00${sha256(content)}\n` per file:
	//   handler.sh    755  #!/bin/sh\necho ok\n
	//   data/notes.txt 644  notes\n
	const want = "sha256:59620006995a6ec5ea5bd54d99d05e48ad14247fa38ed1d867bdeb55323d98c3"
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "handler.sh"), []byte("#!/bin/sh\necho ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "handler.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "data", "notes.txt"), []byte("notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := bundleFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(files, ","); got != "data/notes.txt,handler.sh" {
		t.Fatalf("files = %q", got)
	}
	got, err := bundleHash(root, files)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("bundle hash = %q, want %q", got, want)
	}
}

func TestBundleHashChangesWithContent(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "handler.sh")
	if err := os.WriteFile(p, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := bundleFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	first, err := bundleHash(root, files)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err = bundleFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := bundleHash(root, files)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("hash did not change after content changed: %q", first)
	}
}

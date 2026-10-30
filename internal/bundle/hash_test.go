package bundle

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBundleHashMatchesHandComputedValue(t *testing.T) {
	// Hand-computed value for this exact tree with python3 hashing
	// `${path}\x00${mode}\x00${sha256(content)}\n` per file:
	//   handler.sh    755  #!/bin/sh\necho ok\n
	//   data/notes.txt 644  notes\n
	// On Windows, agent-protocol 5.1 makes every file count as mode 644,
	// so the hand-computed value differs there.
	const windowsWant = "sha256:8ac76fa8fc222dd971744a901d855391e6a9dc4758285443243749bcabaf3622"
	const othersWant = "sha256:59620006995a6ec5ea5bd54d99d05e48ad14247fa38ed1d867bdeb55323d98c3"
	want := othersWant
	if runtime.GOOS == "windows" {
		want = windowsWant
	}
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
	files, err := Files(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(files, ","); got != "data/notes.txt,handler.sh" {
		t.Fatalf("files = %q", got)
	}
	got, err := Hash(root, files)
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
	files, err := Files(root)
	if err != nil {
		t.Fatal(err)
	}
	first, err := Hash(root, files)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err = Files(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Hash(root, files)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("hash did not change after content changed: %q", first)
	}
}

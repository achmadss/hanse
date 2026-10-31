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

// TestHashWithModesIsPlatformIndependent proves the agent rebuilds the
// Gateway's hash from the modes an archive recorded, on every OS. The
// expected value is the one the Gateway computes on Linux, so the test
// fails on Windows when HashDir replaces it (issue 88).
func TestHashWithModesIsPlatformIndependent(t *testing.T) {
	const want = "sha256:59620006995a6ec5ea5bd54d99d05e48ad14247fa38ed1d867bdeb55323d98c3"
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "handler.sh"), []byte("#!/bin/sh\necho ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "data", "notes.txt"), []byte("notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	modes := map[string]string{"handler.sh": "755", "data/notes.txt": "644"}
	got, err := HashWithModes(root, modes)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("hash with recorded modes = %q, want %q", got, want)
	}
	// The recorded mode is what the hash covers, so the same content with
	// 644 must hash differently.
	got, err = HashWithModes(root, map[string]string{"handler.sh": "644", "data/notes.txt": "644"})
	if err != nil {
		t.Fatal(err)
	}
	if got == want {
		t.Fatal("the hash ignored the recorded mode")
	}
}

// TestHashWithModesRejectsBadModeLists covers a mode list that does not
// describe the directory, or holds a mode other than 755 and 644.
func TestHashWithModesRejectsBadModeLists(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "handler.sh"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := map[string]map[string]string{
		"missing file": {"handler.sh": "755", "gone.txt": "644"},
		"missing mode": {"gone.txt": "644"},
		"bad mode":     {"handler.sh": "700"},
	}
	for name, modes := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := HashWithModes(root, modes); err == nil {
				t.Fatalf("HashWithModes accepted %v", modes)
			}
		})
	}
}

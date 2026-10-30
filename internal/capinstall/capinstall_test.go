package capinstall

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	herrors "github.com/achmadss/hanse/internal/errors"
)

// testManifest is a minimal valid manifest for the capability id.
func testManifest(id string) string {
	return fmt.Sprintf(`version: 1
id: %s
name: %s
description: Test capability %s
contract: hanse.handler/v1

handler:
  kind: exec
  entry:
    linux: [sh, handler.sh]
    darwin: [sh, handler.sh]

operations:
  run:
    description: Run the test handler
    arguments:
      type: object
      additionalProperties: false
      properties:
        value:
          type: string
`, id, id, id)
}

// writePackage writes a valid package directory.
func writePackage(t *testing.T, dir, id string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create package: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(testManifest(id)), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "handler.sh"), []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatalf("write handler: %v", err)
	}
}

func TestPrepareLocalAndInstall(t *testing.T) {
	source := filepath.Join(t.TempDir(), "package")
	writePackage(t, source, "demo")

	pkg, err := Prepare(source, "")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer pkg.Cleanup()
	if pkg.ID != "demo" {
		t.Errorf("ID = %q, want demo", pkg.ID)
	}
	if pkg.SHA != "" {
		t.Errorf("SHA = %q, want empty for a local copy", pkg.SHA)
	}
	if len(pkg.Operations) != 1 || pkg.Operations[0].ID != "run" {
		t.Fatalf("Operations = %+v, want one run operation", pkg.Operations)
	}

	target := filepath.Join(t.TempDir(), "capabilities")
	installed, err := pkg.Install(target)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if installed != filepath.Join(target, "demo") {
		t.Errorf("installed = %q", installed)
	}
	if _, err := os.Stat(filepath.Join(installed, "manifest.yaml")); err != nil {
		t.Errorf("installed manifest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(installed, pinFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a local install must not write a source pin, got %v", err)
	}
	if _, ok, err := Find("demo", []string{target}); err != nil || !ok {
		t.Errorf("Find after install = %v, %v, want found", ok, err)
	}
}

func TestInstallLocalGitRepository(t *testing.T) {
	git := requireGit(t)
	repo := filepath.Join(t.TempDir(), "repo")
	writePackage(t, repo, "demo")
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Hanse Test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Hanse Test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-b", "main")
	run("add", ".")
	run("commit", "-m", "initial")
	want := run("rev-parse", "HEAD")
	run("branch", "stable")

	for _, ref := range []string{"", "main", "stable", want} {
		name := ref
		if name == "" {
			name = "default"
		}
		t.Run(name, func(t *testing.T) {
			pkg, err := Prepare(repo, ref)
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			defer pkg.Cleanup()
			if pkg.SHA != want {
				t.Errorf("SHA = %q, want %q", pkg.SHA, want)
			}
			if pkg.Source != repo {
				t.Errorf("Source = %q, want %q", pkg.Source, repo)
			}
			target := filepath.Join(t.TempDir(), "capabilities")
			installed, err := pkg.Install(target)
			if err != nil {
				t.Fatalf("Install: %v", err)
			}
			data, err := os.ReadFile(filepath.Join(installed, pinFile))
			if err != nil {
				t.Fatalf("read pin: %v", err)
			}
			var pin sourcePin
			if err := json.Unmarshal(data, &pin); err != nil {
				t.Fatalf("decode pin: %v", err)
			}
			if pin.Commit != want {
				t.Errorf("pinned commit = %q, want %q", pin.Commit, want)
			}
			if pin.Source != repo {
				t.Errorf("pinned source = %q, want %q", pin.Source, repo)
			}
			if _, err := os.Stat(filepath.Join(installed, ".git")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the installed package must not hold .git, got %v", err)
			}
		})
	}
}

func TestInvalidManifestRefused(t *testing.T) {
	source := filepath.Join(t.TempDir(), "package")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	// http handler without a URL fails the shared manifest validation.
	manifest := `version: 1
id: broken
description: Broken capability
contract: hanse.handler/v1
handler:
  kind: http
operations:
  run:
    description: Run it
    arguments:
      type: object
`
	if err := os.WriteFile(filepath.Join(source, "manifest.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if pkg, err := Prepare(source, ""); err == nil {
		pkg.Cleanup()
		t.Fatal("Prepare accepted an invalid manifest")
	}
}

func TestBuiltinHandlerRefused(t *testing.T) {
	source := filepath.Join(t.TempDir(), "package")
	writePackage(t, source, "demo")
	manifest := strings.Replace(testManifest("demo"),
		"handler:\n  kind: exec\n  entry:\n    linux: [sh, handler.sh]\n    darwin: [sh, handler.sh]",
		"handler:\n  kind: builtin", 1)
	if err := os.WriteFile(filepath.Join(source, "manifest.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	pkg, err := Prepare(source, "")
	if err == nil {
		pkg.Cleanup()
		t.Fatal("Prepare accepted handler.kind builtin")
	}
	if !strings.Contains(err.Error(), "builtin") {
		t.Errorf("error = %v, want it to name builtin", err)
	}
}

func TestInvalidIDRefused(t *testing.T) {
	source := filepath.Join(t.TempDir(), "package")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "manifest.yaml"), []byte(testManifest("Bad_ID")), 0o644); err != nil {
		t.Fatal(err)
	}
	if pkg, err := Prepare(source, ""); err == nil {
		pkg.Cleanup()
		t.Fatal("Prepare accepted an invalid capability ID")
	}
}

func TestDuplicateIDRefused(t *testing.T) {
	source := filepath.Join(t.TempDir(), "package")
	writePackage(t, source, "demo")
	target := filepath.Join(t.TempDir(), "capabilities")

	first, err := Prepare(source, "")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer first.Cleanup()
	if _, err := first.Install(target); err != nil {
		t.Fatalf("first Install: %v", err)
	}

	second, err := Prepare(source, "")
	if err != nil {
		t.Fatalf("second Prepare: %v", err)
	}
	defer second.Cleanup()
	if _, err := second.Install(target); !errors.Is(err, herrors.OperationConflict) {
		t.Fatalf("second Install error = %v, want operation_conflict", err)
	}
	if _, ok, err := Find("demo", []string{target}); err != nil || !ok {
		t.Errorf("Find after duplicate = %v, %v, want found", ok, err)
	}
}

func TestSymlinkEscapeRefused(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "package")
	writePackage(t, source, "demo")
	if err := os.Symlink(outside, filepath.Join(source, "escape")); err != nil {
		t.Skipf("this platform cannot create symbolic links: %v", err)
	}
	pkg, err := Prepare(source, "")
	if err == nil {
		pkg.Cleanup()
		t.Fatal("Prepare followed a symbolic link outside the package")
	}
	if !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("error = %v, want it to name the symbolic link", err)
	}
}

func TestFindUsesTheManifestID(t *testing.T) {
	target := filepath.Join(t.TempDir(), "capabilities")
	dir := filepath.Join(target, "not-named-demo")
	writePackage(t, dir, "demo")

	path, ok, err := Find("demo", []string{target})
	if err != nil || !ok {
		t.Fatalf("Find = %v, %v, want found", ok, err)
	}
	if path != dir {
		t.Errorf("Find path = %q, want %q", path, dir)
	}
}

func TestRemove(t *testing.T) {
	source := filepath.Join(t.TempDir(), "package")
	writePackage(t, source, "demo")
	target := filepath.Join(t.TempDir(), "capabilities")
	pkg, err := Prepare(source, "")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer pkg.Cleanup()
	installed, err := pkg.Install(target)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	removed, err := Remove("demo", []string{target})
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if removed != installed {
		t.Errorf("removed = %q, want %q", removed, installed)
	}
	if _, err := os.Stat(installed); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the capability directory still exists: %v", err)
	}
	if _, err := Remove("demo", []string{target}); !errors.Is(err, herrors.UnknownCapability) {
		t.Errorf("second Remove error = %v, want unknown_capability", err)
	}
}

func TestRemoveRefusesMismatchedManifest(t *testing.T) {
	target := filepath.Join(t.TempDir(), "capabilities")
	dir := filepath.Join(target, "demo")
	writePackage(t, dir, "other")

	if _, err := Locate("demo", []string{target}); !errors.Is(err, herrors.UnknownCapability) {
		t.Errorf("Locate error = %v, want unknown_capability", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("Locate must not remove anything: %v", err)
	}
}

func TestInvalidRemoveID(t *testing.T) {
	if _, err := Locate("../demo", []string{t.TempDir()}); !errors.Is(err, herrors.InvalidArguments) {
		t.Errorf("Locate error = %v, want invalid_arguments", err)
	}
}

// requireGit returns the git executable path or skips the test.
func requireGit(t *testing.T) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	return git
}

package capinstall

import (
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	herrors "github.com/achmadss/hanse/internal/errors"
)

// isGitSource reports whether source is a git URL, a local git repository,
// or a path that ends in .git. Everything else is a local directory to copy.
func isGitSource(source string) bool {
	if strings.HasPrefix(source, "git@") {
		return true
	}
	if parsed, err := url.Parse(source); err == nil {
		switch strings.ToLower(parsed.Scheme) {
		case "git", "ssh", "http", "https", "file":
			return true
		}
	}
	local := filepath.FromSlash(source)
	if _, err := os.Stat(filepath.Join(local, ".git")); err == nil {
		return true
	}
	trimmed := strings.TrimRight(source, `/\`)
	return strings.HasSuffix(trimmed, ".git")
}

// cloneGit clones source into dir, checks out ref when it is set, resolves
// the full commit SHA, and removes the .git directory so it is never part of
// the installed package. Git runs as an argument list, never through a
// shell (docs/architecture.md section 15).
func cloneGit(source, ref, dir string) (string, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", herrors.New(herrors.Internal,
			"installing from a git source needs the git program: "+err.Error(), nil)
	}
	if err := runGit("", "clone", "--quiet", "--no-checkout", source, dir); err != nil {
		return "", err
	}
	args := []string{"-C", dir, "checkout", "--quiet", "--detach"}
	if ref == "" {
		if err := runGit("", append(args, "HEAD")...); err != nil {
			return "", err
		}
	} else if err := runGit("", append(args, ref)...); err != nil {
		// A clone creates a local branch only for the default branch;
		// every other branch arrives as a remote-tracking ref.
		if err := runGit("", append(args, "origin/"+ref)...); err != nil {
			return "", err
		}
	}
	out, err := gitOutput("-C", dir, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(out)
	if commit == "" {
		return "", herrors.New(herrors.Internal,
			fmt.Sprintf("%s: git did not report a commit", source), nil)
	}
	if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
		return "", herrors.New(herrors.Internal,
			fmt.Sprintf("%s: cannot remove the .git directory: %s", dir, err), nil)
	}
	return commit, nil
}

// runGit runs one git command with an argument list.
func runGit(dir string, args ...string) error {
	cmd := gitCommand(args...)
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		return herrors.New(herrors.Internal, fmt.Sprintf("git %s failed: %s", args[0], err), nil)
	}
	return nil
}

// gitOutput runs one git command and returns its standard output.
func gitOutput(args ...string) (string, error) {
	cmd := gitCommand(args...)
	out, err := cmd.Output()
	if err != nil {
		return "", herrors.New(herrors.Internal, fmt.Sprintf("git %s failed: %s", args[0], err), nil)
	}
	return string(out), nil
}

// gitCommand builds a git command that never prompts for credentials, so a
// private repository fails instead of waiting for input.
func gitCommand(args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
	return cmd
}

// copyDir copies the source tree to dst. It never follows a symbolic link:
// a link inside a package is refused, because a capability directory must
// hold regular files only (docs/architecture.md section 7.1) and a link
// could point outside the package.
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return herrors.New(herrors.Internal, fmt.Sprintf("%s: %s", path, err), nil)
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return herrors.New(herrors.Internal, fmt.Sprintf("%s: %s", path, err), nil)
		}
		target := dst
		if rel != "." {
			target = filepath.Join(dst, rel)
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return herrors.New(herrors.InvalidArguments,
				fmt.Sprintf("%s: the capability package contains a symbolic link", path), nil)
		}
		if entry.IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return herrors.New(herrors.Internal, fmt.Sprintf("%s: %s", target, err), nil)
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return herrors.New(herrors.InvalidArguments,
				fmt.Sprintf("%s: the capability package contains a file that is not a regular file", path), nil)
		}
		info, err := entry.Info()
		if err != nil {
			return herrors.New(herrors.Internal, fmt.Sprintf("%s: %s", path, err), nil)
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

// copyFile copies one regular file and keeps its permission bits, so an
// executable handler stays executable.
func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return herrors.New(herrors.Internal, fmt.Sprintf("%s: %s", src, err), nil)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return herrors.New(herrors.Internal, fmt.Sprintf("%s: %s", dst, err), nil)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return herrors.New(herrors.Internal, fmt.Sprintf("%s: %s", dst, err), nil)
	}
	if err := out.Close(); err != nil {
		return herrors.New(herrors.Internal, fmt.Sprintf("%s: %s", dst, err), nil)
	}
	return nil
}

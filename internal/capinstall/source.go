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
	"unicode"

	"github.com/achmadss/hanse/internal/config"
	herrors "github.com/achmadss/hanse/internal/errors"
)

// sourceKind is how Prepare reads a source: as a git repository or as a
// plain directory to copy.
type sourceKind int

const (
	sourceCopy sourceKind = iota
	sourceGit
)

// sourceSpec is a classified install source. Location is the value handed to
// git or to the filesystem; for a git source it is the URL or the absolute
// local path, and it never starts with "-".
type sourceSpec struct {
	kind     sourceKind
	location string
}

// classifySource decides how to read a source before any git command runs.
// It accepts https, ssh, scp-style, and file sources, http only on a loopback
// host, and an existing local directory. Everything else is refused with
// invalid_arguments: an unknown scheme such as git, ext, or fd, a non-existent
// path, or a file that is not a directory.
func classifySource(source string) (sourceSpec, error) {
	if strings.TrimSpace(source) == "" {
		return sourceSpec{}, invalidSource("the install source is empty")
	}
	if strings.HasPrefix(source, "-") {
		return sourceSpec{}, invalidSource(
			fmt.Sprintf("the install source %q must not start with \"-\"", source))
	}
	if scpLike(source) {
		return sourceSpec{kind: sourceGit, location: source}, nil
	}
	if scheme, ok := sourceScheme(source); ok {
		return classifyURL(source, scheme)
	}

	abs, err := filepath.Abs(source)
	if err != nil {
		return sourceSpec{}, invalidSource(fmt.Sprintf("cannot resolve %q: %s", source, err))
	}
	info, err := os.Stat(abs)
	if err != nil {
		return sourceSpec{}, invalidSource(fmt.Sprintf("%s: %s", abs, err))
	}
	if !info.IsDir() {
		return sourceSpec{}, invalidSource(fmt.Sprintf("%s is not a directory", abs))
	}
	if _, err := os.Stat(filepath.Join(abs, ".git")); err == nil {
		return sourceSpec{kind: sourceGit, location: abs}, nil
	}
	return sourceSpec{kind: sourceCopy, location: abs}, nil
}

// classifyURL classifies a source with a URL scheme.
func classifyURL(source, scheme string) (sourceSpec, error) {
	switch scheme {
	case "https", "ssh":
		parsed, err := url.Parse(source)
		if err != nil || parsed.Host == "" {
			return sourceSpec{}, invalidSource(fmt.Sprintf("%q is not a valid %s URL", source, scheme))
		}
		return sourceSpec{kind: sourceGit, location: source}, nil
	case "file":
		return sourceSpec{kind: sourceGit, location: source}, nil
	case "http":
		// Cleartext http cannot authenticate the server, so anyone on the
		// path could swap the package before the commit is pinned. Keep it
		// for local testing only, matching the https-unless-loopback rule.
		parsed, err := url.Parse(source)
		if err != nil || parsed.Hostname() == "" {
			return sourceSpec{}, invalidSource(fmt.Sprintf("%q is not a valid http URL", source))
		}
		if !config.IsLoopbackHost(parsed.Hostname()) {
			return sourceSpec{}, invalidSource(
				fmt.Sprintf("http source %q is not allowed: use https, or http only on a loopback host", source))
		}
		return sourceSpec{kind: sourceGit, location: source}, nil
	case "git":
		return sourceSpec{}, invalidSource(
			fmt.Sprintf("git source %q is not allowed: the git:// transport is unauthenticated; use https or ssh", source))
	default:
		return sourceSpec{}, invalidSource(
			fmt.Sprintf("source %q uses unsupported scheme %q", source, scheme))
	}
}

// sourceScheme returns the lowercase scheme of a URL source. A Windows drive
// path such as C:\packages\demo has no "://", so it stays a local path.
func sourceScheme(source string) (string, bool) {
	for _, separator := range []string{"://", "::"} {
		i := strings.Index(source, separator)
		if i > 0 && validScheme(source[:i]) {
			return strings.ToLower(source[:i]), true
		}
	}
	return "", false
}

// validScheme reports whether s is a URL scheme: a letter followed by
// letters, digits, "+", "-", or ".".
func validScheme(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && (r >= '0' && r <= '9' || r == '+' || r == '-' || r == '.'):
		default:
			return false
		}
	}
	return true
}

// scpLike reports whether source is the scp-style git form,
// user@host:path. Git reads this form as ssh.
func scpLike(source string) bool {
	if strings.Contains(source, "://") || strings.ContainsAny(source, " \t\r\n") {
		return false
	}
	at := strings.Index(source, "@")
	if at <= 0 {
		return false
	}
	rest := source[at+1:]
	colon := strings.Index(rest, ":")
	if colon <= 0 || colon == len(rest)-1 {
		return false
	}
	return !strings.Contains(rest[:colon], "/")
}

// invalidSource builds the invalid_arguments error of a refused source.
func invalidSource(message string) error {
	return herrors.New(herrors.InvalidArguments, message, nil)
}

// validateRef refuses a git ref that git could read as an option or that
// cannot be a branch, tag, or commit name.
func validateRef(ref string) error {
	if ref == "" {
		return nil
	}
	if strings.HasPrefix(ref, "-") {
		return herrors.New(herrors.InvalidArguments,
			fmt.Sprintf("the git ref %q must not start with \"-\"", ref), nil)
	}
	for _, r := range ref {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return herrors.New(herrors.InvalidArguments,
				fmt.Sprintf("the git ref %q must not contain whitespace or control characters", ref), nil)
		}
	}
	return nil
}

// cloneGit clones source into dir, checks out ref when it is set, resolves
// the full commit SHA, and removes the .git directory so it is never part of
// the installed package. Git runs as an argument list, never through a
// shell, and every positional argument follows "--" so a source that looks
// like an option stays a repository path (docs/architecture.md section 15).
func cloneGit(source, ref, dir string) (string, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", herrors.New(herrors.Internal,
			"installing from a git source needs the git program: "+err.Error(), nil)
	}
	if err := runGit("", "clone", "--quiet", "--no-checkout", "--", source, dir); err != nil {
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

// gitEnvKeys are the variables gitCommand sets for every call. They are
// removed from the inherited environment first, so a duplicate cannot shadow
// the value this process chooses.
var gitEnvKeys = map[string]bool{
	"GIT_ALLOW_PROTOCOL":  true,
	"GIT_CONFIG_NOSYSTEM": true,
	"GIT_TERMINAL_PROMPT": true,
	"GCM_INTERACTIVE":     true,
}

// gitCommand builds a git command that never prompts for credentials and
// that restricts the transports it may use. GIT_ALLOW_PROTOCOL covers
// submodules and redirects too, so a repository cannot reach ext:: or the
// unauthenticated git:// transport.
func gitCommand(args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	env := make([]string, 0, len(os.Environ())+len(gitEnvKeys))
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if gitEnvKeys[name] {
			continue
		}
		env = append(env, entry)
	}
	env = append(env,
		"GIT_ALLOW_PROTOCOL=https:ssh:file",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=never",
	)
	cmd.Env = env
	return cmd
}

// copyDir copies the source tree to dst. It never follows a symbolic link:
// a link inside a package is refused, because a capability directory must
// hold regular files only (docs/architecture.md section 7.1) and a link
// could point outside the package.
func copyDir(src, dst string) error {
	// A local source may itself be a symbolic link to the package. Resolve
	// only the root, so the tree walk still sees and refuses every link
	// inside the package.
	resolved, err := filepath.EvalSymlinks(src)
	if err != nil {
		return herrors.New(herrors.Internal, fmt.Sprintf("%s: %s", src, err), nil)
	}
	return filepath.WalkDir(resolved, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return herrors.New(herrors.Internal, fmt.Sprintf("%s: %s", path, err), nil)
		}
		rel, err := filepath.Rel(resolved, path)
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

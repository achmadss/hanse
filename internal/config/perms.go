package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// unsafeGroupOther is the permission mask that makes a file or directory
// readable by users other than its owner: group-read (0o0040) or
// other-read (0o0004).
const unsafeGroupOther = 0o0044

// permsCheckable reports whether POSIX permission checks run on the
// current platform. On Windows POSIX modes do not apply, so the mode
// check is skipped (docs/configuration.md §8: on Windows the Gateway
// refuses files that Users, Authenticated Users, or Everyone can read;
// that ACL check is future work).
func permsCheckable() bool { return runtime.GOOS != "windows" }

// checkPermissions checks the permissions of the credentials directory,
// the secret files inside it, and the TLS key, per docs/configuration.md
// §8. It never reads or logs a secret's contents: every check inspects
// file metadata only, and every message names a path only.
func checkPermissions(g *GatewayConfig, problems *Problems) {
	const file = "gateway.yaml"
	if !permsCheckable() {
		// Skipped on Windows: POSIX mode bits and ACL ownership do not
		// map to each other, so there is nothing to check here yet.
		return
	}
	// An empty (already reported) credentials directory is not checked
	// again, so "is required" stays the only problem about it.
	if g.Paths.CredentialsDir != "" {
		checkCredentialsDir(g.Paths.CredentialsDir, file, problems)
	}
	if g.TLS != nil && g.TLS.KeyFile != "" {
		if info, err := os.Stat(g.TLS.KeyFile); err != nil {
			*problems = append(*problems, Problem{file, "tls.key_file",
				"cannot read the file: " + err.Error()})
		} else {
			checkMode(info, g.TLS.KeyFile, problems, file, "tls.key_file", "")
		}
	}
}

// checkCredentialsDir checks the directory, and every file and
// subdirectory inside it, so an unsafe secret file fails at startup even
// when no operation is waiting for that secret yet. A secret file lives
// in credentials_dir/<capability id>/<secret name> (docs/configuration.md
// §8.1).
func checkCredentialsDir(dir, file string, problems *Problems) {
	info, err := os.Stat(dir)
	if err != nil {
		*problems = append(*problems, Problem{file, "paths.credentials_dir",
			"cannot read the directory: " + err.Error()})
		return
	}
	if checkMode(info, dir, problems, file, "paths.credentials_dir", "") && !info.IsDir() {
		*problems = append(*problems, Problem{file, "paths.credentials_dir",
			"is not a directory"})
	}
	walkErr := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			// The file that goes unread is on the same discovery path a
			// handler uses at run time; report what the walk saw.
			*problems = append(*problems, Problem{file, "paths.credentials_dir",
				"cannot read " + path + ": " + err.Error()})
			return nil
		}
		if path == dir {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			*problems = append(*problems, Problem{file, "paths.credentials_dir",
				"cannot read " + path + ": " + err.Error()})
			return nil
		}
		checkMode(info, path, problems, file, "paths.credentials_dir", path[len(dir):])
		return nil
	})
	if walkErr != nil {
		*problems = append(*problems, Problem{file, "paths.credentials_dir",
			"cannot walk the directory: " + walkErr.Error()})
	}
}

// checkMode reports whether the mode was checked and is safe. An unsafe
// problem is already appended when it returns false.
func checkMode(info os.FileInfo, path string, problems *Problems, file, field, extraPath string) bool {
	mode := info.Mode().Perm()
	if mode&unsafeGroupOther == 0 {
		return true
	}
	name := field
	if extraPath != "" {
		name = field + extraPath
	}
	*problems = append(*problems, Problem{file, name,
		path + " is readable by users other than its owner: remove the group and other read bits (mode " + mode.String() + ")"})
	return false
}

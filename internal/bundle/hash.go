package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	herrors "github.com/achmadss/hanse/internal/errors"
)

// Files lists every regular file of a capability directory, sorted by
// relative path with `/` as the separator, as agent-protocol.md section
// 5.1 requires. It rejects symbolic links and anything that is not a
// regular file; a bundle must contain no links.
func Files(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("%s: %s", errPath(path), err)
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s: capability directory contains a symbolic link", errPath(path))
		}
		if !d.IsDir() {
			if !d.Type().IsRegular() {
				return fmt.Errorf("%s: capability directory contains a file that is not a regular file", errPath(path))
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return fmt.Errorf("%s: %s", errPath(path), relErr)
			}
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return nil, herrors.New(herrors.Internal, err.Error(), nil)
	}
	sort.Slice(files, func(i, j int) bool {
		return strings.Compare(files[i], files[j]) < 0
	})
	return files, nil
}

// Hash computes the bundle hash of a capability directory exactly as
// agent-protocol.md section 5.1 defines, for the paths Files listed. On
// Windows every file counts as mode 644.
func Hash(root string, files []string) (string, error) {
	modes := make(map[string]string, len(files))
	for _, rel := range files {
		path := filepath.FromSlash(filepath.Join(root, rel))
		mode, err := fileMode(path)
		if err != nil {
			return "", err
		}
		modes[rel] = mode
	}
	return hashFiles(root, files, modes)
}

// HashDir lists and hashes one capability or cache directory.
func HashDir(root string) (string, error) {
	files, err := Files(root)
	if err != nil {
		return "", err
	}
	return Hash(root, files)
}

// HashWithModes hashes one unpacked bundle from the entry modes recorded
// beside it, instead of the modes the local file system reports. The agent
// uses the modes an archive recorded, so a bundle built on Linux or macOS
// verifies on Windows too (agent-protocol.md sections 5.1 and 5.2).
//
// modes holds one `755` or `644` value per file, keyed by the relative
// path Files reports. Every file of the directory must appear in modes, and
// every entry of modes must be a file of the directory.
func HashWithModes(root string, modes map[string]string) (string, error) {
	files, err := Files(root)
	if err != nil {
		return "", err
	}
	if len(files) != len(modes) {
		return "", herrors.New(herrors.HandlerIntegrity,
			fmt.Sprintf("the bundle directory holds %d files, but %d entry modes are recorded", len(files), len(modes)), nil)
	}
	for rel, mode := range modes {
		if err := checkMode(rel, mode); err != nil {
			return "", err
		}
	}
	return hashFiles(root, files, modes)
}

// hashFiles writes the bundle hash lines of files with the given modes.
func hashFiles(root string, files []string, modes map[string]string) (string, error) {
	lines := sha256.New()
	for _, rel := range files {
		mode, ok := modes[rel]
		if !ok {
			return "", herrors.New(herrors.HandlerIntegrity,
				fmt.Sprintf("no entry mode is recorded for %q", rel), nil)
		}
		path := filepath.FromSlash(filepath.Join(root, rel))
		contentHash := sha256.New()
		if err := hashFile(path, contentHash); err != nil {
			return "", err
		}
		if _, err := fmt.Fprintf(lines, "%s\x00%s\x00%s\n", rel, mode, hex.EncodeToString(contentHash.Sum(nil))); err != nil {
			return "", herrors.New(herrors.Internal,
				fmt.Sprintf("%s: hash write failed: %s", errPath(path), err), nil)
		}
	}
	return "sha256:" + hex.EncodeToString(lines.Sum(nil)), nil
}

// checkMode rejects an entry mode other than `755` and `644`.
func checkMode(rel, mode string) error {
	if mode != "755" && mode != "644" {
		return herrors.New(herrors.HandlerIntegrity,
			fmt.Sprintf("%q has entry mode %q, which is not 755 or 644", rel, mode), nil)
	}
	return nil
}

// EntryMode renders the bundle entry mode of one file, from the permission
// bits of its content: `755` when any execute bit is set, `644` otherwise
// (agent-protocol.md section 5.1). The agent reads the mode of a downloaded
// file from the archive header, and the Gateway reads it from the file.
func EntryMode(perm fs.FileMode) string {
	if perm&0o111 != 0 {
		return "755"
	}
	return "644"
}

// hashFile feeds one file's content into h.
func hashFile(path string, h io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("%s: %s", errPath(path), err), nil)
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(h, f); err != nil {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("%s: %s", errPath(path), err), nil)
	}
	return nil
}

// fileMode renders the bundle entry mode of one file on disk. On Windows
// the file permission bits hold no execute bit, so every file counts as
// 644.
func fileMode(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", herrors.New(herrors.Internal,
			fmt.Sprintf("%s: %s", errPath(path), err), nil)
	}
	if runtime.GOOS == "windows" {
		return "644", nil
	}
	return EntryMode(fi.Mode().Perm()), nil
}

// errPath renders a filesystem path for an error message.
func errPath(p string) string { return fmt.Sprintf("%q", filepath.ToSlash(p)) }

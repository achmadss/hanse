package bundle

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// DefaultMaxUnpackedBytes is the largest total size of an unpacked
// bundle. A capability holds handler files and a manifest, so this cap is
// far above any real bundle and still bounds one download.
const DefaultMaxUnpackedBytes int64 = 64 << 20

// ErrUnsafeArchive reports an archive that the agent refuses to unpack:
// an absolute path, a `..` path, a link, a device or other special file,
// or an archive over the size limit.
var ErrUnsafeArchive = errors.New("the bundle archive is not safe to unpack")

// Entry is one regular file that Unpack wrote: its path relative to the
// destination, with `/` as the separator, and the bundle entry mode the
// archive recorded for it (`755` or `644`).
//
// The agent verifies a downloaded bundle with these modes, not with the
// modes its own file system reports, so a bundle built on Linux or macOS
// verifies on Windows too (docs/agent-protocol.md sections 5.1 and 5.2).
type Entry struct {
	Path string
	Mode string
}

// Unpack reads a gzip-compressed tar archive into dest, which must be an
// empty directory the caller created. It rejects absolute paths, paths
// that contain `..`, links, device files, and an archive whose unpacked
// size exceeds maxBytes (docs/agent-protocol.md section 5.2). It returns
// the entry modes the archive recorded for the files it wrote.
func Unpack(gz io.Reader, dest string, maxBytes int64) ([]Entry, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxUnpackedBytes
	}
	reader, err := gzip.NewReader(gz)
	if err != nil {
		return nil, fmt.Errorf("%w: it is not a gzip archive: %v", ErrUnsafeArchive, err)
	}
	defer func() { _ = reader.Close() }()
	archive := tar.NewReader(reader)
	var total int64
	var entries []Entry
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return entries, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnsafeArchive, err)
		}
		rel, err := safeName(header.Name)
		if err != nil {
			return nil, err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(filepath.Join(dest, rel), 0o755); err != nil {
				return nil, err
			}
		case tar.TypeReg:
			if header.Size < 0 {
				return nil, fmt.Errorf("%w: %q has a negative size", ErrUnsafeArchive, header.Name)
			}
			total += header.Size
			if total > maxBytes {
				return nil, fmt.Errorf("%w: it holds more than %d bytes", ErrUnsafeArchive, maxBytes)
			}
			if err := writeFile(dest, rel, archive, header); err != nil {
				return nil, err
			}
			entries = append(entries, Entry{
				Path: filepath.ToSlash(rel),
				Mode: EntryMode(os.FileMode(header.Mode)),
			})
		case tar.TypeSymlink, tar.TypeLink:
			return nil, fmt.Errorf("%w: %q is a link", ErrUnsafeArchive, header.Name)
		default:
			return nil, fmt.Errorf("%w: %q is not a regular file or directory", ErrUnsafeArchive, header.Name)
		}
	}
}

// writeFile writes one regular file of the archive to dest.
func writeFile(dest, rel string, body io.Reader, header *tar.Header) error {
	target := filepath.Join(dest, rel)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if header.Mode&0o111 != 0 {
		mode = 0o755
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if _, err := io.CopyN(file, body, header.Size); err != nil {
		return fmt.Errorf("%w: %q is truncated: %v", ErrUnsafeArchive, header.Name, err)
	}
	return nil
}

// safeName checks one archive path and returns it as a local path. It
// rejects absolute paths, a Windows drive or UNC path (so the same
// archive is refused on every OS), and any `..` component.
func safeName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: it holds an entry without a name", ErrUnsafeArchive)
	}
	slash := strings.ReplaceAll(name, `\`, "/")
	if strings.HasPrefix(slash, "/") || filepath.IsAbs(name) || windowsPathPattern.MatchString(name) {
		return "", fmt.Errorf("%w: %q is an absolute path", ErrUnsafeArchive, name)
	}
	clean := path.Clean(slash)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%w: %q leaves the bundle directory", ErrUnsafeArchive, name)
	}
	for _, part := range strings.Split(slash, "/") {
		if part == ".." {
			return "", fmt.Errorf("%w: %q leaves the bundle directory", ErrUnsafeArchive, name)
		}
	}
	return filepath.FromSlash(clean), nil
}

// windowsPathPattern matches a Windows drive path such as `C:\x` on every
// operating system.
var windowsPathPattern = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

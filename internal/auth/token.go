package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/achmadss/hanse/internal/config"
)

// tokenPrefix identifies every API token. The prefix lets operators
// recognize a token in output while the value after it is random.
const tokenPrefix = "hst_"

// tokenEntropy is the number of random bytes in one token: 32 bytes,
// base64url-encoded, come out as 43 characters.
const tokenEntropy = 32

// fileVersion is the tokens file's format version.
const fileVersion = 1

// Record is one stored token. The file never holds a plain token: Hash
// is the hex SHA-256 of "<tokenPrefix><random bytes>" (architecture.md
// §15).
type Record struct {
	// Name is the token's name. It is a valid node-style ID, so the
	// audit log can record who made a request.
	Name string `json:"name"`
	// Hash is the hex SHA-256 of the plain token.
	Hash string `json:"hash"`
	// Allow is this token's allow list of capabilities and operations:
	// "<capability>.<operation>" or "<capability>" entries. An empty
	// list allows everything.
	Allow []string `json:"allow,omitempty"`
	// Created is when the token was added, RFC 3339, UTC.
	Created string `json:"created"`
}

// file is the tokens file's JSON shape.
type file struct {
	Version int      `json:"version"`
	Tokens  []Record `json:"tokens"`
}

// Store holds the tokens read from, or about to be written to, the
// tokens file named in gateway.yaml. A running Gateway reads a Store
// concurrently, so every read and write takes the mutex.
type Store struct {
	mu     sync.RWMutex
	path   string
	tokens []Record
}

// ErrNotFound is the error Revoke and Add report for an unknown or
// duplicated name. It never names a token value.
var ErrNotFound = stderrors.New("token name is unknown")

// LoadStore reads the tokens file at path and returns the store. A file
// that does not exist yields an empty store, so the first
// `hanse-gateway token add` can run before the file exists. Any other
// problem is a hard error: a Gateway must not serve with tokens it
// cannot check. On Windows, POSIX mode checks are skipped (see below);
// everywhere else a file readable or writable by users other than its
// owner is refused.
func LoadStore(path string) (*Store, error) {
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if stderrors.Is(err, fs.ErrNotExist) {
			return s, nil
		}
		return nil, fmt.Errorf("cannot read the tokens file: %w", err)
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("the tokens file is not valid JSON: %w", err)
	}
	if f.Version != fileVersion {
		return nil, fmt.Errorf("the tokens file version must be %d, got %d", fileVersion, f.Version)
	}
	seen := make(map[string]bool, len(f.Tokens))
	for _, r := range f.Tokens {
		if r.Name == "" || !config.ValidID(r.Name) {
			return nil, fmt.Errorf("the tokens file holds an invalid token name %q", r.Name)
		}
		if _, err := hex.DecodeString(r.Hash); err != nil {
			return nil, fmt.Errorf("the tokens file holds an invalid hash for token %q", r.Name)
		}
		for _, a := range r.Allow {
			if !ValidAllowEntry(a) {
				return nil, fmt.Errorf("the tokens file holds an invalid allow entry %q for token %q", a, r.Name)
			}
		}
		if seen[r.Name] {
			return nil, fmt.Errorf("the tokens file holds the token name %q twice", r.Name)
		}
		seen[r.Name] = true
	}
	if err := checkTokensFileMode(path); err != nil {
		return nil, err
	}
	s.tokens = f.Tokens
	return s, nil
}

// Path returns the tokens file path of this store.
func (s *Store) Path() string { return s.path }

// Add generates a new fresh random token for name, persists its hash,
// and returns the plain token, which the caller prints once. If the
// name is already in use it reports ErrNotFound and nothing is written.
func (s *Store) Add(name string, allow []string) (string, error) {
	if !config.ValidID(name) {
		return "", fmt.Errorf("token name %q: must be lowercase letters, digits, \"_\" and \"-\", starting with a letter, at most 63 characters", name)
	}
	for _, a := range allow {
		if !ValidAllowEntry(a) {
			return "", fmt.Errorf("allow entry %q: must be <capability> or <capability>.<operation> with valid IDs", a)
		}
	}

	plain, hash, err := Generate()
	if err != nil {
		return "", fmt.Errorf("cannot generate a token: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.tokens {
		if r.Name == name {
			return "", ErrNotFound
		}
	}
	rec := Record{Name: name, Hash: hash, Allow: append([]string(nil), allow...), Created: time.Now().UTC().Format(time.RFC3339)}
	tokens := append(append([]Record(nil), s.tokens...), rec)
	if err := writeTokens(s.path, tokens); err != nil {
		// The plain token is never returned to the caller when its
		// hash could not be stored, so it can never leak.
		return "", fmt.Errorf("cannot write the tokens file: %w", err)
	}
	s.tokens = tokens
	return plain, nil
}

// Revoke removes the named token and persists the change. An unknown
// name reports ErrNotFound and never writes the file.
func (s *Store) Revoke(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := -1
	for i, r := range s.tokens {
		if r.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ErrNotFound
	}
	tokens := append([]Record(nil), s.tokens[:idx]...)
	tokens = append(tokens, s.tokens[idx+1:]...)
	if err := writeTokens(s.path, tokens); err != nil {
		return fmt.Errorf("cannot write the tokens file: %w", err)
	}
	s.tokens = tokens
	return nil
}

// List returns a copy of the stored tokens, sorted by name.
func (s *Store) List() []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := append([]Record(nil), s.tokens...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// writeTokens atomically writes the tokens file: the bytes go through a
// temporary file in the same directory, created with 0600 so that no
// umask can loosen it, and then renamed into place. The hash values are
// the only token material on disk.
func writeTokens(path string, tokens []Record) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(file{Version: fileVersion, Tokens: tokens}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, ".tokens-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// checkTokensFileMode refuses a tokens file that any user other than
// its owner can read or write. On Windows, POSIX mode bits do not
// exist; the ACL rule from docs/configuration.md §8 is future work, as
// in internal/config (permsCheckable).
func checkTokensFileMode(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("cannot read the tokens file: %w", err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return fmt.Errorf("the tokens file %s is readable or writable by users other than its owner (mode %s)", path, mode)
	}
	return nil
}

// Generate returns a fresh plain token and its hash. The plain token is
// `<tokenPrefix><43 base64url characters>`; the hash is the hex SHA-256
// of the plain token.
func Generate() (plain, hash string, err error) {
	b := make([]byte, tokenEntropy)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	plain = tokenPrefix + base64URL(b)
	hash = Hash(plain)
	return plain, hash, nil
}

// Hash computes the hex SHA-256 of the plain token.
func Hash(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// base64URL encodes b without padding, so a generated token is stable
// in length and free of characters that need quoting.
func base64URL(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// hashesMatch compares a stored hash and a presented hash in constant
// time. The presented value comes from this process's own Hash, so a
// decode failure can only mean a corrupted store; it compares as
// unequal either way.
func hashesMatch(stored, presented string) bool {
	sb, err := hex.DecodeString(stored)
	if err != nil {
		sb = nil
	}
	pb, err := hex.DecodeString(presented)
	if err != nil {
		pb = []byte{0}
	}
	return subtle.ConstantTimeCompare(sb, pb) == 1
}

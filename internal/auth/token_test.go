package auth

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// storeAt returns the path of a tokens file that is not there yet.
func storePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "tokens.json")
}

func TestAddRoundTrip(t *testing.T) {
	p := storePath(t)
	s, err := LoadStore(p)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	plain, err := s.Add("ci-bot", []string{"pc.up", "pc"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !strings.HasPrefix(plain, tokenPrefix) {
		t.Errorf("token %q misses the %q prefix", plain, tokenPrefix)
	}

	// Reload from disk and authenticate with the printed token.
	s2, err := LoadStore(p)
	if err != nil {
		t.Fatalf("LoadStore after add: %v", err)
	}
	id, err := s2.Authenticate(plain)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.Name != "ci-bot" {
		t.Errorf("identity name = %q, want %q", id.Name, "ci-bot")
	}
	if err := id.Authorize("pc", "up"); err != nil {
		t.Errorf("Authorize(pc.up) with entries [pc.up, pc]: %v", err)
	}
	allow := id.Allow()
	if len(allow) != 2 || allow[0] != "pc.up" || allow[1] != "pc" {
		t.Errorf("Allow() = %v, want the two entries", allow)
	}
}

func TestTokensFileStoresHashesOnly(t *testing.T) {
	p := storePath(t)
	s, _ := LoadStore(p)
	plain, err := s.Add("ci", nil)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read the tokens file: %v", err)
	}
	if strings.Contains(string(data), plain) {
		t.Errorf("the tokens file holds the plain token %q", plain)
	}
	if strings.Contains(string(data), plain) {
		t.Errorf("the tokens file holds the plain token %q", plain)
	}
	if strings.Contains(string(data), tokenPrefix) {
		t.Errorf("the tokens file embeds any form of a plain token")
	}
	hash := Hash(plain)
	if !strings.Contains(string(data), hash) {
		t.Errorf("the tokens file misses the hash %q", hash)
	}
}

func TestTokensFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits do not apply on Windows")
	}
	p := storePath(t)
	s, _ := LoadStore(p)
	if _, err := s.Add("ci", nil); err != nil {
		t.Fatalf("Add: %v", err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the tokens file mode = %s, want 0600", mode)
	}
}

func TestLoadStoreRefusesOpenFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode checks are skipped on Windows")
	}
	p := storePath(t)
	s, _ := LoadStore(p)
	if _, err := s.Add("ci", nil); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := LoadStore(p); err == nil {
		t.Errorf("LoadStore of a group/other-readable tokens file: want an error")
	} else if msg := err.Error(); !strings.Contains(msg, "other than its owner") {
		t.Errorf("error = %q, want the unsafe-permissions message", msg)
	}
}

func TestAddDuplicateName(t *testing.T) {
	p := storePath(t)
	s, _ := LoadStore(p)
	if _, err := s.Add("ci", nil); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := s.Add("ci", nil); err != ErrExists {
		t.Errorf("second Add of the same name: err = %v, want ErrExists", err)
	}
	records := s.List()
	if len(records) != 1 {
		t.Errorf("List() = %d records, want the first one only", len(records))
	}
}

func TestAddInvalidNameAndAllow(t *testing.T) {
	p := storePath(t)
	s, _ := LoadStore(p)
	for _, name := range []string{"", "Bot", "a b", "ope_fully_invalid_beyond_63_" + strings.Repeat("x", 50)} {
		if _, err := s.Add(name, nil); err == nil {
			t.Errorf("Add(%q): want an error", name)
		}
	}
	if _, err := s.Add("ci", []string{"pc.up", "ALL"}); err == nil {
		t.Errorf("Add with the invalid allow entry %q: want an error", "ALL")
	}
	if _, statErr := os.Stat(p); statErr == nil {
		t.Errorf("a failed Add must not create the tokens file")
	}
}

func TestValidAllowEntry(t *testing.T) {
	valid := []string{"pc.up", "pc", "pc-wake.up_now", "a1"}
	invalid := []string{"", "PC.up", "pc.UP", "..", ".up", "pc.", "pc.up.extra"}
	for _, e := range valid {
		if !ValidAllowEntry(e) {
			t.Errorf("ValidAllowEntry(%q) = false, want true", e)
		}
	}
	for _, e := range invalid {
		if ValidAllowEntry(e) {
			t.Errorf("ValidAllowEntry(%q) = true, want false", e)
		}
	}
}

func TestRevoke(t *testing.T) {
	p := storePath(t)
	s, _ := LoadStore(p)
	plain, err := s.Add("ci", nil)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Revoke("ci"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if err := s.Revoke("ci"); err != ErrNotFound {
		t.Errorf("second Revoke: err = %v, want ErrNotFound", err)
	}
	// A revoked token authenticates against nothing.
	after, _ := LoadStore(p)
	if _, err := after.Authenticate(plain); err == nil {
		t.Errorf("Authenticate with a revoked token: want an error")
	}
	if lv := after.List(); len(lv) != 0 {
		t.Errorf("List() after revoke = %d records, want 0", len(lv))
	}
}

// TestAddDuplicateNameSentinels shows the two sentinels: Add reports
// ErrExists, Revoke reports ErrNotFound, for one and the same name.
func TestAddDuplicateNameSentinels(t *testing.T) {
	p := storePath(t)
	s, _ := LoadStore(p)
	if _, err := s.Add("ci", nil); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := s.Add("ci", nil); err != ErrExists {
		t.Errorf("second Add of the same name: err = %v, want ErrExists", err)
	}
	if err := s.Revoke("ghost"); err != ErrNotFound {
		t.Errorf("Revoke of an unknown name: err = %v, want ErrNotFound", err)
	}
	records := s.List()
	if len(records) != 1 {
		t.Errorf("List() = %d records, want the first one only", len(records))
	}
}

// TestStorePicksUpRevocationWithoutRestart is the issue #13 behavior:
// a store acting as the Gateway reloads the tokens file when another
// process changed it, so the revoked token stops authenticating and a
// newly added token starts. The detection runs on file size and mtime,
// so it needs no clock cooperation: every test write changes the size
// or the mtime.
func TestStorePicksUpRevocationWithoutRestart(t *testing.T) {
	p := storePath(t)
	gatewayStore, _ := LoadStore(p)
	cliStore, _ := LoadStore(p)

	tokenA, err := cliStore.Add("ci", nil)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// The Gateway store had loaded an empty file, and the file now
	// exists: size detection reloads it and the token is accepted.
	id, err := gatewayStore.Authenticate(tokenA)
	if err != nil {
		t.Fatalf("Authenticate after add: %v", err)
	}
	if id.Name != "ci" {
		t.Errorf("identity name = %q, want ci", id.Name)
	}

	// The CLI revokes; the Gateway store's next Authenticate picks it
	// up and refuses the token.
	if err := cliStore.Revoke("ci"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := gatewayStore.Authenticate(tokenA); err == nil {
		t.Errorf("Authenticate with a revoked token: want 401 after the reload")
	}

	// A new token reaches the same store too.
	tokenB, err := cliStore.Add("new", nil)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := gatewayStore.Authenticate(tokenB); err != nil {
		t.Errorf("Authenticate with a newly added token: %v", err)
	}
}

// TestStoreFailsClosedAfterBadFile follows the review requirement: when
// the tokens file becomes unreadable or invalid, Authenticate refuses
// every token, including one it had accepted before, until the file is
// fixed.
func TestStoreFailsClosedAfterBadFile(t *testing.T) {
	p := storePath(t)
	s, _ := LoadStore(p)
	plain, err := s.Add("ci", nil)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := s.Authenticate(plain); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	broken := []string{"{not json", `{"version": 2, "tokens": []}`}
	for i, content := range broken {
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Authenticate(plain); err == nil {
			t.Errorf("case %d: Authenticate with a broken tokens file: want 401, fail closed", i)
		}
	}

	// Fixing the file un-sticks the store.
	if err := writeTokens(p, []Record{{Name: "ci", Hash: Hash(plain)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(plain); err != nil {
		t.Errorf("Authenticate after the file was fixed: %v", err)
	}
}

func TestStoreReload(t *testing.T) {
	p := storePath(t)
	s, _ := LoadStore(p)
	plain, err := s.Add("ci", nil)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := os.WriteFile(p, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(); err == nil {
		t.Errorf("Reload with a broken file: want an error")
	}
	if err := writeTokens(p, []Record{{Name: "ci", Hash: Hash(plain)}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(); err != nil {
		t.Fatalf("Reload after the file was fixed: %v", err)
	}
	if _, err := s.Authenticate(plain); err != nil {
		t.Errorf("Authenticate after Reload: %v", err)
	}
}

func TestLoadStoreMissingFileIsEmpty(t *testing.T) {
	s, err := LoadStore(storePath(t))
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if got := len(s.List()); got != 0 {
		t.Errorf("List() = %d records, want an empty store", got)
	}
}

func TestLoadStoreRejectsBadContent(t *testing.T) {
	cases := []struct{ name, content string }{
		{"bad json", "{not json"},
		{"bad version", `{"version": 2, "tokens": []}`},
		{"bad name", `{"version": 1, "tokens": [{"name": "Bot", "hash": "00"}]}`},
		{"bad hash", `{"version": 1, "tokens": [{"name": "ci", "hash": "zz"}]}`},
		{"bad allow", `{"version": 1, "tokens": [{"name": "ci", "hash": "00", "allow": ["ALL"]}]}`},
		{"duplicate name", `{"version": 1, "tokens": [{"name": "ci", "hash": "00"}, {"name": "ci", "hash": "11"}]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "tokens.json")
			if err := os.WriteFile(p, []byte(c.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadStore(p); err == nil {
				t.Errorf("LoadStore with %s: want an error", c.name)
			}
		})
	}
}

func TestGenerateShape(t *testing.T) {
	a, ha, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	b, hb, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if a == b {
		t.Errorf("two generated tokens are equal")
	}
	if !strings.HasPrefix(a, tokenPrefix) || len(a) != len(tokenPrefix)+43 {
		t.Errorf("token %q has the wrong shape", a)
	}
	if ha != Hash(a) || hb != Hash(b) {
		t.Errorf("hash does not match its token")
	}
}

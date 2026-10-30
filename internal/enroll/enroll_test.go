package enroll_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/achmadss/hanse/internal/enroll"
	herrors "github.com/achmadss/hanse/internal/errors"
)

// clock is the injected clock of the tests.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock {
	return &clock{now: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// recorder records the node IDs a Disconnecter was called with.
type recorder struct {
	mu    sync.Mutex
	nodes []string
}

func (r *recorder) Disconnect(nodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nodes = append(r.nodes, nodeID)
}

func (r *recorder) Nodes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.nodes...)
}

// open opens a service over a fresh state directory with a fixed clock.
func open(t *testing.T, opts enroll.Options) (*enroll.Service, *clock) {
	t.Helper()
	c := newClock()
	if opts.StateDir == "" {
		opts.StateDir = t.TempDir()
	}
	if opts.Now == nil {
		opts.Now = c.Now
	}
	service, err := enroll.Open(opts)
	if err != nil {
		t.Fatalf("open enrollment store: %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return service, c
}

// issue issues a join token and fails the test on error.
func issue(t *testing.T, service *enroll.Service, nodeID string) enroll.Token {
	t.Helper()
	token, err := service.IssueJoinToken(nodeID)
	if err != nil {
		t.Fatalf("issue join token for %s: %v", nodeID, err)
	}
	return token
}

// join joins with check nil and fails the test on error.
func join(t *testing.T, service *enroll.Service, token string) enroll.Outcome {
	t.Helper()
	out, err := service.Join(token, nil)
	if err != nil {
		t.Fatalf("join with a fresh token: %v", err)
	}
	return out
}

// TestValidJoin exchanges a token for a credential, verifies the
// credential, and checks the token's default 15-minute lifetime.
func TestValidJoin(t *testing.T) {
	service, c := open(t, enroll.Options{})
	token := issue(t, service, "node-a")
	if !strings.HasPrefix(token.Plain, enroll.JoinTokenPrefix) {
		t.Errorf("join token %q misses the %q prefix", token.Plain, enroll.JoinTokenPrefix)
	}
	if got, want := token.Expires, c.Now().Add(15*time.Minute); !got.Equal(want) {
		t.Errorf("expiry = %s, want %s: the default lifetime is 15 minutes", got, want)
	}
	out := join(t, service, token.Plain)
	if out.Node != "node-a" {
		t.Errorf("joined node = %q, want node-a", out.Node)
	}
	if !strings.HasPrefix(out.Credential, enroll.CredentialPrefix) {
		t.Errorf("credential %q misses the %q prefix", out.Credential, enroll.CredentialPrefix)
	}
	node, err := service.Verify(out.Credential)
	if err != nil {
		t.Fatalf("verify a fresh credential: %v", err)
	}
	if node != "node-a" {
		t.Errorf("verified node = %q, want node-a", node)
	}
}

// TestJoinTokenIsUsedOnce proves that a second join with the same token
// fails with the documented code, while the first credential keeps working.
func TestJoinTokenIsUsedOnce(t *testing.T) {
	service, _ := open(t, enroll.Options{})
	token := issue(t, service, "node-a")
	first := join(t, service, token.Plain)
	_, err := service.Join(token.Plain, nil)
	if !errors.Is(err, herrors.JoinTokenInvalid) {
		t.Fatalf("second join error = %v, want join_token_invalid", err)
	}
	if node, err := service.Verify(first.Credential); err != nil || node != "node-a" {
		t.Errorf("verify after the reused token = %q, %v, want node-a, nil", node, err)
	}
}

// TestJoinTokenExpires proves that the injected clock makes a token stop
// working after its lifetime, and that it still works just before.
func TestJoinTokenExpires(t *testing.T) {
	service, c := open(t, enroll.Options{JoinTokenTTL: 15 * time.Minute})
	token := issue(t, service, "node-a")
	c.Advance(14*time.Minute + 59*time.Second)
	out := join(t, service, token.Plain)

	expired := issue(t, service, "node-b")
	c.Advance(15 * time.Minute)
	_, err := service.Join(expired.Plain, nil)
	if !errors.Is(err, herrors.JoinTokenInvalid) {
		t.Fatalf("expired join error = %v, want join_token_invalid", err)
	}
	// A credential outlives the join token that created it, so it still
	// verifies after the token expired.
	if node, err := service.Verify(out.Credential); err != nil || node != "node-a" {
		t.Errorf("verify after the token expired = %q, %v, want node-a, nil", node, err)
	}
}

// TestRevokedCredential proves that a revoke invalidates the credential and
// asks for the node's connection to be closed.
func TestRevokedCredential(t *testing.T) {
	disconnect := &recorder{}
	service, _ := open(t, enroll.Options{Disconnect: disconnect})
	out := join(t, service, issue(t, service, "node-a").Plain)
	if err := service.Revoke("node-a"); err != nil {
		t.Fatalf("revoke node-a: %v", err)
	}
	if _, err := service.Verify(out.Credential); !errors.Is(err, herrors.AuthenticationFailed) {
		t.Fatalf("verify a revoked credential = %v, want authentication_failed", err)
	}
	if got := disconnect.Nodes(); len(got) != 1 || got[0] != "node-a" {
		t.Errorf("disconnect calls = %v, want [node-a]", got)
	}
	// Revoking twice is safe and still asks for the connection to close.
	if err := service.Revoke("node-a"); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	if got := disconnect.Nodes(); len(got) != 2 {
		t.Errorf("disconnect calls = %v, want two calls", got)
	}
}

// TestReenrollmentRevokesPreviousCredential proves that a new enrollment
// replaces the node's credential and closes the old connection.
func TestReenrollmentRevokesPreviousCredential(t *testing.T) {
	disconnect := &recorder{}
	service, _ := open(t, enroll.Options{Disconnect: disconnect})
	first := join(t, service, issue(t, service, "node-a").Plain)
	second := join(t, service, issue(t, service, "node-a").Plain)
	if first.Credential == second.Credential {
		t.Fatal("the second enrollment returned the same credential")
	}
	if _, err := service.Verify(first.Credential); !errors.Is(err, herrors.AuthenticationFailed) {
		t.Errorf("verify the replaced credential = %v, want authentication_failed", err)
	}
	if node, err := service.Verify(second.Credential); err != nil || node != "node-a" {
		t.Errorf("verify the new credential = %q, %v, want node-a, nil", node, err)
	}
	if got := disconnect.Nodes(); len(got) != 1 || got[0] != "node-a" {
		t.Errorf("disconnect calls = %v, want [node-a]: replacing a credential closes the connection", got)
	}
}

// TestCredentialBelongsToOneNode proves that a credential for one node can
// never authenticate as another node.
func TestCredentialBelongsToOneNode(t *testing.T) {
	service, _ := open(t, enroll.Options{})
	a := join(t, service, issue(t, service, "node-a").Plain)
	b := join(t, service, issue(t, service, "node-b").Plain)

	nodeA, err := service.Verify(a.Credential)
	if err != nil || nodeA != "node-a" {
		t.Fatalf("verify node-a's credential = %q, %v, want node-a, nil", nodeA, err)
	}
	nodeB, err := service.Verify(b.Credential)
	if err != nil || nodeB != "node-b" {
		t.Fatalf("verify node-b's credential = %q, %v, want node-b, nil", nodeB, err)
	}
	if nodeA == nodeB {
		t.Errorf("both credentials verify as %q: a credential must belong to one node", nodeA)
	}
	// Revoking one node's credential never touches the other's.
	if err := service.Revoke("node-b"); err != nil {
		t.Fatalf("revoke node-b: %v", err)
	}
	if node, err := service.Verify(a.Credential); err != nil || node != "node-a" {
		t.Errorf("verify node-a after revoking node-b = %q, %v, want node-a, nil", node, err)
	}
}

// TestNodeCheckKeepsTokenUnused proves the check runs before the token is
// consumed, so a mismatched agent can join after the node entry is fixed.
func TestNodeCheckKeepsTokenUnused(t *testing.T) {
	service, _ := open(t, enroll.Options{})
	token := issue(t, service, "node-a")
	mismatch := herrors.New(herrors.NodeMismatch, "the agent's OS differs", nil)
	if _, err := service.Join(token.Plain, func(string) error { return mismatch }); !errors.Is(err, herrors.NodeMismatch) {
		t.Fatalf("join with a failing check = %v, want node_mismatch", err)
	}
	out, err := service.Join(token.Plain, func(nodeID string) error {
		if nodeID != "node-a" {
			t.Errorf("check node = %q, want node-a", nodeID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("join after the failed check: %v", err)
	}
	if out.Node != "node-a" {
		t.Errorf("joined node = %q, want node-a", out.Node)
	}
}

// TestUnknownJoinToken proves that an unknown token and an empty one fail
// with the same code, so a response reveals nothing about which exist.
func TestUnknownJoinToken(t *testing.T) {
	service, _ := open(t, enroll.Options{})
	join(t, service, issue(t, service, "node-a").Plain)
	for _, token := range []string{"", "hjt_" + strings.Repeat("A", 43)} {
		if _, err := service.Join(token, nil); !errors.Is(err, herrors.JoinTokenInvalid) {
			t.Errorf("join with token %q = %v, want join_token_invalid", token, err)
		}
	}
}

// TestStoreKeepsOnlyHashes scans the store files for the plain join token
// and the plain credential (acceptance criterion: store only hashes).
func TestStoreKeepsOnlyHashes(t *testing.T) {
	dir := t.TempDir()
	service, _ := open(t, enroll.Options{StateDir: dir})
	token := issue(t, service, "node-a")
	out := join(t, service, token.Plain)
	// A used token and a live credential are both on disk now. Close first,
	// so the write-ahead log is part of the scan.
	if err := service.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the state directory: %v", err)
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, secret := range []string{token.Plain, out.Credential} {
			if strings.Contains(string(data), secret) {
				t.Errorf("%s holds the plain value %q", entry.Name(), secret)
			}
		}
	}
}

// TestConcurrentJoinsUseTokenOnce proves that one token enrolls one node
// once, even when several agents present it at the same time: exactly one
// join succeeds and every loser gets the documented 401.
func TestConcurrentJoinsUseTokenOnce(t *testing.T) {
	service, _ := open(t, enroll.Options{})
	token := issue(t, service, "node-a")
	const callers = 8
	start := make(chan struct{})
	outcomes := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := service.Join(token.Plain, nil)
			outcomes <- err
		}()
	}
	close(start)
	wg.Wait()
	close(outcomes)
	successes := 0
	for err := range outcomes {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, herrors.JoinTokenInvalid):
		default:
			t.Errorf("losing join error = %v, want join_token_invalid", err)
		}
	}
	if successes != 1 {
		t.Errorf("successful joins = %d, want exactly 1", successes)
	}
}

// TestNewTokenInvalidatesOlderToken proves that only the latest printed
// token works: issuing a token tears down the node's earlier ones.
func TestNewTokenInvalidatesOlderToken(t *testing.T) {
	service, _ := open(t, enroll.Options{})
	older := issue(t, service, "node-a")
	newer := issue(t, service, "node-a")
	if _, err := service.Join(older.Plain, nil); !errors.Is(err, herrors.JoinTokenInvalid) {
		t.Fatalf("join with the older token = %v, want join_token_invalid", err)
	}
	out := join(t, service, newer.Plain)
	if out.Node != "node-a" {
		t.Errorf("joined node = %q, want node-a", out.Node)
	}
}

// TestRevokeDropsPendingJoinToken proves that a token printed before a
// revoke cannot enroll the node afterwards.
func TestRevokeDropsPendingJoinToken(t *testing.T) {
	service, _ := open(t, enroll.Options{})
	token := issue(t, service, "node-a")
	if err := service.Revoke("node-a"); err != nil {
		t.Fatalf("revoke node-a: %v", err)
	}
	if _, err := service.Join(token.Plain, nil); !errors.Is(err, herrors.JoinTokenInvalid) {
		t.Errorf("join after revoke = %v, want join_token_invalid", err)
	}
}

// TestInvalidNodeID proves that issue and revoke refuse an ID that
// nodes.yaml could never hold.
func TestInvalidNodeID(t *testing.T) {
	service, _ := open(t, enroll.Options{})
	if _, err := service.IssueJoinToken("Node-A"); err == nil {
		t.Error("issue with an invalid node ID succeeded")
	}
	if err := service.Revoke("Node-A"); err == nil {
		t.Error("revoke with an invalid node ID succeeded")
	}
}

// TestEmptyCredential proves that verification without a credential fails
// with the documented code.
func TestEmptyCredential(t *testing.T) {
	service, _ := open(t, enroll.Options{})
	if _, err := service.Verify(""); !errors.Is(err, herrors.AuthenticationFailed) {
		t.Errorf("verify an empty credential = %v, want authentication_failed", err)
	}
}

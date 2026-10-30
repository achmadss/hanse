package enroll

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	// The pure Go SQLite driver, so the Gateway cross-compiles without
	// cgo (Makefile `cross`, .github/workflows/ci.yml).
	_ "modernc.org/sqlite"

	"github.com/achmadss/hanse/internal/auth"
	"github.com/achmadss/hanse/internal/config"
	herrors "github.com/achmadss/hanse/internal/errors"
)

// DatabaseName is the store's file name inside the Gateway state
// directory. It is a separate file from the history database so this
// package owns its whole schema (docs/configuration.md §1).
const DatabaseName = "enroll.db"

// DefaultJoinTokenTTL is how long a join token stays valid when the
// Gateway configuration sets no agents.join_token_ttl
// (docs/agent-protocol.md §2.1).
const DefaultJoinTokenTTL = 15 * time.Minute

// Token is one issued join token. Plain holds the value the operator
// prints once; the store keeps only its hash.
type Token struct {
	// Node is the node ID the token enrolls.
	Node string
	// Plain is the token value: JoinTokenPrefix and random characters.
	Plain string
	// Expires is when the token stops working.
	Expires time.Time
}

// Outcome is the result of a successful join: the node the token enrolled
// and the credential the agent stores.
type Outcome struct {
	Node       string
	Credential string
}

// Check runs during Join after the token is validated and before it is
// marked used. It receives the node ID the token belongs to. A non-nil
// return aborts the join and leaves the token unused, so an agent that
// does not match its node entry can try again after the operator fixes
// nodes.yaml (docs/agent-protocol.md §2.3).
type Check func(nodeID string) error

// Disconnecter closes a node's live agent connection. Connections are
// issue #38; until then the default is a no-op, and the Gateway passes its
// connection registry here.
type Disconnecter interface {
	Disconnect(nodeID string)
}

// Options configures Open.
type Options struct {
	// Path is the store file. Empty means <StateDir>/enroll.db.
	Path string
	// StateDir is the directory that holds the store. It is created with
	// mode 0700 when it is missing.
	StateDir string
	// JoinTokenTTL is how long an issued join token stays valid. Zero
	// takes DefaultJoinTokenTTL.
	JoinTokenTTL time.Duration
	// Now returns the current time. Nil means time.Now; tests inject a
	// clock here.
	Now func() time.Time
	// Disconnect closes a node's live connection after a join or a
	// revoke. Nil is a no-op.
	Disconnect Disconnecter
}

// Service issues, checks, and revokes join tokens and node credentials. It
// is safe for concurrent use: the API handles joins while operator
// commands read and write the same store.
type Service struct {
	db         *sql.DB
	path       string
	ttl        time.Duration
	now        func() time.Time
	disconnect Disconnecter

	closeOnce  sync.Once
	closeError error
}

// schema is the store layout. Times are Unix nanoseconds, which compare
// correctly in SQL and need no time zone handling.
var schema = []string{
	`CREATE TABLE IF NOT EXISTS join_tokens (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	node        TEXT NOT NULL,
	hash        TEXT NOT NULL UNIQUE,
	created_at  INTEGER NOT NULL,
	expires_at  INTEGER NOT NULL,
	used_at     INTEGER NOT NULL DEFAULT 0
)`,
	`CREATE INDEX IF NOT EXISTS join_tokens_node ON join_tokens (node)`,
	`CREATE TABLE IF NOT EXISTS node_credentials (
	node        TEXT PRIMARY KEY,
	hash        TEXT NOT NULL,
	created_at  INTEGER NOT NULL,
	revoked_at  INTEGER NOT NULL DEFAULT 0
)`,
}

// Open opens or creates the store. On Unix the store file is created with
// mode 0600 and the state directory with mode 0700, matching how
// internal/auth writes the tokens file and internal/history the database.
func Open(opts Options) (*Service, error) {
	path := opts.Path
	if path == "" {
		if opts.StateDir == "" {
			return nil, herrors.New(herrors.Internal,
				"the enrollment store needs a path or a state directory", nil)
		}
		path = filepath.Join(opts.StateDir, DatabaseName)
	}
	ttl := opts.JoinTokenTTL
	if ttl <= 0 {
		ttl = DefaultJoinTokenTTL
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	if err := prepareFiles(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", databaseDSN(path))
	if err != nil {
		return nil, storeError("cannot open the enrollment store", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, storeError("cannot open the enrollment store", err)
	}
	for _, statement := range schema {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			return nil, storeError("cannot prepare the enrollment store", err)
		}
	}
	return &Service{db: db, path: path, ttl: ttl, now: now, disconnect: opts.Disconnect}, nil
}

// databaseDSN builds the driver DSN. The pragmas run on every new
// connection: WAL for concurrent readers, a busy timeout so a writer waits,
// and NORMAL synchronous mode, which is safe with WAL for this use.
func databaseDSN(path string) string {
	return path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
}

// prepareFiles creates the store's directory and file with restrictive
// modes before SQLite opens it, so no umask can widen them. On Windows
// POSIX mode bits do not exist; the file then inherits the state
// directory's ACL, which is the limit of what Go offers portably.
func prepareFiles(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return storeError("cannot create the enrollment directory", err)
	}
	if err := restrictMode(dir, 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return storeError("cannot create the enrollment store", err)
	}
	if err := file.Close(); err != nil {
		return storeError("cannot create the enrollment store", err)
	}
	return restrictMode(path, 0o600)
}

// restrictMode tightens the mode of path on Unix. Windows has no POSIX mode
// bits; its ACL rule is future work, as in internal/config and
// internal/auth.
func restrictMode(path string, mode os.FileMode) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if err := os.Chmod(path, mode); err != nil {
		return storeError("cannot set the mode of "+path, err)
	}
	return nil
}

// Path returns the store file path.
func (s *Service) Path() string { return s.path }

// Close closes the store. It is safe to call more than once.
func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() { s.closeError = s.db.Close() })
	return s.closeError
}

// IssueJoinToken creates a one-time join token for nodeID and returns it.
// The caller prints Plain exactly once. The token expires after the
// configured lifetime, which is 15 minutes by default.
func (s *Service) IssueJoinToken(nodeID string) (Token, error) {
	if !config.ValidID(nodeID) {
		return Token{}, fmt.Errorf("node %q must be a valid node ID: lowercase letters, digits, \"_\" and \"-\", starting with a letter, at most 63 characters", nodeID)
	}
	plain, hash, err := generate(JoinTokenPrefix)
	if err != nil {
		return Token{}, herrors.New(herrors.Internal,
			"cannot generate a join token: "+err.Error(), nil)
	}
	now := s.now().UTC()
	expires := now.Add(s.ttl)
	// Drop this node's used and expired tokens, so the table does not grow
	// with every enrollment.
	if _, err := s.db.Exec(
		`DELETE FROM join_tokens WHERE node = ? AND (used_at <> 0 OR expires_at <= ?)`,
		nodeID, now.UnixNano()); err != nil {
		return Token{}, storeError("cannot clean the join tokens", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO join_tokens (node, hash, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		nodeID, hash, now.UnixNano(), expires.UnixNano()); err != nil {
		return Token{}, storeError("cannot store the join token", err)
	}
	return Token{Node: nodeID, Plain: plain, Expires: expires}, nil
}

// joinTokenRow is one stored join token during a join.
type joinTokenRow struct {
	id        int64
	node      string
	hash      string
	expiresAt int64
	usedAt    int64
}

// Join exchanges a join token for a node credential. An unknown, used, or
// expired token returns the JoinTokenInvalid code, HTTP 401, and all three
// cases look the same (docs/agent-protocol.md §2.3). A successful join
// marks the token used and replaces any previous credential of the node.
func (s *Service) Join(joinToken string, check Check) (Outcome, error) {
	if joinToken == "" {
		return Outcome{}, invalidJoinToken()
	}
	hash := auth.Hash(joinToken)
	now := s.now().UTC()

	tx, err := s.db.Begin()
	if err != nil {
		return Outcome{}, storeError("cannot start the join", err)
	}
	defer func() { _ = tx.Rollback() }()

	tokens, err := loadJoinTokens(tx)
	if err != nil {
		return Outcome{}, storeError("cannot read the join tokens", err)
	}
	// Compare every stored hash, so the time does not depend on which row
	// matched or on the row order.
	match := -1
	for i := range tokens {
		if hashMatches(tokens[i].hash, hash) {
			match = i
		}
	}
	if match < 0 || tokens[match].usedAt != 0 || now.UnixNano() >= tokens[match].expiresAt {
		return Outcome{}, invalidJoinToken()
	}
	node := tokens[match].node
	if check != nil {
		// The check runs before the token is marked used, so an agent
		// whose OS or architecture differs keeps its token.
		if err := check(node); err != nil {
			return Outcome{}, err
		}
	}
	if _, err := tx.Exec(
		`UPDATE join_tokens SET used_at = ? WHERE id = ?`,
		now.UnixNano(), tokens[match].id); err != nil {
		return Outcome{}, storeError("cannot mark the join token used", err)
	}
	plain, credentialHash, err := generate(CredentialPrefix)
	if err != nil {
		return Outcome{}, herrors.New(herrors.Internal,
			"cannot generate a node credential: "+err.Error(), nil)
	}
	// A new credential replaces the node's previous one, and enrolling
	// again revokes that previous credential (agent protocol §2.1). Only a
	// replaced credential can have a live connection, so only that case
	// closes one.
	revoked, err := tx.Exec(
		`UPDATE node_credentials SET revoked_at = ? WHERE node = ? AND revoked_at = 0`,
		now.UnixNano(), node)
	if err != nil {
		return Outcome{}, storeError("cannot revoke the previous node credential", err)
	}
	replaced, err := revoked.RowsAffected()
	if err != nil {
		return Outcome{}, storeError("cannot revoke the previous node credential", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO node_credentials (node, hash, created_at, revoked_at) VALUES (?, ?, ?, 0)
		ON CONFLICT(node) DO UPDATE SET hash = excluded.hash,
			created_at = excluded.created_at, revoked_at = 0`,
		node, credentialHash, now.UnixNano()); err != nil {
		return Outcome{}, storeError("cannot store the node credential", err)
	}
	if err := tx.Commit(); err != nil {
		return Outcome{}, storeError("cannot store the node credential", err)
	}
	// The node's open connection used the credential that just got
	// replaced, so it is closed now (agent protocol §2.1). Until issue #38
	// wires a connection registry, the default Disconnecter does nothing.
	if replaced > 0 {
		s.disconnectNode(node)
	}
	return Outcome{Node: node, Credential: plain}, nil
}

// Verify returns the node ID that a plain node credential belongs to. A
// missing, wrong, or revoked credential returns the AuthenticationFailed
// code, HTTP 401 (docs/architecture.md §14, agent protocol §3.1). The
// credential belongs to exactly one node, so a credential issued for one
// node can never authenticate as another.
func (s *Service) Verify(credential string) (string, error) {
	if credential == "" {
		return "", authenticationFailed()
	}
	hash := auth.Hash(credential)
	credentials, err := loadCredentials(s.db)
	if err != nil {
		return "", storeError("cannot read the node credentials", err)
	}
	node := ""
	// Compare every row, so the time does not depend on the row order.
	for _, c := range credentials {
		if c.revokedAt == 0 && hashMatches(c.hash, hash) {
			node = c.node
		}
	}
	if node == "" {
		return "", authenticationFailed()
	}
	return node, nil
}

// Revoke revokes nodeID's credential and closes its live connection. A
// node without a credential is a no-op, so revoking twice is safe. The
// next connection attempt of the node fails authentication until it
// enrolls again.
func (s *Service) Revoke(nodeID string) error {
	if !config.ValidID(nodeID) {
		return fmt.Errorf("node %q must be a valid node ID: lowercase letters, digits, \"_\" and \"-\", starting with a letter, at most 63 characters", nodeID)
	}
	if _, err := s.db.Exec(
		`UPDATE node_credentials SET revoked_at = ? WHERE node = ? AND revoked_at = 0`,
		s.now().UTC().UnixNano(), nodeID); err != nil {
		return storeError("cannot revoke the node credential", err)
	}
	s.disconnectNode(nodeID)
	return nil
}

// disconnectNode closes a node's live connection through the configured
// Disconnecter. A nil Disconnecter is the no-op default.
func (s *Service) disconnectNode(nodeID string) {
	if s.disconnect != nil {
		s.disconnect.Disconnect(nodeID)
	}
}

// credentialRow is one stored node credential during a check.
type credentialRow struct {
	node      string
	hash      string
	revokedAt int64
}

// loadJoinTokens reads every stored join token of tx.
func loadJoinTokens(tx *sql.Tx) ([]joinTokenRow, error) {
	rows, err := tx.Query(`SELECT id, node, hash, expires_at, used_at FROM join_tokens`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []joinTokenRow
	for rows.Next() {
		var row joinTokenRow
		if err := rows.Scan(&row.id, &row.node, &row.hash, &row.expiresAt, &row.usedAt); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// loadCredentials reads every stored node credential.
func loadCredentials(db *sql.DB) ([]credentialRow, error) {
	rows, err := db.Query(`SELECT node, hash, revoked_at FROM node_credentials`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []credentialRow
	for rows.Next() {
		var row credentialRow
		if err := rows.Scan(&row.node, &row.hash, &row.revokedAt); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// invalidJoinToken is the one error for an unknown, used, or expired token,
// so a response reveals nothing about which tokens exist. It never names a
// token value.
func invalidJoinToken() error {
	return herrors.New(herrors.JoinTokenInvalid, "", nil)
}

// authenticationFailed is the error of a missing, wrong, or revoked
// credential. It never names a credential value.
func authenticationFailed() error {
	return herrors.New(herrors.AuthenticationFailed, "", nil)
}

// storeError wraps a storage failure. It names the operation and the
// underlying problem, never a token or a credential value.
func storeError(doing string, err error) error {
	return herrors.New(herrors.Internal, doing+": "+err.Error(), nil)
}

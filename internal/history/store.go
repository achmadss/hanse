package history

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	// The pure Go SQLite driver. The Gateway cross-compiles without cgo
	// (Makefile `cross`, .github/workflows/ci.yml), so a cgo driver
	// would not build.
	_ "modernc.org/sqlite"

	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/operations"
)

// DatabaseName is the file name inside the Gateway state directory
// (docs/configuration.md section 1: the database lives in paths.state_dir).
const DatabaseName = "hanse.db"

// DefaultCleanupInterval is how often the retention timer runs.
const DefaultCleanupInterval = time.Hour

// Options configures Open.
type Options struct {
	// Path is the database file. Empty means <StateDir>/hanse.db.
	Path string
	// StateDir is the directory that holds the database. It is created
	// with mode 0700 when it is missing.
	StateDir string
	// Retention is how long finished records are kept. Zero disables the
	// cleanup.
	Retention time.Duration
	// CleanupInterval is how often the retention timer runs. Zero takes
	// DefaultCleanupInterval.
	CleanupInterval time.Duration
	// Now returns the current time. Nil means time.Now; tests inject a
	// clock here.
	Now func() time.Time
}

// Store is the SQLite implementation of operations.Store.
type Store struct {
	db        *sql.DB
	path      string
	retention time.Duration
	now       func() time.Time

	stop       chan struct{}
	done       chan struct{}
	closeOnce  sync.Once
	closeError error
}

var (
	_ operations.Store  = (*Store)(nil)
	_ operations.Pruner = (*Store)(nil)
)

// The record columns, in the order scanRecord expects.
const recordColumns = `id, capability, operation, requester, executor,
	executor_source, mode, status, created_at, started_at, finished_at,
	result, summary, failure_code, failure_message, failure_details,
	idempotency_key, argument_fingerprint, arguments, concurrency_key`

// schema is the database layout. The idempotency index is partial so the
// many records without a key do not collide on the empty string.
var schema = []string{
	`CREATE TABLE IF NOT EXISTS operations (
	id                  TEXT PRIMARY KEY,
	capability          TEXT NOT NULL,
	operation           TEXT NOT NULL,
	requester           TEXT NOT NULL,
	executor            TEXT NOT NULL,
	executor_source     TEXT NOT NULL,
	mode                TEXT NOT NULL,
	status              TEXT NOT NULL,
	created_at          TEXT NOT NULL,
	started_at          TEXT NOT NULL DEFAULT '',
	finished_at         TEXT NOT NULL DEFAULT '',
	result              BLOB,
	summary             TEXT NOT NULL DEFAULT '',
	failure_code        TEXT NOT NULL DEFAULT '',
	failure_message     TEXT NOT NULL DEFAULT '',
	failure_details     TEXT NOT NULL DEFAULT '',
	idempotency_key     TEXT NOT NULL DEFAULT '',
	argument_fingerprint TEXT NOT NULL DEFAULT '',
	arguments           BLOB,
	concurrency_key     TEXT NOT NULL DEFAULT ''
)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS operations_idempotency
	ON operations (requester, idempotency_key)
	WHERE idempotency_key <> ''`,
	`CREATE INDEX IF NOT EXISTS operations_finished
	ON operations (finished_at)`,
	`CREATE INDEX IF NOT EXISTS operations_active
	ON operations (capability, operation, status)`,
}

// Open opens or creates the database, recovers operations an earlier
// process left active, and removes records past the retention. It starts
// the retention timer when Retention is positive.
//
// On Unix the database file is created with mode 0600 and the state
// directory with mode 0700, matching how internal/auth writes the tokens
// file. SQLite runs in WAL mode so readers do not block a writer.
func Open(opts Options) (*Store, error) {
	path := opts.Path
	if path == "" {
		if opts.StateDir == "" {
			return nil, herrors.New(herrors.Internal,
				"the history store needs a path or a state directory", nil)
		}
		path = filepath.Join(opts.StateDir, DatabaseName)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	interval := opts.CleanupInterval
	if interval <= 0 {
		interval = DefaultCleanupInterval
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, herrors.New(herrors.Internal,
			fmt.Sprintf("cannot create the history directory %s: %v", dir, err), nil)
	}
	if err := restrictMode(dir, 0o700); err != nil {
		return nil, err
	}
	// Create the file before SQLite opens it, so no umask can widen the
	// mode and so an existing permissive file is tightened.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, herrors.New(herrors.Internal,
			fmt.Sprintf("cannot create the history database %s: %v", path, err), nil)
	}
	if err := file.Close(); err != nil {
		return nil, herrors.New(herrors.Internal,
			fmt.Sprintf("cannot create the history database %s: %v", path, err), nil)
	}
	if err := restrictMode(path, 0o600); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", databaseDSN(path))
	if err != nil {
		return nil, herrors.New(herrors.Internal,
			fmt.Sprintf("cannot open the history database %s: %v", path, err), nil)
	}
	// WAL lets readers proceed while a writer runs. A small pool keeps
	// the number of SQLite connections bounded; the busy timeout makes
	// two writers wait instead of failing.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, herrors.New(herrors.Internal,
			fmt.Sprintf("cannot open the history database %s: %v", path, err), nil)
	}
	if _, err := db.Exec(schema[0]); err != nil {
		_ = db.Close()
		return nil, herrors.New(herrors.Internal,
			fmt.Sprintf("cannot prepare the history database %s: %v", path, err), nil)
	}
	for _, statement := range schema[1:] {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			return nil, herrors.New(herrors.Internal,
				fmt.Sprintf("cannot prepare the history database %s: %v", path, err), nil)
		}
	}
	if err := prepareLogSchema(db); err != nil {
		_ = db.Close()
		return nil, herrors.New(herrors.Internal,
			fmt.Sprintf("cannot prepare the history database %s: %v", path, err), nil)
	}

	s := &Store{db: db, path: path, retention: opts.Retention, now: now}
	if _, err := s.RecoverInterrupted(now().UTC()); err != nil {
		_ = db.Close()
		return nil, err
	}
	if s.retention > 0 {
		if _, err := s.PurgeOld(now().UTC()); err != nil {
			_ = db.Close()
			return nil, err
		}
		s.startRetention(interval)
	}
	return s, nil
}

// databaseDSN builds the driver DSN. The pragmas run on every new
// connection: WAL for concurrent readers, a busy timeout so a writer waits,
// and NORMAL synchronous mode, which is safe with WAL for this use.
func databaseDSN(path string) string {
	return path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
}

// restrictMode tightens the mode of path on Unix. Windows has no POSIX mode
// bits; its ACL rule is future work, as in internal/config and internal/auth.
func restrictMode(path string, mode os.FileMode) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if err := os.Chmod(path, mode); err != nil {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("cannot set the mode of %s: %v", path, err), nil)
	}
	return nil
}

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// startRetention starts the periodic cleanup.
func (s *Store) startRetention(interval time.Duration) {
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				// A cleanup problem must not stop the timer; the next
				// tick retries.
				_, _ = s.PurgeOld(s.now().UTC())
			case <-s.stop:
				return
			}
		}
	}()
}

// Close stops the retention timer and closes the database. It is safe to
// call more than once.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		if s.stop != nil {
			close(s.stop)
			<-s.done
			s.stop = nil
		}
		s.closeError = s.db.Close()
	})
	return s.closeError
}

// Insert stores a new record. An ID that already exists is an error, as the
// Store interface requires.
func (s *Store) Insert(rec operations.Record) error {
	if rec.ID == "" {
		return herrors.New(herrors.Internal, "an operation record needs an ID", nil)
	}
	if _, ok, err := s.Get(rec.ID); err != nil {
		return err
	} else if ok {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("operation %q is stored twice", rec.ID), nil)
	}
	const query = `INSERT INTO operations (` + recordColumns + `)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := s.db.Exec(query, recordValues(rec)...); err != nil {
		return storeError("store operation "+rec.ID, err)
	}
	return nil
}

// Update replaces the record with the same ID. An unknown ID is an error.
func (s *Store) Update(rec operations.Record) error {
	const query = `UPDATE operations SET
		capability = ?, operation = ?, requester = ?, executor = ?,
		executor_source = ?, mode = ?, status = ?, created_at = ?,
		started_at = ?, finished_at = ?, result = ?, summary = ?,
		failure_code = ?, failure_message = ?, failure_details = ?,
		idempotency_key = ?, argument_fingerprint = ?, arguments = ?,
		concurrency_key = ?
		WHERE id = ?`
	values := recordValues(rec)
	// UPDATE takes the non-ID columns in the same order, then the ID.
	args := append(values[1:], values[0])
	result, err := s.db.Exec(query, args...)
	if err != nil {
		return storeError("update operation "+rec.ID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return storeError("update operation "+rec.ID, err)
	}
	if affected == 0 {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("operation %q is not stored", rec.ID), nil)
	}
	return nil
}

// Get returns one record by operation ID. A missing operation is the false
// bool; a database failure is the error.
func (s *Store) Get(id string) (operations.Record, bool, error) {
	row := s.db.QueryRow(`SELECT `+recordColumns+` FROM operations WHERE id = ?`, id)
	rec, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return operations.Record{}, false, nil
	}
	if err != nil {
		return operations.Record{}, false, storeError("read operation "+id, err)
	}
	return rec, true, nil
}

// List returns the records that satisfy f, sorted by operation ID
// (docs/architecture.md section 11.1: every list is sorted by ID).
func (s *Store) List(f operations.Filter) ([]operations.Record, error) {
	query := `SELECT ` + recordColumns + ` FROM operations`
	var clauses []string
	var args []any
	add := func(column, value string) {
		if value == "" {
			return
		}
		clauses = append(clauses, column+" = ?")
		args = append(args, value)
	}
	add("capability", f.Capability)
	add("operation", f.Operation)
	add("executor", f.Executor)
	add("status", string(f.Status))
	add("requester", f.Requester)
	if f.Active {
		clauses = append(clauses, "status IN (?, ?)")
		args = append(args, string(operations.StateQueued), string(operations.StateRunning))
	}
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY id"
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, storeError("list operations", err)
	}
	defer func() { _ = rows.Close() }()
	out := []operations.Record{}
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, storeError("read an operation row", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, storeError("list operations", err)
	}
	return out, nil
}

// ByIdempotency returns the operation of one requester and key. An empty
// key never matches. A missing record is the false bool; a database failure
// is the error, so a retry never looks like a new request.
func (s *Store) ByIdempotency(requester, key string) (operations.Record, bool, error) {
	if key == "" {
		return operations.Record{}, false, nil
	}
	row := s.db.QueryRow(`SELECT `+recordColumns+` FROM operations
		WHERE requester = ? AND idempotency_key = ? LIMIT 1`, requester, key)
	rec, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return operations.Record{}, false, nil
	}
	if err != nil {
		return operations.Record{}, false, storeError("read the idempotency key", err)
	}
	return rec, true, nil
}

// RecoverInterrupted fails every operation that is still queued or running,
// with the gateway_restart reason (docs/architecture.md section 9.6). It
// returns how many records it changed. Open runs it exactly once, before
// the Gateway can dispatch anything, because a later call would also fail
// the runs this process is executing.
func (s *Store) RecoverInterrupted(now time.Time) (int, error) {
	code := herrors.GatewayRestart
	result, err := s.db.Exec(`UPDATE operations
		SET status = ?, finished_at = ?, failure_code = ?, failure_message = ?
		WHERE status IN (?, ?)`,
		string(operations.StateFailed), formatTime(now), code.Name(), code.Message(),
		string(operations.StateQueued), string(operations.StateRunning))
	if err != nil {
		return 0, storeError("recover interrupted operations", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, storeError("recover interrupted operations", err)
	}
	return int(affected), nil
}

// PurgeOld deletes the finished records older than the retention period,
// and the logs of those operations with them. A zero retention keeps
// everything. An active record has no finish time, so it is never deleted.
func (s *Store) PurgeOld(now time.Time) (int64, error) {
	if s.retention <= 0 {
		return 0, nil
	}
	cutoff := formatTime(now.Add(-s.retention))
	if err := s.purgeLogs(cutoff); err != nil {
		return 0, err
	}
	result, err := s.db.Exec(`DELETE FROM operations
		WHERE finished_at <> '' AND finished_at < ?`, cutoff)
	if err != nil {
		return 0, storeError("remove old operations", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, storeError("remove old operations", err)
	}
	return affected, nil
}

// recordValues returns the record columns in recordColumns order.
func recordValues(rec operations.Record) []any {
	var result any
	if rec.Result != nil {
		result = []byte(rec.Result)
	}
	var arguments any
	if rec.Arguments != nil {
		arguments = []byte(rec.Arguments)
	}
	failureCode, failureMessage, failureDetails := "", "", ""
	if rec.Failure != nil {
		failureCode = rec.Failure.Code
		failureMessage = rec.Failure.Message
		if len(rec.Failure.Details) > 0 {
			if encoded, err := json.Marshal(rec.Failure.Details); err == nil {
				failureDetails = string(encoded)
			}
		}
	}
	return []any{
		rec.ID,
		rec.Capability,
		rec.Operation,
		rec.Requester,
		rec.Executor,
		rec.ExecutorSource,
		rec.Mode,
		string(rec.Status),
		formatTime(rec.CreatedAt),
		formatTime(rec.StartedAt),
		formatTime(rec.FinishedAt),
		result,
		rec.Summary,
		failureCode,
		failureMessage,
		failureDetails,
		rec.IdempotencyKey,
		rec.ArgumentFingerprint,
		arguments,
		rec.ConcurrencyKey,
	}
}

// scanner is the common Scan method of *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// scanRecord reads one row in recordColumns order.
func scanRecord(row scanner) (operations.Record, error) {
	var rec operations.Record
	var status, created, started, finished string
	var result, arguments []byte
	var failureCode, failureMessage, failureDetails string
	err := row.Scan(
		&rec.ID,
		&rec.Capability,
		&rec.Operation,
		&rec.Requester,
		&rec.Executor,
		&rec.ExecutorSource,
		&rec.Mode,
		&status,
		&created,
		&started,
		&finished,
		&result,
		&rec.Summary,
		&failureCode,
		&failureMessage,
		&failureDetails,
		&rec.IdempotencyKey,
		&rec.ArgumentFingerprint,
		&arguments,
		&rec.ConcurrencyKey,
	)
	if err != nil {
		return operations.Record{}, err
	}
	rec.Status = operations.State(status)
	rec.CreatedAt = parseTime(created)
	rec.StartedAt = parseTime(started)
	rec.FinishedAt = parseTime(finished)
	if result != nil {
		rec.Result = json.RawMessage(result)
	}
	if arguments != nil {
		rec.Arguments = json.RawMessage(arguments)
	}
	if failureCode != "" || failureMessage != "" || failureDetails != "" {
		rec.Failure = &operations.Failure{Code: failureCode, Message: failureMessage}
		if failureDetails != "" {
			var details map[string]any
			if err := json.Unmarshal([]byte(failureDetails), &details); err == nil {
				rec.Failure.Details = details
			}
		}
	}
	return rec, nil
}

// timestampLayout is the stored timestamp format: UTC, fixed width, and
// always nine fractional digits, so text order is time order. RFC 3339's
// variable fraction would make "10:00:00Z" sort after "10:00:00.5Z".
const timestampLayout = "2006-01-02T15:04:05.000000000Z"

// formatTime encodes a timestamp in UTC with a fixed width. The zero time
// is the empty string, which marks a timestamp that has not happened
// (docs/architecture.md section 9.1).
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(timestampLayout)
}

// parseTime decodes a stored timestamp; an empty or malformed value is the
// zero time.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// storeError wraps a database failure in the Internal code. The message
// names the action and the driver's error, never a record value.
func storeError(action string, err error) error {
	return herrors.New(herrors.Internal,
		fmt.Sprintf("the history store could not %s: %v", action, err), nil)
}

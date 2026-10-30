package history

import (
	"database/sql"

	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/operations"
)

// The history store also keeps the operation logs (docs/architecture.md
// section 10).
var _ operations.LogStore = (*Store)(nil)

// logSchema is the table layout of the operation logs. The pair
// (operation_id, seq) is the primary key: one entry per sequence number of
// one run, so a resume can never replay a line twice. The key also serves
// the ordered read of one operation.
var logSchema = []string{
	`CREATE TABLE IF NOT EXISTS operation_logs (
	operation_id TEXT NOT NULL,
	seq          INTEGER NOT NULL,
	ts           TEXT NOT NULL,
	stream       TEXT NOT NULL,
	line         TEXT NOT NULL,
	PRIMARY KEY (operation_id, seq)
)`,
}

// prepareLogSchema creates the log table. Open runs it after the operation
// table, because a log entry belongs to a stored operation.
func prepareLogSchema(db *sql.DB) error {
	for _, statement := range logSchema {
		if _, err := db.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

// AppendLog stores one log entry of an operation and returns it with the
// assigned sequence number. The number is the next unused one of that
// operation, assigned inside the same statement, so a concurrent append
// can never take the same number (docs/agent-protocol.md section 4.6: the
// Gateway assigns the operation log's own sequence numbers).
func (s *Store) AppendLog(operationID string, entry operations.LogEntry) (operations.LogEntry, error) {
	if operationID == "" {
		return operations.LogEntry{}, herrors.New(herrors.Internal,
			"a log entry needs an operation ID", nil)
	}
	if entry.Stream == "" {
		entry.Stream = operations.StreamSystem
	}
	if entry.Time.IsZero() {
		entry.Time = s.now().UTC()
	}
	const query = `INSERT INTO operation_logs (operation_id, seq, ts, stream, line)
		VALUES (?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM operation_logs WHERE operation_id = ?), ?, ?, ?)
		RETURNING seq`
	var seq int64
	if err := s.db.QueryRow(query, operationID, operationID,
		formatTime(entry.Time), entry.Stream, entry.Line).Scan(&seq); err != nil {
		return operations.LogEntry{}, storeError("store a log line of operation "+operationID, err)
	}
	entry.Seq = seq
	entry.Time = entry.Time.UTC()
	return entry, nil
}

// Logs returns the log entries of one operation with a sequence number
// greater than after, in sequence order (docs/architecture.md section 10).
func (s *Store) Logs(operationID string, after int64) ([]operations.LogEntry, error) {
	rows, err := s.db.Query(`SELECT seq, ts, stream, line FROM operation_logs
		WHERE operation_id = ? AND seq > ? ORDER BY seq`, operationID, after)
	if err != nil {
		return nil, storeError("read the log of operation "+operationID, err)
	}
	defer func() { _ = rows.Close() }()
	out := []operations.LogEntry{}
	for rows.Next() {
		var entry operations.LogEntry
		var ts string
		if err := rows.Scan(&entry.Seq, &ts, &entry.Stream, &entry.Line); err != nil {
			return nil, storeError("read a log row of operation "+operationID, err)
		}
		entry.Time = parseTime(ts)
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, storeError("read the log of operation "+operationID, err)
	}
	return out, nil
}

// purgeLogs deletes the logs of the operations PurgeOld removes. The
// selection runs before the operation rows are deleted, because the
// finished time lives on the operation row. Logs follow the same retention
// as history (docs/configuration.md section 3).
func (s *Store) purgeLogs(cutoff string) error {
	if _, err := s.db.Exec(`DELETE FROM operation_logs WHERE operation_id IN (
		SELECT id FROM operations WHERE finished_at <> '' AND finished_at < ?)`, cutoff); err != nil {
		return storeError("remove old operation logs", err)
	}
	return nil
}

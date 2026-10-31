package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/achmadss/hanse/internal/client"
	herrors "github.com/achmadss/hanse/internal/errors"
)

// logsCommand implements logs <id> and logs <id> --follow. Without --follow
// it prints the stored lines. With --follow it streams the log until the
// operation ends, then exits with the operation's exit code, using the same
// mapping as run (docs/architecture.md sections 10 and 14).
func (a *app) logsCommand(ctx context.Context, args []string) error {
	fs := newFlagSet("logs")
	formatValue := formatFlag(fs)
	follow := false
	fs.BoolVar(&follow, "follow", false, "stream the log until the operation ends")
	fs.BoolVar(&follow, "f", false, "shorthand for --follow")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	fm, err := outputFormat(*formatValue)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return usagef("usage: hanse logs <operation-id> [-f]")
	}
	operationID := positional[0]

	c, _, err := a.open(ctx)
	if err != nil {
		return err
	}
	if !follow {
		logs, err := c.Logs(ctx, operationID, 0)
		if err != nil {
			return err
		}
		for _, entry := range logs.Logs {
			if err := a.writeLogEntry(fm, entry); err != nil {
				return err
			}
		}
		return nil
	}

	record, err := a.followLogs(ctx, c, operationID, fm)
	if err != nil {
		if errors.Is(err, errWaitInterrupted) {
			// Ctrl+C stops the following only: the operation keeps
			// running (docs/architecture.md section 12).
			a.warnf("stopped following operation %s; it is still running: follow it again with \"hanse logs %s -f\"",
				record.ID, record.ID)
		}
		return err
	}
	return operationFailure(record)
}

// followLogs streams one operation's log and returns the final state that
// the stream's status event reports. Ctrl+C ends the stream and leaves the
// operation running, which errWaitInterrupted reports: Run turns it into
// exit code 130.
func (a *app) followLogs(ctx context.Context, c *client.Client, operationID string, fm format) (client.OperationRecord, error) {
	var status client.LogStatus
	haveStatus := false
	err := c.FollowLogs(ctx, operationID, 0, func(event client.LogEvent) error {
		switch event.Event {
		case client.EventLog:
			return a.writeLogEntry(fm, event.Entry)
		case client.EventStatus:
			status = event.Status
			haveStatus = true
			if fm == formatJSON {
				return writeJSONLine(a.opts.Stdout, logStatusJSON{
					OperationID: status.OperationID,
					Status:      status.Status,
					Error:       status.Error,
				})
			}
		}
		return nil
	})
	record := client.OperationRecord{ID: operationID, Status: status.Status, Error: status.Error}
	if status.OperationID != "" {
		record.ID = status.OperationID
	}
	if err != nil {
		if ctx.Err() != nil {
			return record, errWaitInterrupted
		}
		return record, err
	}
	if !haveStatus {
		return record, herrors.New(herrors.Internal,
			fmt.Sprintf("the log stream of operation %s ended without a final status", operationID), nil)
	}
	return record, nil
}

// writeLogEntry prints one log line. The text format prefixes every line
// with its stream, "stderr: " or "system: ", so Gateway events are never
// mistaken for handler output. The JSON format prints one object per line,
// so a script can read a live stream line by line.
func (a *app) writeLogEntry(fm format, entry client.LogEntry) error {
	stream := entry.Stream
	if stream == "" {
		stream = client.StreamSystem
	}
	if fm == formatJSON {
		return writeJSONLine(a.opts.Stdout, logLineJSON{
			Stream: stream,
			Line:   entry.Line,
			Time:   entry.Time,
			ID:     entry.Seq,
		})
	}
	_, err := fmt.Fprintf(a.opts.Stdout, "%s: %s\n", stream, entry.Line)
	return err
}

// logLineJSON is one log line in the JSON format of the logs command. The
// key order is stable, and the time keeps the Gateway's timestamp.
type logLineJSON struct {
	Stream string    `json:"stream"`
	Line   string    `json:"line"`
	Time   time.Time `json:"time"`
	ID     int64     `json:"id"`
}

// logStatusJSON is the last object of a followed log in JSON form: the
// operation's final state and its recorded error, if any.
type logStatusJSON struct {
	OperationID string                 `json:"operation_id"`
	Status      string                 `json:"status"`
	Error       *client.OperationError `json:"error,omitempty"`
}

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/operations"
)

// logView is one stored log line (docs/architecture.md section 10). Seq is
// the operation's own sequence number, which the Last-Event-ID header
// carries.
type logView struct {
	Seq    int64     `json:"seq"`
	Time   time.Time `json:"ts"`
	Stream string    `json:"stream"`
	Line   string    `json:"line"`
}

// logsResponse is the body of GET /v1/operations/{id}/logs.
type logsResponse struct {
	Logs []logView `json:"logs"`
}

// logStatusView is the data of the final status event of a followed log.
type logStatusView struct {
	OperationID string       `json:"operation_id"`
	Status      string       `json:"status"`
	Error       *failureView `json:"error,omitempty"`
}

// getOperationLogs answers GET /v1/operations/{id}/logs. Without follow it
// returns every stored line as JSON; with `follow=true` it streams the
// lines as Server-Sent Events and closes after a final status event
// (docs/architecture.md section 10). The Last-Event-ID header resumes at the
// line after the given sequence number, for both forms.
func (s *server) getOperationLogs(w http.ResponseWriter, r *http.Request) {
	id, ok := s.identity(w, r)
	if !ok {
		return
	}
	ops, ok := s.operations(w)
	if !ok {
		return
	}
	rec, err := ops.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if err := id.Authorize(rec.Capability, rec.Operation); err != nil {
		writeError(w, err)
		return
	}
	after, err := lastEventID(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if r.URL.Query().Get("follow") != "true" {
		entries, err := ops.Logs(rec.ID, after)
		if err != nil {
			writeError(w, err)
			return
		}
		out := logsResponse{Logs: make([]logView, 0, len(entries))}
		for _, entry := range entries {
			out.Logs = append(out.Logs, logViewOf(entry))
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	s.followOperationLogs(w, r, ops, rec, after)
}

// followOperationLogs streams one operation's log. It subscribes before it
// reads the backlog, so it sees every line exactly once, and it closes the
// stream with the operation's final status.
func (s *server) followOperationLogs(w http.ResponseWriter, r *http.Request, ops *operations.Manager, rec operations.Record, after int64) {
	sub, backlog, err := ops.Follow(rec.ID, after)
	if err != nil {
		writeError(w, err)
		return
	}
	defer sub.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, herrors.New(herrors.Internal,
			"the response writer cannot stream Server-Sent Events", nil))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// A reverse proxy must not buffer the stream (docs/architecture.md
	// section 10: SSE works through reverse proxies).
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// last drops a repeated sequence number, so a resume never shows a
	// line twice even when the client re-sends an older Last-Event-ID.
	last := after
	send := func(entries []operations.LogEntry) bool {
		for _, entry := range entries {
			if entry.Seq <= last {
				continue
			}
			data, err := json.Marshal(logViewOf(entry))
			if err != nil {
				return false
			}
			if err := writeLogEvent(w, entry.Seq, "log", data); err != nil {
				return false
			}
			last = entry.Seq
		}
		flusher.Flush()
		return true
	}
	if !send(backlog) {
		return
	}

	// A quiet run must not leave the stream silent, or a reverse proxy
	// closes the idle upstream. A comment line carries no id, so it never
	// changes Last-Event-ID.
	var ticks <-chan time.Time
	if s.keepAlive > 0 {
		ticker := time.NewTicker(s.keepAlive)
		defer ticker.Stop()
		ticks = ticker.C
	}
	done := ops.Done(rec.ID)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-done:
			// The manager writes the last line before it closes the done
			// channel, so one drain takes everything that remains.
			if !send(sub.Drain()) {
				return
			}
			final, err := ops.Get(rec.ID)
			if err != nil {
				return
			}
			sendLogStatus(w, flusher, final)
			return
		case <-ticks:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case <-sub.Wake():
			if !send(sub.Drain()) {
				return
			}
		}
	}
}

// sendLogStatus writes the final status event and closes the stream.
func sendLogStatus(w http.ResponseWriter, flusher http.Flusher, rec operations.Record) {
	data, err := json.Marshal(logStatusView{
		OperationID: rec.ID,
		Status:      string(rec.Status),
		Error:       failureViewOf(rec),
	})
	if err != nil {
		return
	}
	_ = writeLogEvent(w, 0, "status", data)
	flusher.Flush()
}

// writeLogEvent writes one Server-Sent Event. A zero id omits the id field.
// The data is one JSON object, which never holds a raw newline, so one data
// line is enough.
func writeLogEvent(w http.ResponseWriter, id int64, event string, data []byte) error {
	var b strings.Builder
	if id > 0 {
		fmt.Fprintf(&b, "id: %d\n", id)
	}
	fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", event, data)
	_, err := w.Write([]byte(b.String()))
	return err
}

// lastEventID returns the sequence number of the Last-Event-ID header, or
// zero when the header is absent. A value that is not a non-negative
// integer is the InvalidRequest code, HTTP 400.
func lastEventID(r *http.Request) (int64, error) {
	raw := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if raw == "" {
		return 0, nil
	}
	seq, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seq < 0 {
		return 0, herrors.New(herrors.InvalidRequest,
			"the Last-Event-ID header is not a sequence number",
			map[string]any{"header": "Last-Event-ID"})
	}
	return seq, nil
}

// logViewOf renders one stored entry for a response. The sink redacts the
// run's secrets before it stores a line, so nothing here needs redaction.
func logViewOf(entry operations.LogEntry) logView {
	return logView{
		Seq:    entry.Seq,
		Time:   entry.Time,
		Stream: entry.Stream,
		Line:   entry.Line,
	}
}

// failureViewOf renders the recorded failure of an operation, or nil.
func failureViewOf(rec operations.Record) *failureView {
	if rec.Failure == nil {
		return nil
	}
	return &failureView{
		Code:    rec.Failure.Code,
		Message: rec.Failure.Message,
		Details: rec.Failure.Details,
	}
}

package client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	herrors "github.com/achmadss/hanse/internal/errors"
)

// The fixtures below follow internal/api/logs.go. Every value is a
// placeholder.

// TestOperationLogs reads the stored lines of an operation and resumes with
// the Last-Event-ID header.
func TestOperationLogs(t *testing.T) {
	var (
		mu      sync.Mutex
		headers []string
	)
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"/v1/operations/op_01J9Z3/logs": func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			headers = append(headers, r.Header.Get("Last-Event-ID"))
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"logs":[
				{"seq":2,"ts":"2026-10-31T08:00:01Z","stream":"system","line":"executor resolved: node-a"},
				{"seq":3,"ts":"2026-10-31T08:00:02Z","stream":"stderr","line":"waking node-c"}
			]}`))
		},
	})
	c := newTestClient(t, srv)

	logs, err := c.Logs(context.Background(), "op_01J9Z3", 1)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if len(logs.Logs) != 2 {
		t.Fatalf("logs: got %d lines, want 2", len(logs.Logs))
	}
	if logs.Logs[0].Seq != 2 || logs.Logs[0].Stream != StreamSystem {
		t.Errorf("logs: first line %+v", logs.Logs[0])
	}
	if logs.Logs[1].Line != "waking node-c" || logs.Logs[1].Stream != StreamStderr {
		t.Errorf("logs: second line %+v", logs.Logs[1])
	}
	if got := logs.Logs[1].Time.UTC().Format(time.RFC3339); got != "2026-10-31T08:00:02Z" {
		t.Errorf("logs: time %q", got)
	}

	if _, err := c.Logs(context.Background(), "op_01J9Z3", 0); err != nil {
		t.Fatalf("logs from the start: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(headers) != 2 {
		t.Fatalf("logs: %d requests, want 2", len(headers))
	}
	if headers[0] != "1" {
		t.Errorf("logs: first Last-Event-ID %q, want 1", headers[0])
	}
	if headers[1] != "" {
		t.Errorf("logs: second Last-Event-ID %q, want none", headers[1])
	}
}

// TestOperationLogsEmptyList requires an empty list rather than null.
func TestOperationLogsEmptyList(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"/v1/operations/op_01J9Z3/logs": jsonRoute(`{"logs":null}`),
	})
	c := newTestClient(t, srv)

	logs, err := c.Logs(context.Background(), "op_01J9Z3", 0)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if logs.Logs == nil {
		t.Error("logs: got nil, want an empty slice")
	}
}

// TestReadSSE covers the line rules of the format: comments, CRLF, an id and
// an event field, a field with no value, several data lines, and an event
// that the stream cut off.
func TestReadSSE(t *testing.T) {
	const stream = ": keepalive\r\n" +
		"id: 7\r\n" +
		"event: log\r\n" +
		"data: {\"seq\":7,\r\n" +
		"data: \"line\":\"first\"}\r\n" +
		"\r\n" +
		"id: 8\n" +
		"data: {\"seq\":8}\n" +
		"\n" +
		"id: 9\nevent: note\ndata: short\n\n" +
		"data: cut off"
	var events []sseEvent
	lines, err := readSSE(strings.NewReader(stream), func(event sseEvent) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatalf("readSSE: %v", err)
	}
	if lines == 0 {
		t.Error("readSSE: read no lines")
	}
	if len(events) != 3 {
		t.Fatalf("readSSE: got %d events, want 3 (the comment and the cut-off event are dropped): %+v", len(events), events)
	}
	if events[0].ID != "7" || events[0].Event != "log" {
		t.Errorf("readSSE: first event %+v", events[0])
	}
	if want := "{\"seq\":7,\n\"line\":\"first\"}"; events[0].Data != want {
		t.Errorf("readSSE: first data %q, want %q", events[0].Data, want)
	}
	if events[1].Event != "message" {
		t.Errorf("readSSE: an event without an event field is %q, want message", events[1].Event)
	}
	if events[2].ID != "9" || events[2].Event != "note" || events[2].Data != "short" {
		t.Errorf("readSSE: third event %+v", events[2])
	}
}

// TestReadSSEEmitError stops the reader when the caller reports an error.
func TestReadSSEEmitError(t *testing.T) {
	sentinel := fmt.Errorf("stop the reader")
	_, err := readSSE(strings.NewReader("data: one\n\ndata: two\n\n"), func(sseEvent) error {
		return sentinel
	})
	if err != sentinel {
		t.Fatalf("readSSE: error %v, want the sentinel", err)
	}
}

// followServer serves a followed log. Each attempt is one request; the
// handler sees the attempt number and the Last-Event-ID header it carried.
func followServer(t *testing.T, attempt func(n int, lastEventID string, w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		n := requests
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"authentication_failed","message":"the token is wrong"}`))
			return
		}
		if r.URL.Query().Get("follow") != "true" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":"invalid_arguments","message":"the fixture follows only"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		attempt(n, r.Header.Get("Last-Event-ID"), w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// writeSSE writes one event and flushes it, so the client sees it at once.
func writeSSE(w http.ResponseWriter, id int64, event, data string) {
	flusher, _ := w.(http.Flusher)
	if id > 0 {
		_, _ = fmt.Fprintf(w, "id: %d\n", id)
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	if flusher != nil {
		flusher.Flush()
	}
}

// logStreamLine renders one log event body for a sequence number and line.
func logStreamLine(seq int64, line string) string {
	return fmt.Sprintf(`{"seq":%d,"ts":"2026-10-31T08:00:0%dZ","stream":"stderr","line":%q}`,
		seq, seq, line)
}

// logStreamStatus renders one final status event body.
func logStreamStatus(operationID, status, extra string) string {
	return fmt.Sprintf(`{"operation_id":%q,"status":%q%s}`, operationID, status, extra)
}

// TestFollowLogs covers a stream that ends with the completed status.
func TestFollowLogs(t *testing.T) {
	srv := followServer(t, func(_ int, _ string, w http.ResponseWriter, _ *http.Request) {
		writeSSE(w, 1, EventLog, logStreamLine(1, "first"))
		writeSSE(w, 2, EventLog, logStreamLine(2, "second"))
		writeSSE(w, 0, EventStatus, logStreamStatus("op_01J9Z3", "completed", ""))
	})
	c := newTestClient(t, srv)

	var lines []string
	var status LogStatus
	err := c.FollowLogs(context.Background(), "op_01J9Z3", 0, func(event LogEvent) error {
		switch event.Event {
		case EventLog:
			lines = append(lines, event.Entry.Line)
		case EventStatus:
			status = event.Status
		}
		return nil
	})
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	if strings.Join(lines, ",") != "first,second" {
		t.Errorf("follow: lines %v, want [first second]", lines)
	}
	if status.Status != "completed" || status.OperationID != "op_01J9Z3" {
		t.Errorf("follow: status %+v", status)
	}
}

// TestFollowLogsReconnect drops the connection after one line and requires
// the resume to carry Last-Event-ID, so no line repeats and none is skipped.
func TestFollowLogsReconnect(t *testing.T) {
	var (
		mu      sync.Mutex
		headers []string
	)
	srv := followServer(t, func(n int, lastEventID string, w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		headers = append(headers, lastEventID)
		mu.Unlock()
		switch n {
		case 1:
			// Deliver one line, then drop the connection before the
			// status event, as a proxy timeout would.
			writeSSE(w, 1, EventLog, logStreamLine(1, "first"))
		default:
			// Replay the line that Last-Event-ID names, exactly
			// as a resume may, then continue and end.
			writeSSE(w, 1, EventLog, logStreamLine(1, "first"))
			writeSSE(w, 2, EventLog, logStreamLine(2, "second"))
			writeSSE(w, 3, EventLog, logStreamLine(3, "third"))
			writeSSE(w, 0, EventStatus, logStreamStatus("op_01J9Z3", "completed", ""))
		}
	})
	c := newTestClient(t, srv)
	c.follow = followTuning{
		initialBackoff: time.Millisecond,
		maxBackoff:     5 * time.Millisecond,
		retryWindow:    time.Second,
		now:            time.Now,
	}

	var lines []string
	err := c.FollowLogs(context.Background(), "op_01J9Z3", 0, func(event LogEvent) error {
		if event.Event == EventLog {
			lines = append(lines, event.Entry.Line)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	if strings.Join(lines, ",") != "first,second,third" {
		t.Errorf("follow: lines %v, want [first second third] with no duplicate and no gap", lines)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(headers) != 2 {
		t.Fatalf("follow: %d requests, want 2", len(headers))
	}
	if headers[0] != "" {
		t.Errorf("follow: first Last-Event-ID %q, want none", headers[0])
	}
	if headers[1] != "1" {
		t.Errorf("follow: resume Last-Event-ID %q, want 1", headers[1])
	}
}

// TestFollowLogsObservesContext requires Ctrl+C to end a quiet stream.
func TestFollowLogsObservesContext(t *testing.T) {
	srv := followServer(t, func(_ int, _ string, w http.ResponseWriter, r *http.Request) {
		writeSSE(w, 1, EventLog, logStreamLine(1, "first"))
		<-r.Context().Done()
	})
	c := newTestClient(t, srv)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- c.FollowLogs(ctx, "op_01J9Z3", 0, nil)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("follow: no error after the cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follow: the cancellation did not end the stream")
	}
}

// TestFollowLogsFatalError requires a rejected request to fail at once
// instead of retrying for the whole window.
func TestFollowLogsFatalError(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"unknown_operation","message":"operation does not exist"}`))
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv)

	err := c.FollowLogs(context.Background(), "op_99ZZZZ", 0, nil)
	if err == nil {
		t.Fatal("follow: no error for an unknown operation")
	}
	if !strings.Contains(err.Error(), "unknown_operation") {
		t.Errorf("follow: error %q, want the unknown_operation code", err)
	}
	if got := herrors.ExitCode(err); got != 21 {
		t.Errorf("follow: exit code %d, want 21", got)
	}
	if requests != 1 {
		t.Errorf("follow: %d requests, want 1", requests)
	}
}

// TestFollowLogsGivesUp requires one clear error once no reconnect succeeds
// within the window.
func TestFollowLogsGivesUp(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":"executor_unavailable","message":"the executor is not connected"}`))
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv)
	c.follow = followTuning{
		initialBackoff: time.Millisecond,
		maxBackoff:     2 * time.Millisecond,
		retryWindow:    30 * time.Millisecond,
		now:            time.Now,
	}

	start := time.Now()
	err := c.FollowLogs(context.Background(), "op_01J9Z3", 0, nil)
	if err == nil {
		t.Fatal("follow: no error after the reconnect window")
	}
	if !strings.Contains(err.Error(), "did not reconnect") {
		t.Errorf("follow: error %q, want a reconnect failure", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("follow: gave up after %s, want a bounded wait", elapsed)
	}
	if requests < 2 {
		t.Errorf("follow: %d requests, want more than one", requests)
	}
}

// TestFollowLogsStatusError decodes the recorded error of a failed
// operation.
func TestFollowLogsStatusError(t *testing.T) {
	srv := followServer(t, func(_ int, _ string, w http.ResponseWriter, _ *http.Request) {
		writeSSE(w, 1, EventLog, logStreamLine(1, "first"))
		writeSSE(w, 0, EventStatus, logStreamStatus("op_01J9Z6", "failed",
			`,"error":{"code":"handler_exit","message":"the handler exited nonzero","details":{"exit_code":1}}`))
	})
	c := newTestClient(t, srv)

	var status LogStatus
	err := c.FollowLogs(context.Background(), "op_01J9Z6", 0, func(event LogEvent) error {
		if event.Event == EventStatus {
			status = event.Status
		}
		return nil
	})
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	if status.Status != "failed" || status.Error == nil {
		t.Fatalf("follow: status %+v", status)
	}
	if status.Error.Code != "handler_exit" {
		t.Errorf("follow: error code %q, want handler_exit", status.Error.Code)
	}
}

// TestFollowLogsHandlerErrorStops requires a failed write to end the stream
// instead of retrying it.
func TestFollowLogsHandlerErrorStops(t *testing.T) {
	srv := followServer(t, func(_ int, _ string, w http.ResponseWriter, _ *http.Request) {
		writeSSE(w, 1, EventLog, logStreamLine(1, "first"))
		writeSSE(w, 2, EventLog, logStreamLine(2, "second"))
	})
	c := newTestClient(t, srv)

	sentinel := fmt.Errorf("the output is closed")
	err := c.FollowLogs(context.Background(), "op_01J9Z3", 0, func(LogEvent) error {
		return sentinel
	})
	if !strings.Contains(err.Error(), sentinel.Error()) {
		t.Fatalf("follow: error %v, want the handler error", err)
	}
}

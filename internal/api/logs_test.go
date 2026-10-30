package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// sseEvent is one Server-Sent Event of a followed log.
type sseEvent struct {
	ID    int64
	Event string
	Data  string
}

// logServer serves the fixture over a real HTTP server, because a followed
// log streams until the operation ends and a recorder cannot be read while
// the handler still writes.
func logServer(t *testing.T, fixture *executionFixture) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(fixture.handler)
	t.Cleanup(server.Close)
	return server
}

// getLogs performs one authenticated GET against the log endpoint of the
// server. The caller closes the body.
func getLogs(t *testing.T, server *httptest.Server, token, path string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

// readSSE reads events until the stream closes.
func readSSE(t *testing.T, resp *http.Response) []sseEvent {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content type %q, want text/event-stream", got)
	}
	var events []sseEvent
	var current sseEvent
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			id, err := strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
			if err != nil {
				t.Fatalf("event id line %q: %v", line, err)
			}
			current.ID = id
		case strings.HasPrefix(line, "event: "):
			current.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			current.Data = strings.TrimPrefix(line, "data: ")
		case line == "":
			if current.Event != "" {
				events = append(events, current)
			}
			current = sseEvent{}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read event stream: %v", err)
	}
	return events
}

// splitLogEvents separates the log events from the final status event.
func splitLogEvents(t *testing.T, events []sseEvent) ([]sseEvent, sseEvent) {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("the followed log sent no events")
	}
	last := events[len(events)-1]
	if last.Event != "status" {
		t.Fatalf("last event %q, want status", last.Event)
	}
	for _, event := range events[:len(events)-1] {
		if event.Event != "log" {
			t.Fatalf("event %q before the status event, want log", event.Event)
		}
	}
	return events[:len(events)-1], last
}

// TestLogsEndpoint reads the stored log as JSON after the operation ended.
func TestLogsEndpoint(t *testing.T) {
	fixture := newExecutionFixture(t)
	server := logServer(t, fixture)
	body := `{"capability":"echo","operation":"say","arguments":{"message":"hello"}}`
	resp, payload := call(t, fixture.handler, fixture.token, http.MethodPost, "/v1/operations", body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("submit: status %d, body %s", resp.StatusCode, payload)
	}
	view := decodeOperation(t, payload)

	resp = getLogs(t, server, fixture.token, "/v1/operations/"+view.ID+"/logs", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logs: status %d", resp.StatusCode)
	}
	var logs logsResponse
	if err := json.NewDecoder(resp.Body).Decode(&logs); err != nil {
		t.Fatalf("decode logs: %v", err)
	}
	if len(logs.Logs) < 3 {
		t.Fatalf("stored %d lines, want the executor, start, and exit entries: %v", len(logs.Logs), logs.Logs)
	}
	for i, entry := range logs.Logs {
		if entry.Seq != int64(i+1) {
			t.Errorf("line %d has sequence %d, want %d", i, entry.Seq, i+1)
		}
		if entry.Stream != "system" {
			t.Errorf("line %d has stream %q, want system", i, entry.Stream)
		}
	}
	if logs.Logs[0].Line != "executor resolved: gateway (default)" {
		t.Errorf("first line %q, want the executor resolution", logs.Logs[0].Line)
	}
	if last := logs.Logs[len(logs.Logs)-1].Line; last != "handler exited: completed" {
		t.Errorf("last line %q, want the handler exit", last)
	}

	// The Last-Event-ID header resumes the JSON read as well.
	resp = getLogs(t, server, fixture.token, "/v1/operations/"+view.ID+"/logs",
		map[string]string{"Last-Event-ID": "2"})
	defer func() { _ = resp.Body.Close() }()
	var resumed logsResponse
	if err := json.NewDecoder(resp.Body).Decode(&resumed); err != nil {
		t.Fatalf("decode resumed logs: %v", err)
	}
	if len(resumed.Logs) != len(logs.Logs)-2 || resumed.Logs[0].Seq != 3 {
		t.Errorf("resume after 2 returned %d lines starting at %d, want %d lines starting at 3",
			len(resumed.Logs), resumed.Logs[0].Seq, len(logs.Logs)-2)
	}
}

// TestFollowLogsLive covers the first acceptance criterion over HTTP: a
// follower that joins a running operation gets the earlier lines from the
// backlog and then the live lines, with no gap and no duplicate, and the
// stream ends with a status event.
func TestFollowLogsLive(t *testing.T) {
	fixture := newExecutionFixture(t)
	server := logServer(t, fixture)
	const seconds = 40
	body := fmt.Sprintf(`{"capability":"echo","operation":"sleep","arguments":{"seconds":%d}}`, seconds)
	resp, payload := call(t, fixture.handler, fixture.token, http.MethodPost, "/v1/operations", body, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("submit: status %d, body %s", resp.StatusCode, payload)
	}
	view := decodeOperation(t, payload)
	awaitOperation(t, fixture, view.ID, "running")

	// Wait for a few lines, so the follower certainly joins mid-run and
	// depends on the backlog for the lines it missed.
	before := 0
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && before < 3 {
		resp := getLogs(t, server, fixture.token, "/v1/operations/"+view.ID+"/logs", nil)
		var logs logsResponse
		err := json.NewDecoder(resp.Body).Decode(&logs)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("decode logs: %v", err)
		}
		before = len(logs.Logs)
		if before < 3 {
			time.Sleep(2 * time.Millisecond)
		}
	}
	if before < 3 {
		t.Fatal("the operation produced no log lines")
	}

	follow := getLogs(t, server, fixture.token, "/v1/operations/"+view.ID+"/logs?follow=true", nil)
	logEvents, status := splitLogEvents(t, readSSE(t, follow))
	if len(logEvents) == 0 {
		t.Fatal("the follower received no lines")
	}
	if logEvents[0].ID != 1 {
		t.Errorf("the follower starts at line %d, want the first line", logEvents[0].ID)
	}
	seen := map[int64]bool{}
	for i, event := range logEvents {
		if event.ID != int64(i+1) {
			t.Fatalf("event %d has sequence %d, want %d", i, event.ID, i+1)
		}
		if seen[event.ID] {
			t.Fatalf("line %d was sent twice", event.ID)
		}
		seen[event.ID] = true
	}
	if int64(len(logEvents)) <= int64(before) {
		t.Errorf("the follower got %d lines after joining at %d, want live lines too", len(logEvents), before)
	}
	var final logStatusView
	if err := json.Unmarshal([]byte(status.Data), &final); err != nil {
		t.Fatalf("decode status event %q: %v", status.Data, err)
	}
	if final.Status != "completed" || final.OperationID != view.ID {
		t.Errorf("status event %+v, want the completed operation", final)
	}
	if len(logEvents) < seconds {
		t.Errorf("the follower got %d lines, want at least the %d sleep lines", len(logEvents), seconds)
	}
}

// TestFollowLogsResume covers the second acceptance criterion: a reconnect
// with Last-Event-ID gets the next line and no earlier one.
func TestFollowLogsResume(t *testing.T) {
	fixture := newExecutionFixture(t)
	server := logServer(t, fixture)
	body := `{"capability":"echo","operation":"sleep","arguments":{"seconds":5}}`
	resp, payload := call(t, fixture.handler, fixture.token, http.MethodPost, "/v1/operations", body, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("submit: status %d, body %s", resp.StatusCode, payload)
	}
	view := decodeOperation(t, payload)

	first := getLogs(t, server, fixture.token, "/v1/operations/"+view.ID+"/logs?follow=true", nil)
	logEvents, _ := splitLogEvents(t, readSSE(t, first))
	if len(logEvents) < 4 {
		t.Fatalf("the first stream sent %d lines, want the whole run", len(logEvents))
	}
	resumeAt := logEvents[len(logEvents)/2].ID

	second := getLogs(t, server, fixture.token, "/v1/operations/"+view.ID+"/logs?follow=true",
		map[string]string{"Last-Event-ID": strconv.FormatInt(resumeAt, 10)})
	resumed, status := splitLogEvents(t, readSSE(t, second))
	if len(resumed) == 0 {
		t.Fatal("the resumed stream sent no lines")
	}
	if resumed[0].ID != resumeAt+1 {
		t.Errorf("the resumed stream starts at line %d, want %d", resumed[0].ID, resumeAt+1)
	}
	for i, event := range resumed {
		want := resumeAt + int64(i) + 1
		if event.ID != want {
			t.Errorf("resumed event %d has sequence %d, want %d", i, event.ID, want)
		}
	}
	if total := resumeAt + int64(len(resumed)); total != logEvents[len(logEvents)-1].ID {
		t.Errorf("resume ended at %d, want the last line %d", total, logEvents[len(logEvents)-1].ID)
	}
	var final logStatusView
	if err := json.Unmarshal([]byte(status.Data), &final); err != nil {
		t.Fatalf("decode status event %q: %v", status.Data, err)
	}
	if final.Status != "completed" {
		t.Errorf("status event %+v, want completed", final)
	}
}

// TestLogsAuthorization covers the allow list, an unknown operation, and a
// malformed Last-Event-ID header.
func TestLogsAuthorization(t *testing.T) {
	fixture := newExecutionFixture(t)
	server := logServer(t, fixture)
	body := `{"capability":"echo","operation":"say","arguments":{"message":"hello"}}`
	resp, payload := call(t, fixture.handler, fixture.token, http.MethodPost, "/v1/operations", body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("submit: status %d, body %s", resp.StatusCode, payload)
	}
	view := decodeOperation(t, payload)

	resp = getLogs(t, server, fixture.limited, "/v1/operations/"+view.ID+"/logs", nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("read with a limited token: status %d, want 403", resp.StatusCode)
	}

	resp = getLogs(t, server, fixture.token, "/v1/operations/op_missing/logs", nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("read an unknown operation: status %d, want 404", resp.StatusCode)
	}

	resp = getLogs(t, server, fixture.token, "/v1/operations/"+view.ID+"/logs",
		map[string]string{"Last-Event-ID": "not-a-number"})
	payload = readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed Last-Event-ID: status %d, body %s", resp.StatusCode, payload)
	}
	if got := decodeWireError(t, payload).Code; got != "invalid_request" {
		t.Errorf("malformed Last-Event-ID: code %q, want invalid_request", got)
	}
}

// readBody reads a response body and closes it.
func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	data := new(strings.Builder)
	if _, err := bufio.NewReader(resp.Body).WriteTo(data); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return []byte(data.String())
}

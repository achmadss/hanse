package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	herrors "github.com/achmadss/hanse/internal/errors"
)

// The types and requests in this file mirror internal/api/logs.go and
// docs/architecture.md section 10. The Gateway serves the stored lines as
// JSON, and a followed log as Server-Sent Events whose last event carries
// the operation's final state.

// Stream names of one log line (docs/architecture.md section 10). The
// Gateway serves only these two.
const (
	// StreamStderr is a line the handler wrote to stderr.
	StreamStderr = "stderr"
	// StreamSystem is a Gateway or node agent event.
	StreamSystem = "system"
)

// LogEntry is one stored line of an operation log. Seq is the operation's
// own sequence number, which the Last-Event-ID header carries.
type LogEntry struct {
	Seq    int64     `json:"seq"`
	Time   time.Time `json:"ts"`
	Stream string    `json:"stream"`
	Line   string    `json:"line"`
}

// OperationLogs is the body of GET /v1/operations/{id}/logs.
type OperationLogs struct {
	Logs []LogEntry `json:"logs"`
}

// LogStatus is the data of the final status event of a followed log. A
// failed or cancelled operation carries its recorded error, so a client can
// map the status to the exit code of docs/architecture.md section 14.
type LogStatus struct {
	OperationID string          `json:"operation_id"`
	Status      string          `json:"status"`
	Error       *OperationError `json:"error,omitempty"`
}

// Event names of a followed log (docs/architecture.md section 10).
const (
	// EventLog carries one stored line.
	EventLog = "log"
	// EventStatus is the last event of the stream: the operation ended.
	EventStatus = "status"
)

// LogEvent is one event of a followed log stream. When Event is EventLog,
// Entry holds the line. When Event is EventStatus, Status holds the final
// state. An event that this client does not know is dropped, so a newer
// Gateway can add one.
type LogEvent struct {
	Event  string
	ID     int64
	Entry  LogEntry
	Status LogStatus
}

// The timings of a reconnect. A dropped stream resumes with the
// Last-Event-ID header, and the wait between attempts starts short and backs
// off. A client gives up once no attempt has succeeded within retryWindow,
// so a Gateway that stays down produces one clear error instead of a wait
// that never ends.
const (
	defaultFollowBackoff = 250 * time.Millisecond
	defaultMaxBackoff    = 5 * time.Second
	defaultRetryWindow   = 5 * time.Minute
)

// followTuning holds the timings of FollowLogs. New sets the defaults; a
// test replaces it to keep a reconnect test fast.
type followTuning struct {
	initialBackoff time.Duration
	maxBackoff     time.Duration
	retryWindow    time.Duration
	now            func() time.Time
}

// defaultFollowTuning returns the tuning of a new client.
func defaultFollowTuning() followTuning {
	return followTuning{
		initialBackoff: defaultFollowBackoff,
		maxBackoff:     defaultMaxBackoff,
		retryWindow:    defaultRetryWindow,
		now:            time.Now,
	}
}

// Logs returns GET /v1/operations/{id}/logs: the stored lines after a
// sequence number, which zero means from the start. It does not follow.
func (c *Client) Logs(ctx context.Context, operationID string, after int64) (OperationLogs, error) {
	header := http.Header{}
	if after > 0 {
		header.Set("Last-Event-ID", strconv.FormatInt(after, 10))
	}
	var out OperationLogs
	err := c.doWithHeader(ctx, http.MethodGet, &out, nil, nil, header, "v1", "operations", operationID, "logs")
	normalize(&out.Logs)
	return out, err
}

// The query values that select a log read (docs/architecture.md section
// 10). follow keeps the stream open until the operation ends; stream sends
// the stored lines and closes at once.
const (
	logsQueryFollow = "follow"
	logsQueryStored = "stream"
)

// FollowLogs streams GET /v1/operations/{id}/logs?follow=true and calls
// handle for every event, in sequence order and with no duplicate and no
// gap. It returns when the operation ends (the status event) or when ctx is
// cancelled. A dropped connection resumes with the Last-Event-ID header
// after a short backoff; a response that retrying cannot fix, such as an
// unknown operation or a rejected token, returns at once. Errors from handle
// stop the stream, so a failed write is not retried.
func (c *Client) FollowLogs(ctx context.Context, operationID string, after int64, handle func(LogEvent) error) error {
	return c.logStream(ctx, operationID, after, logsQueryFollow, handle)
}

// StreamLogs reads the stored lines of one operation through
// GET /v1/operations/{id}/logs?stream=true and calls handle for every line.
// The Gateway sends the lines as Server-Sent Events and closes the stream
// after a status event, so a log larger than the JSON response cap arrives
// whole and a running operation is not waited for. A reconnect after a
// dropped connection resumes with the Last-Event-ID header, as a followed
// log does.
func (c *Client) StreamLogs(ctx context.Context, operationID string, after int64, handle func(LogEvent) error) error {
	return c.logStream(ctx, operationID, after, logsQueryStored, handle)
}

// logStream runs one log read in the given query form.
func (c *Client) logStream(ctx context.Context, operationID string, after int64, mode string, handle func(LogEvent) error) error {
	if handle == nil {
		handle = func(LogEvent) error { return nil }
	}
	tuning := c.follow
	if tuning.now == nil {
		tuning = defaultFollowTuning()
	}
	delay := tuning.initialBackoff
	last := after
	var giveUp time.Time
	var lastErr error
	for {
		out, err := c.logStreamOnce(ctx, operationID, last, mode, handle)
		last = out.last
		if err != nil {
			var fatal *fatalFollow
			if errors.As(err, &fatal) {
				return fatal.err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !retryableFollowError(err) {
				return err
			}
			lastErr = err
		}
		if out.done {
			return nil
		}
		now := tuning.now()
		switch {
		case out.connected && out.healthy:
			// A stream that delivered something is a successful
			// reconnect: the window and the backoff start over.
			giveUp = now.Add(tuning.retryWindow)
			delay = tuning.initialBackoff
		case giveUp.IsZero():
			giveUp = now.Add(tuning.retryWindow)
		}
		if !now.Before(giveUp) {
			message := fmt.Sprintf("the log stream of operation %s stopped and did not reconnect within %s",
				operationID, tuning.retryWindow)
			if lastErr != nil {
				message += ": " + lastErr.Error()
			}
			return herrors.New(herrors.Internal, message, nil)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		delay = nextFollowDelay(delay, tuning.maxBackoff)
	}
}

// followOutcome reports what one connection of a log read did. connected
// means the Gateway accepted the request; healthy means the stream carried
// at least one line, which a keepalive comment alone already does.
type followOutcome struct {
	connected bool
	healthy   bool
	done      bool
	last      int64
}

// logStreamOnce runs one connection of a log read. It resumes after
// out.last, and it reports the newest sequence number it saw so a retry
// never repeats a line.
func (c *Client) logStreamOnce(ctx context.Context, operationID string, after int64, mode string, handle func(LogEvent) error) (followOutcome, error) {
	out := followOutcome{last: after}
	endpoint, err := c.endpoint([]string{"v1", "operations", operationID, "logs"})
	if err != nil {
		return out, fatalFollowOf(err)
	}
	query := url.Values{mode: {"true"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return out, fatalFollowOf(herrors.New(herrors.Internal,
			"the log request could not be built: "+err.Error(), nil))
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.token)
	if out.last > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(out.last, 10))
	}
	// A followed log stays open until the operation ends, so the request
	// timeout of the regular client must not apply. The copy keeps the
	// transport, the TLS setup, and the proxy settings.
	stream := *c.http
	stream.Timeout = 0
	resp, err := stream.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		return out, herrors.New(herrors.Internal,
			fmt.Sprintf("cannot reach the Gateway at %s: %v", c.baseURL.Redacted(), err), nil)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		return out, errorFromResponse(resp.StatusCode, body)
	}
	if contentType := resp.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		// A Gateway that ignores the query would answer with JSON, so a
		// caller must get one clear error instead of a five-minute wait.
		return out, fatalFollowOf(herrors.New(herrors.Internal,
			fmt.Sprintf("the Gateway at %s did not stream the log (content type %q)",
				c.baseURL.Redacted(), contentType), nil))
	}
	out.connected = true

	lines, err := readSSE(resp.Body, func(event sseEvent) error {
		id := out.last
		if event.ID != "" {
			if parsed, parseErr := strconv.ParseInt(strings.TrimSpace(event.ID), 10, 64); parseErr == nil && parsed > 0 {
				id = parsed
			}
		}
		switch event.Event {
		case EventLog:
			var entry LogEntry
			if decodeErr := decodeSSEData(event.Data, &entry); decodeErr != nil {
				return fatalFollowOf(herrors.New(herrors.Internal,
					"the Gateway sent an invalid log event: "+decodeErr.Error(), nil))
			}
			if entry.Seq <= out.last {
				// The Gateway resends the line that Last-Event-ID
				// names, and a resume may overlap; neither may show
				// a line twice.
				return nil
			}
			if entry.Seq > id {
				id = entry.Seq
			}
			out.last = id
			if entry.Seq == 0 {
				entry.Seq = id
			}
			if handleErr := handle(LogEvent{Event: EventLog, ID: id, Entry: entry}); handleErr != nil {
				return fatalFollowOf(handleErr)
			}
		case EventStatus:
			var status LogStatus
			if decodeErr := decodeSSEData(event.Data, &status); decodeErr != nil {
				return fatalFollowOf(herrors.New(herrors.Internal,
					"the Gateway sent an invalid status event: "+decodeErr.Error(), nil))
			}
			out.done = true
			if handleErr := handle(LogEvent{Event: EventStatus, ID: id, Status: status}); handleErr != nil {
				return fatalFollowOf(handleErr)
			}
			return errStreamEnded
		}
		return nil
	})
	if lines > 0 {
		out.healthy = true
	}
	if errors.Is(err, errStreamEnded) {
		return out, nil
	}
	if err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		return out, err
	}
	// The stream closed without a status event, so the operation is most
	// likely still running and the caller reconnects.
	return out, nil
}

// retryableFollowError reports whether a failed attempt is worth another
// one. A client error (HTTP 4xx) such as an unknown operation or a rejected
// token fails the same way every time.
func retryableFollowError(err error) bool {
	var apiErr *herrors.Error
	if !errors.As(err, &apiErr) {
		return true
	}
	return apiErr.Code().HTTPStatus() >= 500
}

// nextFollowDelay doubles a wait up to a limit.
func nextFollowDelay(delay, limit time.Duration) time.Duration {
	next := delay * 2
	if next > limit {
		next = limit
	}
	return next
}

// errStreamEnded stops the SSE reader after the status event.
var errStreamEnded = errors.New("the followed log ended")

// fatalFollow marks an error that a reconnect cannot fix, such as a rejected
// request, an event that is not valid JSON, or a failed write by the caller.
type fatalFollow struct{ err error }

// Error implements the error interface.
func (e *fatalFollow) Error() string { return e.err.Error() }

// Unwrap exposes the cause to errors.Is and errors.As.
func (e *fatalFollow) Unwrap() error { return e.err }

// fatalFollowOf wraps err as a failure that stops the stream.
func fatalFollowOf(err error) error { return &fatalFollow{err: err} }

// sseEvent is one parsed Server-Sent Event. Event is empty when the stream
// set none, which the SSE specification calls "message".
type sseEvent struct {
	ID    string
	Event string
	Data  string
}

// readSSE parses a Server-Sent Events stream and calls emit for each event
// whose data is not empty. It follows the specification's line rules: a
// comment line such as the keepalive starts with a colon and is dropped, a
// field value loses one leading space, several data lines join with a
// newline, and CRLF and LF both end a line. It returns the number of lines
// it read, so a caller can tell a quiet stream from a dead one.
func readSSE(r io.Reader, emit func(sseEvent) error) (int, error) {
	reader := bufio.NewReaderSize(r, 64<<10)
	var event sseEvent
	var data strings.Builder
	lines := 0
	flush := func() error {
		if data.Len() == 0 {
			event = sseEvent{}
			return nil
		}
		event.Data = strings.TrimSuffix(data.String(), "\n")
		data.Reset()
		pending := event
		event = sseEvent{}
		if pending.Event == "" {
			pending.Event = "message"
		}
		return emit(pending)
	}
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			lines++
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")
			switch {
			case line == "":
				if flushErr := flush(); flushErr != nil {
					return lines, flushErr
				}
			case strings.HasPrefix(line, ":"):
				// A comment, such as ": keepalive", carries no data.
			default:
				field, value := line, ""
				if index := strings.IndexByte(line, ':'); index >= 0 {
					field, value = line[:index], line[index+1:]
					value = strings.TrimPrefix(value, " ")
				}
				switch field {
				case "data":
					data.WriteString(value)
					data.WriteByte('\n')
				case "event":
					event.Event = value
				case "id":
					event.ID = value
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				// An event that the stream cut off is incomplete, and
				// the specification discards it.
				return lines, nil
			}
			return lines, err
		}
	}
}

// decodeSSEData decodes the JSON object of one event.
func decodeSSEData(data string, out any) error {
	dec := json.NewDecoder(strings.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the JSON object")
	}
	return nil
}

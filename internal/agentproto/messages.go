package agentproto

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// The protocol identity. The full version appears in message fields; the
// subprotocol name appears in Sec-WebSocket-Protocol, which cannot contain
// a slash (docs/agent-protocol.md section 12).
const (
	// ProtocolV1 is the full protocol version of this package.
	ProtocolV1 = "hanse.agent/v1"
	// SubprotocolV1 is the WebSocket subprotocol name of ProtocolV1.
	SubprotocolV1 = "hanse.agent.v1"
	// MaxMessageBytes is the largest accepted WebSocket message: 4 MiB
	// (docs/agent-protocol.md section 11).
	MaxMessageBytes = 4 << 20
	// HelloTimeout is how long the hello and welcome exchange may take
	// (docs/agent-protocol.md section 11).
	HelloTimeout = 10 * time.Second
)

// Message is one protocol message. Decode returns a concrete message type,
// and Encode writes it back as one JSON object.
type Message interface {
	// MessageType is the value of the `type` field.
	MessageType() string
	// Validate checks the fields the protocol requires. A message that a
	// peer receives must pass this check before it is handled.
	Validate() error
	// sealed keeps the message set closed to this package.
	sealed()
}

// Run states that a hello message may report
// (docs/agent-protocol.md section 4.1).
const (
	RunStateRunning  = "running"
	RunStateFinished = "finished"
	RunStateLost     = "lost"
)

// Reconnect actions that a welcome message may set
// (docs/agent-protocol.md section 8.2).
const (
	ActionResume  = "resume"
	ActionAbandon = "abandon"
)

// Log streams of a log message (docs/agent-protocol.md section 4.6).
const (
	StreamStderr = "stderr"
	StreamSystem = "system"
)

// Stop reasons of a result message (docs/agent-protocol.md section 4.7).
const (
	StopCancel         = "cancel"
	StopDeadline       = "deadline"
	StopOutputTooLarge = "output_too_large"
	StopAbandon        = "abandon"
)

// Output errors of a result message (docs/agent-protocol.md section 4.7).
const (
	OutputMissing  = "missing"
	OutputInvalid  = "invalid"
	OutputTooLarge = "too_large"
)

// helloMessage, welcomeMessage and so on are the `type` values. The
// exported constants are the same values, for code that builds a message.
const (
	typeHello     = "hello"
	typeWelcome   = "welcome"
	typeRun       = "run"
	typeStarted   = "started"
	typeRejected  = "rejected"
	typeLog       = "log"
	typeResult    = "result"
	typeAck       = "ack"
	typeCancel    = "cancel"
	typeHeartbeat = "heartbeat"
	typeError     = "error"

	// TypeHello is the `type` of a hello message.
	TypeHello = typeHello
	// TypeWelcome is the `type` of a welcome message.
	TypeWelcome = typeWelcome
	// TypeRun is the `type` of a run message.
	TypeRun = typeRun
	// TypeStarted is the `type` of a started message.
	TypeStarted = typeStarted
	// TypeRejected is the `type` of a rejected message.
	TypeRejected = typeRejected
	// TypeLog is the `type` of a log message.
	TypeLog = typeLog
	// TypeResult is the `type` of a result message.
	TypeResult = typeResult
	// TypeAck is the `type` of an ack message.
	TypeAck = typeAck
	// TypeCancel is the `type` of a cancel message.
	TypeCancel = typeCancel
	// TypeHeartbeat is the `type` of a heartbeat message.
	TypeHeartbeat = typeHeartbeat
	// TypeError is the `type` of an error message.
	TypeError = typeError
)

// TimestampLayout is the protocol's timestamp format: RFC 3339 in UTC with
// milliseconds (docs/agent-protocol.md section 4).
const TimestampLayout = "2006-01-02T15:04:05.000Z"

// Timestamp renders t in the protocol's format.
func Timestamp(t time.Time) string { return t.UTC().Format(TimestampLayout) }

// parseTimestamp checks one timestamp field and returns its instant.
func parseTimestamp(value string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("timestamp %q is not RFC 3339", value)
	}
	return t.UTC(), nil
}

// Hello is the agent's first message. It introduces the agent and lists
// every run whose outcome the agent still holds
// (docs/agent-protocol.md section 4.1).
type Hello struct {
	Type              string     `json:"type"`
	Protocol          string     `json:"protocol"`
	AgentVersion      string     `json:"agent_version"`
	OS                string     `json:"os"`
	Arch              string     `json:"arch"`
	Contracts         []string   `json:"contracts"`
	MaxConcurrentRuns int        `json:"max_concurrent_runs"`
	Runs              []RunState `json:"runs"`
}

// RunState is one run entry of hello. LastSeq is the highest sequence
// number the agent produced for the run, or 0 for a lost run.
type RunState struct {
	RunID   string `json:"run_id"`
	State   string `json:"state"`
	LastSeq int64  `json:"last_seq"`
}

// Welcome accepts the agent, sets the heartbeat timing, and decides what
// happens to each run of hello (docs/agent-protocol.md section 4.2).
type Welcome struct {
	Type              string       `json:"type"`
	Node              string       `json:"node"`
	GatewayVersion    string       `json:"gateway_version"`
	HeartbeatInterval int          `json:"heartbeat_interval"`
	HeartbeatTimeout  int          `json:"heartbeat_timeout"`
	Runs              []WelcomeRun `json:"runs"`
}

// WelcomeRun is the Gateway's decision about one run of hello. AckedSeq is
// present exactly for a resume.
type WelcomeRun struct {
	RunID    string `json:"run_id"`
	Action   string `json:"action"`
	AckedSeq *int64 `json:"acked_seq,omitempty"`
}

// Run starts one handler on the agent (docs/agent-protocol.md section 4.3).
type Run struct {
	Type        string          `json:"type"`
	RunID       string          `json:"run_id"`
	Bundle      string          `json:"bundle"`
	Entry       []string        `json:"entry"`
	Request     json.RawMessage `json:"request"`
	Privileged  bool            `json:"privileged"`
	Elevation   *Elevation      `json:"elevation,omitempty"`
	GracePeriod int64           `json:"grace_period_ms"`
	Limits      Limits          `json:"limits"`
}

// Elevation is the transient elevation input of a privileged run. It is
// never stored (docs/agent-protocol.md section 9).
type Elevation struct {
	Password string `json:"password"`
}

// Limits are the handler contract limits the agent enforces.
type Limits struct {
	ResponseBytes int `json:"response_bytes"`
	LogLineBytes  int `json:"log_line_bytes"`
}

// Started reports that the handler process exists and received its
// request (docs/agent-protocol.md section 4.4).
type Started struct {
	Type  string `json:"type"`
	RunID string `json:"run_id"`
	TS    string `json:"ts"`
}

// Rejected reports that the handler did not start
// (docs/agent-protocol.md section 4.5).
type Rejected struct {
	Type    string `json:"type"`
	RunID   string `json:"run_id"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Log is one log line of a run (docs/agent-protocol.md section 4.6).
type Log struct {
	Type   string `json:"type"`
	RunID  string `json:"run_id"`
	Seq    int64  `json:"seq"`
	TS     string `json:"ts"`
	Stream string `json:"stream"`
	Line   string `json:"line"`
}

// Result is the final message of a run (docs/agent-protocol.md section
// 4.7). Response is the response envelope the handler wrote, or nil.
type Result struct {
	Type        string          `json:"type"`
	RunID       string          `json:"run_id"`
	Seq         int64           `json:"seq"`
	TS          string          `json:"ts"`
	ExitCode    *int            `json:"exit_code"`
	Signal      *string         `json:"signal"`
	StopReason  *string         `json:"stop_reason"`
	Response    json.RawMessage `json:"response"`
	OutputError *string         `json:"output_error"`
	DurationMS  int64           `json:"duration_ms"`
}

// Ack reports that the Gateway stored every message of a run up to and
// including Seq (docs/agent-protocol.md section 4.8).
type Ack struct {
	Type  string `json:"type"`
	RunID string `json:"run_id"`
	Seq   int64  `json:"seq"`
}

// Cancel asks the agent to stop a handler
// (docs/agent-protocol.md section 4.9).
type Cancel struct {
	Type  string `json:"type"`
	RunID string `json:"run_id"`
}

// Heartbeat keeps the connection alive
// (docs/agent-protocol.md section 4.10).
type Heartbeat struct {
	Type string `json:"type"`
	TS   string `json:"ts"`
}

// Error reports a protocol problem (docs/agent-protocol.md section 4.11).
type Error struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
	RunID   string `json:"run_id,omitempty"`
}

func (*Hello) sealed()     {}
func (*Welcome) sealed()   {}
func (*Run) sealed()       {}
func (*Started) sealed()   {}
func (*Rejected) sealed()  {}
func (*Log) sealed()       {}
func (*Result) sealed()    {}
func (*Ack) sealed()       {}
func (*Cancel) sealed()    {}
func (*Heartbeat) sealed() {}
func (*Error) sealed()     {}

// MessageType returns the message's `type` value.
func (*Hello) MessageType() string { return typeHello }

// MessageType returns the message's `type` value.
func (*Welcome) MessageType() string { return typeWelcome }

// MessageType returns the message's `type` value.
func (*Run) MessageType() string { return typeRun }

// MessageType returns the message's `type` value.
func (*Started) MessageType() string { return typeStarted }

// MessageType returns the message's `type` value.
func (*Rejected) MessageType() string { return typeRejected }

// MessageType returns the message's `type` value.
func (*Log) MessageType() string { return typeLog }

// MessageType returns the message's `type` value.
func (*Result) MessageType() string { return typeResult }

// MessageType returns the message's `type` value.
func (*Ack) MessageType() string { return typeAck }

// MessageType returns the message's `type` value.
func (*Cancel) MessageType() string { return typeCancel }

// MessageType returns the message's `type` value.
func (*Heartbeat) MessageType() string { return typeHeartbeat }

// MessageType returns the message's `type` value.
func (*Error) MessageType() string { return typeError }

// UnknownMessageError reports a `type` this protocol version does not
// know. The receiver answers an `error` message with code unknown_message
// and keeps the connection open (docs/agent-protocol.md section 10).
type UnknownMessageError struct {
	Type string
}

func (e *UnknownMessageError) Error() string {
	return fmt.Sprintf("unknown message type %q", e.Type)
}

// DecodeError reports a frame that is not a valid message of this
// protocol version. The connection must close with 4000.
type DecodeError struct {
	Err error
}

// Error implements the error interface.
func (e *DecodeError) Error() string { return e.Err.Error() }

// Unwrap returns the underlying problem.
func (e *DecodeError) Unwrap() error { return e.Err }

// Decode reads one message frame. A message over MaxMessageBytes, a frame
// that is not one JSON object, or a message with invalid fields returns a
// *DecodeError; an unknown type returns *UnknownMessageError.
func Decode(data []byte) (Message, error) {
	if len(data) > MaxMessageBytes {
		return nil, &DecodeError{Err: fmt.Errorf("message of %d bytes is over the %d-byte limit", len(data), MaxMessageBytes)}
	}
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, &DecodeError{Err: fmt.Errorf("message is not one JSON object: %w", err)}
	}
	if head.Type == "" {
		return nil, &DecodeError{Err: fmt.Errorf("message has no type")}
	}
	var msg Message
	switch head.Type {
	case typeHello:
		msg = &Hello{}
	case typeWelcome:
		msg = &Welcome{}
	case typeRun:
		msg = &Run{}
	case typeStarted:
		msg = &Started{}
	case typeRejected:
		msg = &Rejected{}
	case typeLog:
		msg = &Log{}
	case typeResult:
		msg = &Result{}
	case typeAck:
		msg = &Ack{}
	case typeCancel:
		msg = &Cancel{}
	case typeHeartbeat:
		msg = &Heartbeat{}
	case typeError:
		msg = &Error{}
	default:
		return nil, &UnknownMessageError{Type: head.Type}
	}
	if err := json.Unmarshal(data, msg); err != nil {
		return nil, &DecodeError{Err: fmt.Errorf("message %q is malformed: %w", head.Type, err)}
	}
	if err := msg.Validate(); err != nil {
		return nil, &DecodeError{Err: fmt.Errorf("message %q is invalid: %w", head.Type, err)}
	}
	return msg, nil
}

// Encode writes one message as one JSON object. The message must pass
// Validate, so the Gateway never sends a message the protocol forbids.
func Encode(msg Message) ([]byte, error) {
	if msg == nil {
		return nil, fmt.Errorf("cannot encode a nil message")
	}
	if err := msg.Validate(); err != nil {
		return nil, fmt.Errorf("cannot encode an invalid %q message: %w", msg.MessageType(), err)
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("cannot encode a %q message: %w", msg.MessageType(), err)
	}
	if len(data) > MaxMessageBytes {
		return nil, fmt.Errorf("a %q message of %d bytes is over the %d-byte limit",
			msg.MessageType(), len(data), MaxMessageBytes)
	}
	return data, nil
}

// Validate checks the hello fields.
func (m *Hello) Validate() error {
	if m.Type != typeHello {
		return fmt.Errorf("type %q, want %q", m.Type, typeHello)
	}
	for name, value := range map[string]string{
		"protocol": m.Protocol, "agent_version": m.AgentVersion, "os": m.OS, "arch": m.Arch,
	} {
		if value == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if !protocolPattern.MatchString(m.Protocol) {
		return fmt.Errorf("protocol %q is not hanse.agent/v<major>", m.Protocol)
	}
	if len(m.Contracts) == 0 {
		return fmt.Errorf("contracts must list at least one handler contract")
	}
	for _, contract := range m.Contracts {
		if contract == "" {
			return fmt.Errorf("contracts holds an empty value")
		}
	}
	if m.MaxConcurrentRuns < 1 {
		return fmt.Errorf("max_concurrent_runs must be at least 1")
	}
	for _, state := range m.Runs {
		if state.RunID == "" {
			return fmt.Errorf("runs holds an entry without run_id")
		}
		switch state.State {
		case RunStateRunning, RunStateFinished, RunStateLost:
		default:
			return fmt.Errorf("run %q has state %q, want running, finished, or lost", state.RunID, state.State)
		}
		if state.LastSeq < 0 {
			return fmt.Errorf("run %q has a negative last_seq", state.RunID)
		}
	}
	return nil
}

// Validate checks the welcome fields.
func (m *Welcome) Validate() error {
	if m.Type != typeWelcome {
		return fmt.Errorf("type %q, want %q", m.Type, typeWelcome)
	}
	if m.Node == "" {
		return fmt.Errorf("node is required")
	}
	if m.GatewayVersion == "" {
		return fmt.Errorf("gateway_version is required")
	}
	if m.HeartbeatInterval < 1 || m.HeartbeatTimeout < 1 {
		return fmt.Errorf("heartbeat_interval and heartbeat_timeout must be at least 1")
	}
	for _, state := range m.Runs {
		if state.RunID == "" {
			return fmt.Errorf("runs holds an entry without run_id")
		}
		switch state.Action {
		case ActionResume:
			if state.AckedSeq == nil {
				return fmt.Errorf("run %q resumes without acked_seq", state.RunID)
			}
			if *state.AckedSeq < 0 {
				return fmt.Errorf("run %q has a negative acked_seq", state.RunID)
			}
		case ActionAbandon:
			if state.AckedSeq != nil {
				return fmt.Errorf("run %q abandons with acked_seq", state.RunID)
			}
		default:
			return fmt.Errorf("run %q has action %q, want resume or abandon", state.RunID, state.Action)
		}
	}
	return nil
}

// Validate checks the run fields.
func (m *Run) Validate() error {
	if m.Type != typeRun {
		return fmt.Errorf("type %q, want %q", m.Type, typeRun)
	}
	if m.RunID == "" {
		return fmt.Errorf("run_id is required")
	}
	if !bundlePattern.MatchString(m.Bundle) {
		return fmt.Errorf("bundle %q is not sha256:<hex>", m.Bundle)
	}
	if len(m.Entry) == 0 {
		return fmt.Errorf("entry must hold at least one argument")
	}
	for _, arg := range m.Entry {
		if arg == "" {
			return fmt.Errorf("entry holds an empty argument")
		}
	}
	if !isJSONObject(m.Request) {
		return fmt.Errorf("request must be one JSON object")
	}
	if m.Privileged && m.Elevation == nil {
		return fmt.Errorf("a privileged run needs elevation")
	}
	if !m.Privileged && m.Elevation != nil {
		return fmt.Errorf("a run that is not privileged must not hold elevation")
	}
	if m.GracePeriod < 0 {
		return fmt.Errorf("grace_period_ms must not be negative")
	}
	if m.Limits.ResponseBytes < 1 || m.Limits.LogLineBytes < 1 {
		return fmt.Errorf("limits must be positive")
	}
	return nil
}

// Validate checks the started fields.
func (m *Started) Validate() error {
	if m.Type != typeStarted {
		return fmt.Errorf("type %q, want %q", m.Type, typeStarted)
	}
	if m.RunID == "" {
		return fmt.Errorf("run_id is required")
	}
	if _, err := parseTimestamp(m.TS); err != nil {
		return err
	}
	return nil
}

// Validate checks the rejected fields.
func (m *Rejected) Validate() error {
	if m.Type != typeRejected {
		return fmt.Errorf("type %q, want %q", m.Type, typeRejected)
	}
	if m.RunID == "" {
		return fmt.Errorf("run_id is required")
	}
	if !wireNamePattern.MatchString(m.Code) {
		return fmt.Errorf("code %q is not snake_case", m.Code)
	}
	return nil
}

// Validate checks the log fields.
func (m *Log) Validate() error {
	if m.Type != typeLog {
		return fmt.Errorf("type %q, want %q", m.Type, typeLog)
	}
	if m.RunID == "" {
		return fmt.Errorf("run_id is required")
	}
	if m.Seq < 1 {
		return fmt.Errorf("seq must be at least 1")
	}
	if _, err := parseTimestamp(m.TS); err != nil {
		return err
	}
	if m.Stream != StreamStderr && m.Stream != StreamSystem {
		return fmt.Errorf("stream %q, want stderr or system", m.Stream)
	}
	return nil
}

// Validate checks the result fields.
func (m *Result) Validate() error {
	if m.Type != typeResult {
		return fmt.Errorf("type %q, want %q", m.Type, typeResult)
	}
	if m.RunID == "" {
		return fmt.Errorf("run_id is required")
	}
	if m.Seq < 1 {
		return fmt.Errorf("seq must be at least 1")
	}
	if _, err := parseTimestamp(m.TS); err != nil {
		return err
	}
	if m.StopReason != nil {
		switch *m.StopReason {
		case StopCancel, StopDeadline, StopOutputTooLarge, StopAbandon:
		default:
			return fmt.Errorf("stop_reason %q is not a documented reason", *m.StopReason)
		}
	}
	if m.Response != nil && !isJSONObject(m.Response) && !isJSONNull(m.Response) {
		return fmt.Errorf("response must be one JSON object or null")
	}
	if m.OutputError != nil {
		switch *m.OutputError {
		case OutputMissing, OutputInvalid, OutputTooLarge:
		default:
			return fmt.Errorf("output_error %q is not a documented value", *m.OutputError)
		}
	}
	if m.DurationMS < 0 {
		return fmt.Errorf("duration_ms must not be negative")
	}
	return nil
}

// Validate checks the ack fields.
func (m *Ack) Validate() error {
	if m.Type != typeAck {
		return fmt.Errorf("type %q, want %q", m.Type, typeAck)
	}
	if m.RunID == "" {
		return fmt.Errorf("run_id is required")
	}
	if m.Seq < 1 {
		return fmt.Errorf("seq must be at least 1")
	}
	return nil
}

// Validate checks the cancel fields.
func (m *Cancel) Validate() error {
	if m.Type != typeCancel {
		return fmt.Errorf("type %q, want %q", m.Type, typeCancel)
	}
	if m.RunID == "" {
		return fmt.Errorf("run_id is required")
	}
	return nil
}

// Validate checks the heartbeat fields.
func (m *Heartbeat) Validate() error {
	if m.Type != typeHeartbeat {
		return fmt.Errorf("type %q, want %q", m.Type, typeHeartbeat)
	}
	if _, err := parseTimestamp(m.TS); err != nil {
		return err
	}
	return nil
}

// Validate checks the error fields.
func (m *Error) Validate() error {
	if m.Type != typeError {
		return fmt.Errorf("type %q, want %q", m.Type, typeError)
	}
	if !wireNamePattern.MatchString(m.Code) {
		return fmt.Errorf("code %q is not snake_case", m.Code)
	}
	if m.Message == "" {
		return fmt.Errorf("message is required")
	}
	return nil
}

// isJSONObject reports whether raw is one JSON object.
func isJSONObject(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return strings.HasPrefix(trimmed, "{") && json.Valid(raw)
}

// isJSONNull reports whether raw is the JSON null value.
func isJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

var (
	protocolPattern = regexp.MustCompile(`^hanse\.agent/v[0-9]+$`)
	bundlePattern   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	wireNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

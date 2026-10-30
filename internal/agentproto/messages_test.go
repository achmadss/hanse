package agentproto

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// nowForTest is the fixed timestamp of the tests.
func nowForTest() time.Time {
	return time.Date(2026, 10, 30, 8, 10, 45, 0, time.UTC)
}

// TestEncodeDecodeRoundTrip covers every message type.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	exit := 0
	seq := int64(1)
	messages := []Message{
		&Hello{
			Type: TypeHello, Protocol: ProtocolV1, AgentVersion: "1.0.0",
			OS: "linux", Arch: "arm64", Contracts: []string{"hanse.handler/v1"},
			MaxConcurrentRuns: 4, Runs: []RunState{{RunID: "op_1", State: RunStateRunning, LastSeq: 2}},
		},
		&Welcome{
			Type: TypeWelcome, Node: "node-a", GatewayVersion: "1.0.0",
			HeartbeatInterval: 15, HeartbeatTimeout: 45,
			Runs: []WelcomeRun{{RunID: "op_1", Action: ActionResume, AckedSeq: &seq}},
		},
		&Run{
			Type: TypeRun, RunID: "op_1", Bundle: "sha256:" + strings.Repeat("a", 64),
			Entry: []string{"sh", "handler.sh"}, Request: json.RawMessage(`{"contract":"hanse.handler/v1"}`),
			GracePeriod: 10000, Limits: Limits{ResponseBytes: 1, LogLineBytes: 1},
		},
		&Started{Type: TypeStarted, RunID: "op_1", TS: Timestamp(nowForTest())},
		&Rejected{Type: TypeRejected, RunID: "op_1", Code: "agent_busy", Message: "busy"},
		&Log{Type: TypeLog, RunID: "op_1", Seq: 1, TS: Timestamp(nowForTest()), Stream: StreamStderr, Line: "line"},
		&Result{
			Type: TypeResult, RunID: "op_1", Seq: 2, TS: Timestamp(nowForTest()),
			ExitCode: &exit, Response: json.RawMessage(`{"status":"ok","result":{}}`), DurationMS: 10,
		},
		&Ack{Type: TypeAck, RunID: "op_1", Seq: 2},
		&Cancel{Type: TypeCancel, RunID: "op_1"},
		&Heartbeat{Type: TypeHeartbeat, TS: Timestamp(nowForTest())},
		&Error{Type: TypeError, Code: "unknown_message", Message: "unknown message type: progress"},
	}
	for _, msg := range messages {
		t.Run(msg.MessageType(), func(t *testing.T) {
			data, err := Encode(msg)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			decoded, err := Decode(data)
			if err != nil {
				t.Fatalf("decode %s: %v", data, err)
			}
			if decoded.MessageType() != msg.MessageType() {
				t.Errorf("decoded %q, want %q", decoded.MessageType(), msg.MessageType())
			}
		})
	}
}

// TestDecodeUnknownType reports an unknown type so the receiver can answer
// unknown_message and keep the connection open.
func TestDecodeUnknownType(t *testing.T) {
	_, err := Decode([]byte(`{"type":"progress"}`))
	var unknown *UnknownMessageError
	if !errors.As(err, &unknown) {
		t.Fatalf("error %v, want UnknownMessageError", err)
	}
	if unknown.Type != "progress" {
		t.Errorf("unknown type %q, want progress", unknown.Type)
	}
}

// TestEncodeRejectsInvalidMessage refuses to send a malformed message.
func TestEncodeRejectsInvalidMessage(t *testing.T) {
	cases := []Message{
		&Hello{Type: TypeHello, Protocol: "hanse.agent/v9", AgentVersion: "1", OS: "linux", Arch: "amd64", Contracts: []string{"x"}, MaxConcurrentRuns: 0},
		&Run{Type: TypeRun, RunID: "op_1", Bundle: "sha256:abc", Entry: []string{"sh"}, Request: json.RawMessage(`{}`), Limits: Limits{ResponseBytes: 1, LogLineBytes: 1}},
		&Log{Type: TypeLog, RunID: "op_1", Seq: 1, TS: Timestamp(nowForTest()), Stream: "stdout", Line: "x"},
	}
	for _, msg := range cases {
		if _, err := Encode(msg); err == nil {
			t.Errorf("encode %T with invalid fields succeeded", msg)
		}
	}
}

// TestDecodeIgnoresUnknownFields keeps a later v1 release compatible
// (docs/agent-protocol.md section 12).
func TestDecodeIgnoresUnknownFields(t *testing.T) {
	data := []byte(`{"type":"heartbeat","ts":"2026-10-30T08:10:45.000Z","future":true}`)
	if _, err := Decode(data); err != nil {
		t.Fatalf("decode with an unknown field: %v", err)
	}
}

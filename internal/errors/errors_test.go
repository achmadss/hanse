package herrors

import (
	"encoding/json"
	stderrors "errors"
	"fmt"
	"strings"
	"testing"
)

func TestAllCodesHaveStableProperties(t *testing.T) {
	seenName := make(map[string]bool)
	seenExit := make(map[int]bool)
	for _, c := range All() {
		name := c.Name()
		if !isWireName(name) {
			t.Errorf("code %q: not a snake_case wire value", name)
		}
		if seenName[name] {
			t.Errorf("duplicate code name %q", name)
		}
		if seenExit[c.ExitCode()] {
			t.Errorf("code %q: duplicate exit code %d", name, c.ExitCode())
		}
		// Documented rule: avoid 0, 2 (usage), and 126+. Only the internal
		// fallback maps to 1, the generic failure exit.
		if e := c.ExitCode(); e == 0 || e == 2 || e >= 126 || (e == 1 && name != "internal") {
			t.Errorf("code %q: exit code %d outside the allowed range", name, e)
		}
		if s := c.HTTPStatus(); s < 200 || s >= 100 && s >= 600 {
			t.Errorf("code %q: implausible HTTP status %d", name, s)
		}
		if c.Message() == "" {
			t.Errorf("code %q: empty human message", name)
		}
		seenName[name] = true
		seenExit[c.ExitCode()] = true
	}
	if len(seenName) != len(All()) || len(seenExit) != len(All()) {
		t.Fatal("uniqueness invariant broken: names or exit codes repeat")
	}
}

// TestArchitectureCodes pins the wire value, HTTP status, and exit code for
// every client-facing code of architecture.md §14.
func TestArchitectureCodes(t *testing.T) {
	tests := []struct {
		code *Code
		name string
		http int
		exit int
	}{
		{UnknownCapability, "unknown_capability", 404, 20},
		{UnknownOperation, "unknown_operation", 404, 21},
		{InvalidArguments, "invalid_arguments", 422, 22},
		{MissingArgument, "missing_argument", 400, 23},
		{InvalidExecutor, "invalid_executor", 404, 24},
		{ExecutorNotEligible, "executor_not_eligible", 422, 25},
		{NoDefaultExecutor, "no_default_executor", 422, 26},
		{ExecutorUnavailable, "executor_unavailable", 503, 27},
		{AuthenticationFailed, "authentication_failed", 401, 28},
		{Forbidden, "forbidden", 403, 29},
		{ElevationRequired, "elevation_required", 403, 30},
		{ElevationDenied, "elevation_denied", 403, 31},
		{OperationConflict, "operation_conflict", 409, 32},
		{HandlerFailed, "handler_failed", 502, 33},
		{Timeout, "timeout", 504, 34},
		{Cancelled, "cancelled", 409, 35},
		{ExecutorConnectionLost, "executor_connection_lost", 502, 36},
		{GatewayRestart, "gateway_restart", 503, 37},
		{HandlerExit, "handler_exit", 502, 38},
		{HandlerOutputInvalid, "handler_output_invalid", 502, 39},
		{HandlerOutputTooLarge, "handler_output_too_large", 502, 49},
		{HandlerHTTPStatus, "http_status", 502, 40},
		{AgentBusy, "agent_busy", 503, 41},
		{BundleUnavailable, "bundle_unavailable", 503, 42},
		{HandlerIntegrity, "handler_integrity", 502, 43},
		{HandlerStartFailed, "handler_start_failed", 502, 44},
		{UnsupportedContract, "unsupported_contract", 422, 45},
		{JoinTokenInvalid, "join_token_invalid", 401, 46},
		{NodeMismatch, "node_mismatch", 409, 47},
		{InvalidRequest, "invalid_request", 400, 48},
		{Internal, "internal", 500, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.code.Name() != tc.name {
				t.Errorf("wire value = %q, want %q", tc.code.Name(), tc.name)
			}
			if tc.code.HTTPStatus() != tc.http {
				t.Errorf("HTTP status = %d, want %d", tc.code.HTTPStatus(), tc.http)
			}
			if tc.code.ExitCode() != tc.exit {
				t.Errorf("exit code = %d, want %d", tc.code.ExitCode(), tc.exit)
			}
		})
	}
}

func TestErrorImplementsErrorInterface(t *testing.T) {
	err := New(UnknownCapability, "capability \"pc\" not found", nil)
	if err.Error() == "" {
		t.Fatal("Error() produced empty string")
	}
	if err.Code() != UnknownCapability {
		t.Errorf("Code() = %s, want unknown_capability", err.Code().Name())
	}

	// Default message when none is given.
	msgOnly := New(Timeout, "", nil)
	if msgOnly.Error() != "timeout: operation timed out" {
		t.Errorf("default message = %q", msgOnly.Error())
	}
}

func TestErrorsIsAndAs(t *testing.T) {
	err := New(ExecutorUnavailable, "agent of node-a is offline", nil)

	// errors.Is with the code as sentinel.
	if !stderrors.Is(err, ExecutorUnavailable) {
		t.Error("errors.Is(err, ExecutorUnavailable) = false, want true")
	}
	if stderrors.Is(err, Timeout) {
		t.Error("errors.Is(err, Timeout) = true, want false")
	}

	// errors.Is with another *Error that carries the same code.
	wrapped := fmt.Errorf("dispatch: %w", New(ExecutorUnavailable, "x", nil))
	if !stderrors.Is(wrapped, err) {
		t.Error("errors.Is(wrapped, err) = false for same code, want true")
	}

	// errors.As unwraps through fmt.Errorf with %w.
	var target *Error
	if !stderrors.As(fmt.Errorf("api: %w", err), &target) {
		t.Fatal("errors.As did not find *Error")
	}
	if target.Code() != ExecutorUnavailable {
		t.Errorf("unwrapped code = %s, want executor_unavailable", target.Code().Name())
	}

	// Plain errors pass through From as internal.
	if got := From(stderrors.New("boom")).Code(); got != Internal {
		t.Errorf("From(non-code).Code() = %s, want internal", got.Name())
	}
}

func TestHTTPStatusAndExitCodeHelpers(t *testing.T) {
	if got := HTTPStatus(New(InvalidArguments, "", nil)); got != 422 {
		t.Errorf("HTTPStatus(invalid_arguments) = %d, want 422", got)
	}
	if got := ExitCode(New(InvalidArguments, "", nil)); got != 22 {
		t.Errorf("ExitCode(invalid_arguments) = %d, want 22", got)
	}
	unknown := stderrors.New("something broke")
	if got := HTTPStatus(unknown); got != 500 {
		t.Errorf("HTTPStatus(unknown) = %d, want 500", got)
	}
	if got := ExitCode(unknown); got != 1 {
		t.Errorf("ExitCode(unknown) = %d, want 1", got)
	}
}

func TestJSONWireShapeAndRoundTrip(t *testing.T) {
	for _, c := range All() {
		e := New(c, "message for "+c.Name(), map[string]any{"k": "v"})
		b, encErr := JSONBytes(e)
		if encErr != nil {
			t.Fatalf("%s: JSONBytes: %v", c.Name(), encErr)
		}
		var got struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		}
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("%s: unmarshal: %v", c.Name(), err)
		}
		if got.Code != c.Name() {
			t.Errorf("%s: wire code = %q, want %q", c.Name(), got.Code, c.Name())
		}
		if got.Message != "message for "+c.Name() {
			t.Errorf("%s: message = %q", c.Name(), got.Message)
		}
		if got.Details["k"] != "v" {
			t.Errorf("%s: details = %v", c.Name(), got.Details)
		}
	}

	// details omitted when empty.
	b, encErr := JSONBytes(New(MissingArgument, "need node", nil))
	if encErr != nil {
		t.Fatal(encErr)
	}
	if string(b) != `{"code":"missing_argument","message":"need node"}` {
		t.Errorf("empty-details JSON = %s", b)
	}
}

func TestRedactionOfValuesAndKeys(t *testing.T) {
	ClearSecrets()
	defer ClearSecrets()

	AddSecretValue("hunter2-password")
	AddSecretValue("e3b0c442")
	AddSecretKey("password")

	err := New(OperationConflict, "conflict while handling hunter2-password", map[string]any{
		"running_operation": "op_01J9Z3",
		"password":          "hunter2-password",
		"nested": map[string]any{
			"secret_note": "uses hunter2-password too",
		},
	})
	b, encErr := JSONBytes(err)
	if encErr != nil {
		t.Fatal(encErr)
	}
	out := string(b)
	for _, banned := range []string{"hunter2-password"} {
		if strings.Contains(out, banned) {
			t.Errorf("encoded error leaks secret: %s", out)
		}
	}
	if !strings.Contains(out, "[redacted]") {
		t.Errorf("encoded error has no [redacted] placeholder: %s", out)
	}
	if !strings.Contains(out, `"running_operation":"op_01J9Z3"`) {
		t.Errorf("non-secret detail lost: %s", out)
	}

	// Redact works on explicit values without a registry.
	e := Redact(stderrors.New("token abc123 in the log without wrapper"), "abc123")
	if got := e.Error(); got != "internal: token [redacted] in the log without wrapper" {
		t.Errorf("Redact of plain error = %q", got)
	}
	if code := e.Code(); code != Internal {
		t.Errorf("Redact of plain error keeps code %q, want internal", code.Name())
	}

	// New already applied the registry for both value and key.
	e2 := New(InvalidArguments, "bad for reason e3b0c442", map[string]any{"password": "irrelevant-value"})
	got := e2.Error()
	if strings.Contains(got, "e3b0c442") || !strings.Contains(got, "[redacted]") {
		t.Errorf("New did not redact registered value: %q", got)
	}
	if v, ok := e2.Details["password"]; ok && v != "[redacted]" {
		t.Errorf("New did not redact registered detail key: %v", v)
	}

	// Non-string detail values under a secret key are replaced wholly.
	e3 := New(InvalidArguments, "", map[string]any{"password": 12345})
	if v := e3.Details["password"]; v != "[redacted]" {
		t.Errorf("numeric password survived: %v", v)
	}
}

func TestWithDetailsDoesNotMutate(t *testing.T) {
	base := New(OperationConflict, "conflict", map[string]any{"running": "op_a"})
	next := base.WithDetails(map[string]any{"suggested": "wait"})
	if len(base.Details) != 1 {
		t.Errorf("WithDetails mutated the original: %v", base.Details)
	}
	if next.Details["running"] != "op_a" || next.Details["suggested"] != "wait" {
		t.Errorf("merged details = %v", next.Details)
	}
	if next.Code() != OperationConflict {
		t.Errorf("code changed: %s", next.Code().Name())
	}
}

func TestHelpersSeeWrappedErrors(t *testing.T) {
	wrapped := fmt.Errorf("resolve executor: %w", New(ExecutorNotEligible, "", nil))
	if got := HTTPStatus(wrapped); got != 422 {
		t.Errorf("HTTPStatus(wrapped) = %d, want 422", got)
	}
	if got := ExitCode(wrapped); got != 25 {
		t.Errorf("ExitCode(wrapped) = %d, want 25", got)
	}
	if got := From(wrapped).Code(); got != ExecutorNotEligible {
		t.Errorf("From(wrapped).Code() = %v, want %v", got, ExecutorNotEligible)
	}
}

func TestNewDoesNotMutateCallerDetails(t *testing.T) {
	ClearSecrets()
	defer ClearSecrets()
	AddSecretKey("password")
	details := map[string]any{"password": "hunter2"}
	e := New(ElevationDenied, "", details)
	if details["password"] != "hunter2" {
		t.Errorf("caller details mutated: %v", details)
	}
	if e.Details["password"] != redacted {
		t.Errorf("error details not redacted: %v", e.Details)
	}
}

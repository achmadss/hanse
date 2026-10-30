package herrors

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// redacted is the placeholder that replaces every secret.
const redacted = "[redacted]"

// Error is the one error type used across the Gateway, the runtimes, and
// the CLI. It carries the code, a message for people, and optional details.
// It works with errors.Is and errors.As.
type Error struct {
	code    *Code
	Message string
	Details map[string]any
}

// New creates an Error for a code. An empty message falls back to the
// code's default message. New applies the registered secret redaction, so
// secrets are never added through this constructor.
func New(code *Code, message string, details map[string]any) *Error {
	if code == nil {
		code = Internal
	}
	if message == "" {
		message = code.message
	}
	e := &Error{code: code, Message: message, Details: cloneDetails(details)}
	applySecrets(e)
	return e
}

// Code returns the error's code.
func (e *Error) Code() *Code { return e.code }

// Error implements the error interface.
func (e *Error) Error() string {
	if len(e.Details) == 0 {
		return e.code.name + ": " + e.Message
	}
	return fmt.Sprintf("%s: %s (%v)", e.code.name, e.Message, keysOf(e.Details))
}

// Is reports target as matching when it is this Error instance, a *Code
// carrying the same code, or another *Error with the same code.
// errors.Is(err, herrors.Timeout) and errors.Is(err, someOtherErrorWithThe
// SameCode) both work.
func (e *Error) Is(target error) bool {
	switch t := target.(type) {
	case *Error:
		return t.code == e.code
	case *Code:
		return t == e.code
	}
	return false
}

// WithDetails returns a copy of the error with added details, like
// errors.WrapInfo. The original error is not modified.
func (e *Error) WithDetails(details map[string]any) *Error {
	merged := make(map[string]any, len(e.Details)+len(details))
	for k, v := range e.Details {
		merged[k] = v
	}
	for k, v := range details {
		merged[k] = v
	}
	return &Error{code: e.code, Message: e.Message, Details: merged}
}

func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// From normalizes any error into an *Error. An error that is or wraps an
// *Error yields that *Error without copying; anything else maps to the
// Internal code but keeps the original error text as its message.
func From(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return New(Internal, err.Error(), nil)
}

// HTTPStatus returns the fixed HTTP status for err. An error outside the
// code set maps to the Internal code's status, 500.
func HTTPStatus(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.code.http
	}
	return Internal.http
}

// ExitCode returns the fixed CLI exit code for err. An error outside the
// code set maps to the Internal code's exit code, 1.
func ExitCode(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.code.exit
	}
	return Internal.exit
}

// --- Secret redaction -------------------------------------------------------

var (
	secretMu     sync.RWMutex
	secretValues []string
	secretKeys   = map[string]bool{}
)

// AddSecretValue registers one secret value. Every code path that creates
// or encodes an Error replaces occurrences of the value with [redacted].
// Registration is safer through configuration; compare values in constant
// time only where that matters for authentication.
func AddSecretValue(value string) {
	if value == "" {
		return
	}
	secretMu.Lock()
	defer secretMu.Unlock()
	secretValues = append(secretValues, value)
}

// AddSecretKey registers a detail key whose values are always redacted.
func AddSecretKey(key string) {
	if key == "" {
		return
	}
	secretMu.Lock()
	defer secretMu.Unlock()
	secretKeys[key] = true
}

// ClearSecrets removes all registered secrets. It exists for tests.
func ClearSecrets() {
	secretMu.Lock()
	defer secretMu.Unlock()
	secretValues = nil
	secretKeys = map[string]bool{}
}

func applySecrets(e *Error) {
	secretMu.RLock()
	values := secretValues
	keys := secretKeys
	secretMu.RUnlock()
	redactInto(e, values, keys)
}

// Redact returns a copy of err with the given secret values replaced by
// [redacted] in the message and details, and the values of the given
// detail keys replaced wholly. Registered secrets are also applied.
func Redact(err error, secrets ...string) *Error {
	e := From(err)
	copyErr := &Error{code: e.code, Message: e.Message, Details: cloneDetails(e.Details)}
	secretMu.RLock()
	keys := secretKeys
	secretMu.RUnlock()
	redactInto(copyErr, secrets, keys)
	applySecrets(copyErr)
	return copyErr
}

func redactInto(e *Error, values []string, keys map[string]bool) {
	for _, v := range values {
		e.Message = redactString(e.Message, v)
	}
	for k := range keys {
		if _, ok := e.Details[k]; ok {
			e.Details[k] = redacted
		}
	}
	for k, v := range e.Details {
		e.Details[k] = redactValue(v, values, keys)
	}
}

func redactValue(v any, values []string, keys map[string]bool) any {
	switch t := v.(type) {
	case string:
		out := t
		for _, s := range values {
			out = redactString(out, s)
		}
		return out
	case map[string]any:
		clone := make(map[string]any, len(t))
		for k, item := range t {
			if keys[k] {
				clone[k] = redacted
				continue
			}
			clone[k] = redactValue(item, values, keys)
		}
		return clone
	case []any:
		clone := make([]any, len(t))
		for i, item := range t {
			clone[i] = redactValue(item, values, keys)
		}
		return clone
	default:
		return v
	}
}

func redactString(s, secret string) string {
	if secret == "" || !strings.Contains(s, secret) {
		return s
	}
	return strings.ReplaceAll(s, secret, redacted)
}

func cloneDetails(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// --- JSON encoding ----------------------------------------------------------

// wireError is the JSON object shape the docs define:
// {"code":..., "message":..., "details":...} with details omitted when empty.
type wireError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// wire builds a redacted copy of the wire shape.
func wire(e *Error) wireError {
	secretMu.RLock()
	values, keys := secretValues, secretKeys
	secretMu.RUnlock()
	redactedErr := &Error{code: e.code, Message: e.Message, Details: cloneDetails(e.Details)}
	redactInto(redactedErr, values, keys)
	return wireError{Code: e.code.name, Message: redactedErr.Message, Details: redactedErr.Details}
}

// WriteJSON writes the error as the Gateway API error object to w, with
// secret values and secret detail keys already redacted.
func WriteJSON(w io.Writer, err error) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(wire(From(err)))
}

// JSONBytes returns the encoded error object. The trailing newline of
// WriteJSON is absent here, which is convenient for tests.
func JSONBytes(err error) ([]byte, error) {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, err); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

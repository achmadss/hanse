package herrors

import (
	"fmt"
)

// Code is one entry of the shared error code set.
//
// Naming rationale:
//   - name is the snake_case wire value used in API JSON, handler responses,
//     and operation result summaries. Where the docs already name a wire
//     value, that value is used exactly.
//   - HTTP statuses follow common API practice: 400/422 for argument
//     problems (400 for missing, 422 for present but invalid), 401/403 for
//     authentication and authorization, 404 for unknown resources, 409 for
//     conflicts, 502 for handler failures, 503 for an unavailable
//     executor or Gateway, and 504 for timeout.
//   - CLI exit codes start at 20 and group by category so they are easy to
//     memorize, are distinct across codes, avoid 0 (success), 1 (generic
//     failure, kept only for internal), 2 (usage errors, reserved by the
//     binaries), and 126+ (reserved by POSIX shells).
//
// Codes used only by the agent protocol or only as an operation's final
// state reason still carry an HTTP status and exit code so that every code
// can be mapped without special cases.
type Code struct {
	name     string
	http     int
	exit     int
	message  string
	internal bool // not returned to API clients; protocol or state-reason only
}

// Name returns the snake_case wire value.
func (c *Code) Name() string { return c.name }

// HTTPStatus returns the fixed HTTP status for the code.
func (c *Code) HTTPStatus() int { return c.http }

// ExitCode returns the fixed CLI exit code.
func (c *Code) ExitCode() int { return c.exit }

// Message returns the default short human message.
func (c *Code) Message() string { return c.message }

// Internal reports whether the code is used only inside the system
// (agent protocol or final-state reason) and is not an API error response.
func (c *Code) Internal() bool { return c.internal }

// Error makes every Code usable with errors.Is(err, Code).
func (c *Code) Error() string { return c.name + ": " + c.message }

// String implements fmt.Stringer.
func (c *Code) String() string { return c.name }

// The client-facing codes, in the order of architecture.md §14.
var (
	// UnknownCapability: the capability does not exist.
	UnknownCapability = &Code{name: "unknown_capability", http: 404, exit: 20, message: "unknown capability"}
	// UnknownOperation: the operation does not exist in that capability.
	UnknownOperation = &Code{name: "unknown_operation", http: 404, exit: 21, message: "unknown operation"}
	// InvalidArguments: the arguments do not match the schema.
	InvalidArguments = &Code{name: "invalid_arguments", http: 422, exit: 22, message: "invalid arguments"}
	// MissingArgument: a required argument is absent.
	MissingArgument = &Code{name: "missing_argument", http: 400, exit: 23, message: "missing argument"}
	// InvalidExecutor: the named executor is not a known node.
	InvalidExecutor = &Code{name: "invalid_executor", http: 404, exit: 24, message: "invalid executor"}
	// ExecutorNotEligible: the executor exists but cannot run this operation.
	ExecutorNotEligible = &Code{name: "executor_not_eligible", http: 422, exit: 25, message: "executor not eligible"}
	// NoDefaultExecutor: the request named no executor, and no default is configured.
	NoDefaultExecutor = &Code{name: "no_default_executor", http: 422, exit: 26, message: "no default executor"}
	// ExecutorUnavailable: the executor is disabled or its node agent is not connected.
	ExecutorUnavailable = &Code{name: "executor_unavailable", http: 503, exit: 27, message: "executor unavailable"}
	// AuthenticationFailed: the token is missing, wrong, or revoked.
	AuthenticationFailed = &Code{name: "authentication_failed", http: 401, exit: 28, message: "authentication failed"}
	// Forbidden: the token is valid but lacks permission for this operation.
	Forbidden = &Code{name: "forbidden", http: 403, exit: 29, message: "forbidden"}
	// ElevationRequired: a privileged operation was requested without a password.
	ElevationRequired = &Code{name: "elevation_required", http: 403, exit: 30, message: "elevation required"}
	// ElevationDenied: the password is wrong, elevation is off or not allowed
	// for this operation, or too many wrong passwords were given.
	ElevationDenied = &Code{name: "elevation_denied", http: 403, exit: 31, message: "elevation denied"}
	// OperationConflict: a run that the concurrency setting forbids running
	// alongside this one is already in progress.
	OperationConflict = &Code{name: "operation_conflict", http: 409, exit: 32, message: "operation conflict"}
	// HandlerFailed: the handler ran and reported an error, or returned
	// malformed output. The handler's own code rides inside the details.
	HandlerFailed = &Code{name: "handler_failed", http: 502, exit: 33, message: "handler failed"}
	// Timeout: the operation exceeded its deadline. Wire value per the
	// handler contract's final-state table.
	Timeout = &Code{name: "timeout", http: 504, exit: 34, message: "operation timed out"}
	// Cancelled: a client cancelled the operation. Never a normal HTTP
	// response status by itself; operations record it as their state reason.
	Cancelled = &Code{name: "cancelled", http: 409, exit: 35, message: "operation cancelled"}
)

// Codes used by the handler contract and the agent protocol. They appear in
// operation summaries and agent messages, not as standalone API responses.
var (
	// ExecutorConnectionLost: the agent did not reconnect within 5 minutes,
	// so the Gateway failed a running operation (architecture.md §8.2).
	ExecutorConnectionLost = &Code{name: "executor_connection_lost", http: 502, exit: 36, message: "executor connection lost", internal: true}
	// GatewayRestart: an operation was running when the Gateway stopped
	// (architecture.md §9.6).
	GatewayRestart = &Code{name: "gateway_restart", http: 503, exit: 37, message: "gateway restarted while the operation ran", internal: true}
	// HandlerExit: the handler exited nonzero without a valid response.
	// Wire value per the handler contract's final-state table.
	HandlerExit = &Code{name: "handler_exit", http: 502, exit: 38, message: "handler exited unexpectedly", internal: true}
	// HandlerOutputInvalid: the handler's response is missing or invalid.
	// Wire value per the handler contract's final-state table.
	HandlerOutputInvalid = &Code{name: "handler_output_invalid", http: 502, exit: 39, message: "handler produced invalid output", internal: true}
	// HTTPStatus: an http handler returned an unexpected HTTP status.
	// Wire value per the handler contract §9. Named to avoid clashing with
	// the HTTPStatus(err) helper.
	HandlerHTTPStatus = &Code{name: "http_status", http: 502, exit: 40, message: "handler endpoint returned an unexpected HTTP status", internal: true}
	// AgentBusy: the agent already runs max_concurrent_runs handlers
	// (agent protocol §4.5).
	AgentBusy = &Code{name: "agent_busy", http: 503, exit: 41, message: "node agent is busy", internal: true}
	// BundleUnavailable: the agent could not download the handler bundle
	// (agent protocol §4.5).
	BundleUnavailable = &Code{name: "bundle_unavailable", http: 503, exit: 42, message: "handler bundle unavailable", internal: true}
	// HandlerIntegrity: the cached handler files do not match the bundle
	// hash (agent protocol §4.5).
	HandlerIntegrity = &Code{name: "handler_integrity", http: 502, exit: 43, message: "handler files failed the integrity check", internal: true}
	// HandlerStartFailed: the entry command could not start
	// (agent protocol §4.5).
	HandlerStartFailed = &Code{name: "handler_start_failed", http: 502, exit: 44, message: "handler could not start", internal: true}
	// UnsupportedContract: the agent cannot run the requested handler
	// contract version (agent protocol §4.5).
	UnsupportedContract = &Code{name: "unsupported_contract", http: 422, exit: 45, message: "unsupported handler contract version", internal: true}
	// JoinTokenInvalid: the join token is unknown, used, or expired
	// (agent protocol §2).
	JoinTokenInvalid = &Code{name: "join_token_invalid", http: 401, exit: 46, message: "join token invalid", internal: true}
	// NodeMismatch: the agent's OS or architecture differs from nodes.yaml
	// (agent protocol §2).
	NodeMismatch = &Code{name: "node_mismatch", http: 409, exit: 47, message: "agent does not match its node entry", internal: true}
	// InvalidRequest: a request body is malformed (agent protocol §2).
	InvalidRequest = &Code{name: "invalid_request", http: 400, exit: 48, message: "invalid request", internal: true}
)

// Internal is the fallback code for errors created outside this set.
// Maps to HTTP 500 and CLI exit code 1.
var Internal = &Code{name: "internal", http: 500, exit: 1, message: "internal error"}

func init() {
	if err := validateCodes(); err != nil {
		panic(err)
	}
}

// validateCodes is called at init so a broken assignment fails at startup.
func validateCodes() error {
	seenName := make(map[string]bool, 32)
	seenExit := make(map[int]bool, 32)
	for _, c := range all {
		if !isWireName(c.name) {
			return fmt.Errorf("code %q is not a snake_case wire value", c.name)
		}
		if seenName[c.name] {
			return fmt.Errorf("duplicate code name %q", c.name)
		}
		if seenExit[c.exit] {
			return fmt.Errorf("code %q reuses exit code %d", c.name, c.exit)
		}
		seenName[c.name] = true
		seenExit[c.exit] = true
		if c.http < 100 || c.http > 599 {
			return fmt.Errorf("code %q has invalid HTTP status %d", c.name, c.http)
		}
		// The documented range avoids 0, 2 (usage), and 126+.
		if c.exit < 1 || c.exit > 255 {
			return fmt.Errorf("code %q uses invalid exit code %d", c.name, c.exit)
		}
		// Exit code 1 is the generic failure exit, allowed only for the
		// internal fallback code.
		if (c.exit == 1 || c.exit == 2 || c.exit >= 126) && c.name != "internal" {
			return fmt.Errorf("code %q uses reserved exit code %d", c.name, c.exit)
		}
	}
	if Internal.ExitCode() != 1 {
		return fmt.Errorf("internal must map to exit code 1")
	}
	return nil
}

// isWireName matches ^[a-z][a-z0-9_]*$, the pattern the agent protocol
// schema requires for wire codes.
func isWireName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		d := name[i]
		switch {
		case d >= 'a' && d <= 'z':
		case d >= '0' && d <= '9':
			if i == 0 {
				return false
			}
		case d == '_':
			if i == 0 || name[i-1] == '_' || i == len(name)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// all is the canonical code list; All returns a copy.
var all = []*Code{
	UnknownCapability, UnknownOperation, InvalidArguments, MissingArgument,
	InvalidExecutor, ExecutorNotEligible, NoDefaultExecutor, ExecutorUnavailable,
	AuthenticationFailed, Forbidden, ElevationRequired, ElevationDenied,
	OperationConflict, HandlerFailed, Timeout, Cancelled,
	ExecutorConnectionLost, GatewayRestart, HandlerExit, HandlerOutputInvalid,
	HandlerHTTPStatus, AgentBusy, BundleUnavailable, HandlerIntegrity,
	HandlerStartFailed, UnsupportedContract, JoinTokenInvalid, NodeMismatch,
	InvalidRequest, Internal,
}

// All returns every defined code.
func All() []*Code {
	out := make([]*Code, len(all))
	copy(out, all)
	return out
}

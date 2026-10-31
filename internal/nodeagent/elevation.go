package nodeagent

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/achmadss/hanse/internal/runner"
)

// Elevation is agent.yaml's elevation configuration
// (docs/configuration.md section 7.1). It lets one node turn privileged
// operations off, limit them to a list, and lock out repeated wrong
// passwords (docs/architecture.md section 8.5).
type Elevation struct {
	// Enabled allows privileged operations on this node.
	Enabled bool
	// Operations allows only these `<capability>.<operation>` names. An
	// empty list allows every privileged operation.
	Operations []string
	// Account is the Windows administrator account whose password
	// authorizes elevation. POSIX systems check the agent's own account
	// through sudo and ignore it.
	Account string
	// LockoutAttempts is the number of wrong passwords allowed within
	// LockoutWindow. Zero means DefaultLockoutAttempts.
	LockoutAttempts int
	// LockoutWindow is the period in which wrong passwords are counted.
	// Zero means DefaultLockoutWindow.
	LockoutWindow time.Duration
	// LockoutDuration is how long elevation stays refused after too many
	// wrong passwords. Zero means DefaultLockoutDuration.
	LockoutDuration time.Duration
}

// Defaults of the elevation lockout (docs/configuration.md section 7.1).
const (
	// DefaultLockoutAttempts is the number of wrong passwords allowed
	// within DefaultLockoutWindow.
	DefaultLockoutAttempts = 5
	// DefaultLockoutWindow is the period in which wrong passwords count.
	DefaultLockoutWindow = 10 * time.Minute
	// DefaultLockoutDuration is how long elevation stays refused after
	// too many wrong passwords.
	DefaultLockoutDuration = 10 * time.Minute
)

// elevationGate decides one privileged run: whether this node allows it,
// whether it is locked after wrong passwords, and whether the password is
// good. It keeps no password: the value is used and dropped
// (docs/agent-protocol.md section 9).
type elevationGate struct {
	enabled  bool
	ops      map[string]bool
	account  string
	attempts int
	window   time.Duration
	lockout  time.Duration
	now      func() time.Time
	// verify checks one password. It returns runner.ErrPasswordRejected
	// for a wrong password and another error when the check cannot run.
	verify func(account, password string) error

	mu          sync.Mutex
	failures    []time.Time
	lockedUntil time.Time
}

// newElevationGate builds the gate of one agent. A nil now means
// time.Now; a nil verify means the platform's password check.
func newElevationGate(cfg Elevation, now func() time.Time, verify func(account, password string) error) *elevationGate {
	if now == nil {
		now = time.Now
	}
	if verify == nil {
		verify = runner.VerifyCredential
	}
	attempts := cfg.LockoutAttempts
	if attempts <= 0 {
		attempts = DefaultLockoutAttempts
	}
	window := cfg.LockoutWindow
	if window <= 0 {
		window = DefaultLockoutWindow
	}
	duration := cfg.LockoutDuration
	if duration <= 0 {
		duration = DefaultLockoutDuration
	}
	ops := make(map[string]bool, len(cfg.Operations))
	for _, name := range cfg.Operations {
		ops[strings.ToLower(name)] = true
	}
	return &elevationGate{
		enabled:  cfg.Enabled,
		ops:      ops,
		account:  cfg.Account,
		attempts: attempts,
		window:   window,
		lockout:  duration,
		now:      now,
		verify:   verify,
	}
}

// allowed reports whether this node may run the operation with
// administrator rights at all.
func (g *elevationGate) allowed(capability, operation string) bool {
	if g == nil || !g.enabled {
		return false
	}
	if len(g.ops) == 0 {
		return true
	}
	return g.ops[strings.ToLower(capability+"."+operation)]
}

// check decides one privileged run. It returns whether elevation may go
// ahead and, when it may not, a reason that holds no password and no
// secret. A wrong password counts toward the lockout; a correct one clears
// the count.
func (g *elevationGate) check(capability, operation, password string) (bool, string) {
	if g == nil || !g.enabled {
		return false, "elevation is turned off on this node"
	}
	if !g.allowed(capability, operation) {
		return false, fmt.Sprintf("elevation is not allowed for %s.%s on this node", capability, operation)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if now.Before(g.lockedUntil) {
		return false, "elevation is locked after too many wrong passwords"
	}
	if password == "" {
		g.recordFailureLocked(now)
		return false, "the run carries no elevation password"
	}
	err := g.verify(g.account, password)
	if err == nil {
		g.failures = nil
		return true, ""
	}
	if errors.Is(err, runner.ErrPasswordRejected) {
		g.recordFailureLocked(now)
		return false, "the elevation password is wrong"
	}
	return false, "the elevation password cannot be checked: " + err.Error()
}

// recordFailureLocked counts one wrong password and starts the lockout
// when the count reaches the limit. The gate mutex must be held.
func (g *elevationGate) recordFailureLocked(now time.Time) {
	kept := g.failures[:0]
	for _, at := range g.failures {
		if now.Sub(at) < g.window {
			kept = append(kept, at)
		}
	}
	kept = append(kept, now)
	g.failures = kept
	if len(g.failures) >= g.attempts {
		g.failures = nil
		g.lockedUntil = now.Add(g.lockout)
	}
}

// locked reports whether elevation is currently refused after wrong
// passwords.
func (g *elevationGate) locked() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.now().Before(g.lockedUntil)
}

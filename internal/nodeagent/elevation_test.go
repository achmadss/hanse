package nodeagent

import (
	"errors"
	"testing"
	"time"

	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/runner"
)

// gateClock is a clock the tests move by hand.
type gateClock struct{ now time.Time }

func (c *gateClock) Now() time.Time          { return c.now }
func (c *gateClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

// newTestGate builds a gate whose password check accepts one placeholder
// value.
func newTestGate(t *testing.T, cfg Elevation, clock *gateClock) (*elevationGate, *int) {
	t.Helper()
	checks := 0
	gate := newElevationGate(cfg, clock.Now, func(_, password string) error {
		checks++
		if password == "placeholder-good-password" {
			return nil
		}
		return runner.ErrPasswordRejected
	})
	return gate, &checks
}

// TestElevationGateAcceptsTheRightPassword covers the correct password and
// a password that sudo or the logon API rejects.
func TestElevationGateAcceptsTheRightPassword(t *testing.T) {
	clock := &gateClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	gate, _ := newTestGate(t, Elevation{Enabled: true}, clock)

	if allowed, _, reason := gate.check("pc", "down", "placeholder-good-password"); !allowed {
		t.Fatalf("correct password refused: %s", reason)
	}
	if allowed, code, _ := gate.check("pc", "down", "placeholder-wrong-password"); allowed ||
		code != herrors.ElevationDenied {
		t.Fatalf("wrong password accepted or reported as %v", code)
	}
}

// TestElevationGateDisabled covers a node that turns elevation off.
func TestElevationGateDisabled(t *testing.T) {
	clock := &gateClock{now: time.Now()}
	gate, checks := newTestGate(t, Elevation{}, clock)
	if allowed, _, _ := gate.check("pc", "down", "placeholder-good-password"); allowed {
		t.Fatal("a node with elevation off accepted a privileged run")
	}
	if *checks != 0 {
		t.Error("the password was checked although elevation is off")
	}
}

// TestElevationGateAllowList covers agent.yaml's `elevation.operations`
// list: only the listed operations may elevate.
func TestElevationGateAllowList(t *testing.T) {
	clock := &gateClock{now: time.Now()}
	gate, _ := newTestGate(t, Elevation{
		Enabled:    true,
		Operations: []string{"pc.down"},
	}, clock)

	if allowed, _, reason := gate.check("pc", "down", "placeholder-good-password"); !allowed {
		t.Fatalf("a listed operation was refused: %s", reason)
	}
	if allowed, _, _ := gate.check("pc", "restart", "placeholder-good-password"); allowed {
		t.Fatal("an operation outside the allow list was elevated")
	}
}

// TestElevationGateLockout covers the lockout: five wrong passwords within
// the window refuse elevation for the lockout duration, even for the
// correct password.
func TestElevationGateLockout(t *testing.T) {
	clock := &gateClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	gate, _ := newTestGate(t, Elevation{Enabled: true}, clock)

	for attempt := 0; attempt < DefaultLockoutAttempts; attempt++ {
		if allowed, _, _ := gate.check("pc", "down", "placeholder-wrong-password"); allowed {
			t.Fatalf("attempt %d was accepted", attempt)
		}
		clock.Advance(time.Minute)
	}
	if allowed, _, reason := gate.check("pc", "down", "placeholder-good-password"); allowed {
		t.Fatalf("the correct password passed during the lockout: %s", reason)
	}
	clock.Advance(DefaultLockoutDuration)
	if allowed, _, reason := gate.check("pc", "down", "placeholder-good-password"); !allowed {
		t.Fatalf("the lockout outlasted its duration: %s", reason)
	}
}

// TestElevationGateWindowForgetsOldFailures proves the window: spread out
// wrong passwords never add up to a lockout.
func TestElevationGateWindowForgetsOldFailures(t *testing.T) {
	clock := &gateClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	gate, _ := newTestGate(t, Elevation{Enabled: true}, clock)

	for attempt := 0; attempt < DefaultLockoutAttempts*2; attempt++ {
		if allowed, _, _ := gate.check("pc", "down", "placeholder-wrong-password"); allowed {
			t.Fatalf("attempt %d was accepted", attempt)
		}
		// Each wrong password is older than the window by the time the
		// next one arrives.
		clock.Advance(DefaultLockoutWindow)
	}
	if gate.locked() {
		t.Error("wrong passwords outside the window caused a lockout")
	}
}

// TestElevationGateRefusesAnEmptyPassword covers the protocol guard: the
// Gateway refuses a privileged run without a password, so an empty one is
// a protocol bug and is neither checked nor counted toward the lockout.
func TestElevationGateRefusesAnEmptyPassword(t *testing.T) {
	clock := &gateClock{now: time.Now()}
	gate, checks := newTestGate(t, Elevation{Enabled: true}, clock)
	for attempt := 0; attempt < DefaultLockoutAttempts*2; attempt++ {
		allowed, code, _ := gate.check("pc", "down", "")
		if allowed {
			t.Fatal("an empty password was accepted")
		}
		if code != herrors.ElevationRequired {
			t.Fatalf("code %v, want elevation_required", code)
		}
	}
	if *checks != 0 {
		t.Error("an empty password reached the platform check")
	}
	if gate.locked() {
		t.Error("an empty password counted toward the lockout")
	}
}

// TestElevationGateDoesNotCountABrokenCheck proves that a check which
// cannot run, such as a missing sudo, is reported without counting as a
// wrong password.
func TestElevationGateDoesNotCountABrokenCheck(t *testing.T) {
	clock := &gateClock{now: time.Now()}
	messages := 0
	gate := newElevationGate(Elevation{Enabled: true}, clock.Now, func(_, _ string) error {
		messages++
		return errors.New("sudo is not installed with the placeholder name")
	})
	if allowed, _, reason := gate.check("pc", "down", "placeholder-some-password"); allowed {
		t.Fatal("a password check that could not run was accepted")
	} else if reason == "" {
		t.Error("the refusal carries no reason")
	}
	if messages != 1 {
		t.Errorf("the check ran %d times, want 1", messages)
	}
	if gate.locked() {
		t.Error("a broken password check caused a lockout")
	}
}

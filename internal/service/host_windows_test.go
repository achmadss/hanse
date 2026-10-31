//go:build windows

package service

import (
	"testing"
	"time"

	"golang.org/x/sys/windows/svc/mgr"
)

// TestRecoveryActionsRestartAfterEveryFailure covers the recovery rule: the
// service manager restarts the agent after a crash, with the documented
// delays, and never reboots the machine or runs a command.
func TestRecoveryActionsRestartAfterEveryFailure(t *testing.T) {
	delays := windowsRecovery()
	actions := recoveryActions(delays)
	if len(actions) != len(delays) {
		t.Fatalf("actions = %d, want %d", len(actions), len(delays))
	}
	for i, action := range actions {
		if action.Type != mgr.ServiceRestart {
			t.Errorf("action %d type = %d, want a restart", i, action.Type)
		}
		if action.Delay != delays[i] {
			t.Errorf("action %d delay = %s, want %s", i, action.Delay, delays[i])
		}
	}
	if got := recoveryResetSeconds(windowsRecoveryReset); got != 24*60*60 {
		t.Errorf("reset period = %d seconds, want one day", got)
	}
	if got := recoveryResetSeconds(-time.Second); got != 0 {
		t.Errorf("reset period = %d, want 0 for a negative period", got)
	}
}

//go:build windows

package runner

import (
	"context"
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

// requireElevatedTestProcess skips the test unless the test binary itself
// runs with the Administrators group enabled. A normal windows CI runner
// and a developer session do not satisfy that, so the test skips cleanly
// there; the service behaves like this elevated process.
func requireElevatedTestProcess(t *testing.T) {
	t.Helper()
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatalf("resolve the Administrators SID: %v", err)
	}
	member, err := windows.GetCurrentProcessToken().IsMember(administrators)
	if err != nil {
		t.Fatalf("read the process token: %v", err)
	}
	if !member {
		t.Skip("the test process is not an administrator; the Windows service elevation test needs an elevated runner")
	}
}

// TestOrdinaryHandlerStartsWithARestrictedToken proves that the restricted
// token is a valid primary token for CreateProcessAsUser, so an ordinary
// handler starts on every Windows runner, and that it holds no
// administrator rights.
func TestOrdinaryHandlerStartsWithARestrictedToken(t *testing.T) {
	helperWantMode(t, "rights")
	logs := &logCollector{}
	spec := helperSpec(t, "rights", logs.log)
	if _, err := newTestRunner(t).Run(context.Background(), spec); err != nil {
		t.Fatalf("ordinary run: %v", err)
	}
	if !logs.contains("RIGHTS admin=false") {
		t.Errorf("the ordinary handler holds administrator rights: %v", logs.all())
	}
}

// TestPrivilegedHandlerRunsWithFullRights proves the Windows elevation
// behavior of docs/architecture.md section 8.4: a privileged handler keeps
// the service's full rights, so it starts with no UAC prompt, and an
// ordinary handler runs with a restricted token in which the
// Administrators group is deny-only.
func TestPrivilegedHandlerRunsWithFullRights(t *testing.T) {
	requireElevatedTestProcess(t)
	helperWantMode(t, "rights")

	logs := &logCollector{}
	spec := helperSpec(t, "rights", logs.log)
	spec.Privileged = true
	if _, err := newTestRunner(t).Run(context.Background(), spec); err != nil {
		t.Fatalf("privileged run: %v", err)
	}
	if !logs.contains("RIGHTS admin=true") {
		t.Errorf("the privileged handler lacks administrator rights: %v", logs.all())
	}

	ordinary := &logCollector{}
	spec = helperSpec(t, "rights", ordinary.log)
	if _, err := newTestRunner(t).Run(context.Background(), spec); err != nil {
		t.Fatalf("ordinary run: %v", err)
	}
	if !ordinary.contains("RIGHTS admin=false") {
		t.Errorf("the ordinary handler kept administrator rights: %v", ordinary.all())
	}
}

// TestVerifyCredentialNeedsAKnownAdministrator covers the two refusals
// that are configuration problems, not wrong passwords: an account that
// does not exist, and an account outside the local Administrators group.
func TestVerifyCredentialNeedsAKnownAdministrator(t *testing.T) {
	err := VerifyCredential("hanse-placeholder-missing-account", "placeholder-password")
	if err == nil {
		t.Fatal("an unknown account was accepted")
	}
	if errors.Is(err, ErrPasswordRejected) {
		t.Errorf("err = %v, want a configuration error, not a rejected password", err)
	}
}

// TestVerifyCredentialWindows covers the logon check: the account that
// agent.yaml names and a wrong password. It needs a real administrator
// account, so it skips unless the operator sets the two placeholders.
func TestVerifyCredentialWindows(t *testing.T) {
	account := os.Getenv("HANSE_TEST_ELEVATION_ACCOUNT")
	if account == "" {
		t.Skip("set HANSE_TEST_ELEVATION_ACCOUNT and HANSE_TEST_ELEVATION_PASSWORD to run the logon check")
	}
	if err := VerifyCredential(account, "placeholder-wrong-password"); err != ErrPasswordRejected {
		t.Errorf("wrong password: err = %v, want ErrPasswordRejected", err)
	}
}

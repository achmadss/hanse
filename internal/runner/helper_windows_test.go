//go:build windows

package runner

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// helperSupportsTerminationSignal reports that Windows has no reliable
// SIGTERM for a process without a console.
func helperSupportsTerminationSignal() bool { return false }

// helperIgnoreTermination is a no-op on Windows: the stop request is the
// cancel file only.
func helperIgnoreTermination() {}

// helperWaitForTerminationSignal has nothing to wait for on Windows. The
// test that calls it skips this platform.
func helperWaitForTerminationSignal() {
	time.Sleep(5 * time.Second)
	fmt.Fprintln(os.Stderr, "no termination signal on Windows")
	os.Exit(6)
}

// helperReportRights writes what the handler's token says about the
// Administrators group, so the test can prove the restricted token of an
// ordinary handler and the full token of a privileged one.
func helperReportRights() {
	token := windows.GetCurrentProcessToken()
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "RIGHTS error=%v\n", err)
		return
	}
	member, err := token.IsMember(administrators)
	if err != nil {
		fmt.Fprintf(os.Stderr, "RIGHTS error=%v\n", err)
		return
	}
	// TokenIsRestricted reports restricting SIDs, which this token does not
	// use: the Administrators group is disabled instead, so IsMember says
	// whether the handler can act as an administrator.
	fmt.Fprintf(os.Stderr, "RIGHTS admin=%t\n", member)
}

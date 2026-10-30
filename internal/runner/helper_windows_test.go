//go:build windows

package runner

import (
	"fmt"
	"os"
	"time"
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

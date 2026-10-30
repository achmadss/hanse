//go:build unix

package runner

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// helperSupportsTerminationSignal reports that SIGTERM exists.
func helperSupportsTerminationSignal() bool { return true }

// helperIgnoreTermination makes the helper ignore SIGTERM, so a test can
// reach the grace-period kill.
func helperIgnoreTermination() {
	signal.Ignore(syscall.SIGTERM)
}

// helperWaitForTerminationSignal waits for SIGTERM, then proves that the
// cancel file was there first and answers with a cancelled response.
func helperWaitForTerminationSignal() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	defer signal.Stop(signals)
	select {
	case <-signals:
		fmt.Fprintln(os.Stderr, "sigterm seen")
		if _, err := os.Stat(os.Getenv("HANSE_CANCEL_FILE")); err == nil {
			fmt.Fprintln(os.Stderr, "cancel file seen before sigterm")
		}
		helperWrite(`{"contract":"hanse.handler/v1","status":"cancelled","result":{"partial":true}}`)
		os.Exit(0)
	case <-time.After(5 * time.Second):
		fmt.Fprintln(os.Stderr, "sigterm did not arrive")
		os.Exit(6)
	}
}

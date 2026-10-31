//go:build windows

package nodeagent

import (
	"fmt"
	"io"
)

// Supervise is the supervisor mode of `hanse-node`. Windows needs no
// separate supervisor: the agent service already holds its full rights, so
// it starts a privileged handler itself and stops it through the job
// object (docs/handler-contract.md section 7.2).
func Supervise(_ []string, _ io.Reader, _ io.Writer, stderr io.Writer) int {
	fmt.Fprintln(stderr, "hanse-node supervise: the supervisor mode runs only on Linux and macOS")
	return 2
}

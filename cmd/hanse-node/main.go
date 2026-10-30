// Command hanse-node is the node agent that runs handlers on an executor
// node. `hanse-node join` enrolls the node once; the connection itself is
// issue #38.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/achmadss/hanse/internal/version"
)

// defaultStateDir is the documented state directory per OS
// (docs/configuration.md §7.1). It holds the node credential that
// `hanse-node join` writes.
func defaultStateDir() string {
	if runtime.GOOS == "windows" {
		return `C:\ProgramData\Hanse\node\state`
	}
	return "/var/lib/hanse-node"
}

// exit codes: 0 success, 1 a failure, 2 a usage error.
const (
	exitOK   = 0
	exitFail = 1
	exitUse  = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// printf writes to w and deliberately does not surface the error: the
// writers are os.Stdout, os.Stderr, and in-memory test buffers.
func printf(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}

// run holds the command logic so tests can exercise it in process with
// their own arguments and writers.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hanse-node", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var showVersion bool
	var stateDir string
	fs.BoolVar(&showVersion, "version", false, "print the version and exit")
	fs.StringVar(&stateDir, "state-dir", defaultStateDir(), "directory that holds the node credential")
	fs.Usage = func() { usage(stderr) }
	if err := fs.Parse(args); err != nil {
		return exitUse
	}
	if fs.NArg() > 0 {
		if fs.Arg(0) == "join" {
			return runJoin(fs.Args()[1:], stdout, stderr, stateDir)
		}
		usage(stderr)
		return exitUse
	}
	if showVersion {
		printf(stdout, "hanse-node %s\n", version.Version)
		return exitOK
	}
	usage(stderr)
	return exitUse
}

func usage(w io.Writer) {
	printf(w, "usage: hanse-node [--version]\n")
	printf(w, "       hanse-node join <gateway-url> <join-token> [--state-dir <path>]\n")
}

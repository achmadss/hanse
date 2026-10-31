// Command hanse-gateway is the Hanse Gateway server. It runs the Gateway,
// validates its configuration with --check-config, and manages API tokens
// and node enrollment with its token and node subcommands.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/achmadss/hanse/internal/config"
	"github.com/achmadss/hanse/internal/version"
)

// defaultConfigPath is the default gateway.yaml location per operating
// system (docs/configuration.md §1.1).
func defaultConfigPath() string {
	switch runtime.GOOS {
	case "darwin":
		return "/usr/local/etc/hanse/gateway.yaml"
	case "windows":
		return `C:\ProgramData\Hanse\gateway\gateway.yaml`
	default:
		return "/etc/hanse/gateway.yaml"
	}
}

// exit codes: 0 success, 1 a configuration problem, 2 a usage error.
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
	fs := flag.NewFlagSet("hanse-gateway", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var showVersion bool
	var checkConfig bool
	var configPath string
	fs.BoolVar(&showVersion, "version", false, "print the version and exit")
	fs.BoolVar(&checkConfig, "check-config", false, "validate the configuration, print every problem, and exit without starting the server")
	fs.StringVar(&configPath, "config", defaultConfigPath(), "path to gateway.yaml")
	fs.Usage = func() { usage(stderr, fs) }

	if err := fs.Parse(args); err != nil {
		return exitUse
	}
	if fs.NArg() > 0 {
		switch fs.Arg(0) {
		case "token":
			return runToken(fs.Args()[1:], stdout, stderr, configPath)
		case "node":
			return runNode(fs.Args()[1:], stdout, stderr, configPath)
		}
		usage(stderr, fs)
		return exitUse
	}

	if showVersion {
		printf(stdout, "hanse-gateway %s\n", version.Version)
		return exitOK
	}
	if checkConfig {
		return runCheckConfig(stdout, stderr, configPath)
	}
	return runServe(stderr, configPath)
}

// runServe runs the Gateway until it receives SIGINT or SIGTERM, then shuts
// it down gracefully. A configuration problem, an unsafe transport, or a
// store or registry failure keeps the server from starting at all.
func runServe(stderr io.Writer, path string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	g, err := startGateway(ctx, path, startOptions{stderr: stderr})
	if err != nil {
		printf(stderr, "hanse-gateway: %v\n", err)
		return exitFail
	}
	if err := <-g.Done(); err != nil {
		printf(stderr, "hanse-gateway: %v\n", err)
		return exitFail
	}
	return exitOK
}

// runCheckConfig validates the configuration and prints every problem,
// one per line, naming the file path and the field. It never starts a
// server.
func runCheckConfig(stdout, stderr io.Writer, path string) int {
	_, problems, err := config.Load(path)
	if err != nil {
		printf(stderr, "hanse-gateway: %v\n", err)
		return exitFail
	}
	if len(problems) > 0 {
		for _, p := range problems {
			printf(stderr, "%s\n", p.Error())
		}
		printf(stderr, "hanse-gateway: %s: %d configuration problem(s)\n", path, len(problems))
		return exitFail
	}
	printf(stdout, "%s: configuration is valid\n", path)
	return exitOK
}

func usage(w io.Writer, fs *flag.FlagSet) {
	printf(w, "usage: hanse-gateway [--config <path>]\n")
	printf(w, "       hanse-gateway --version\n")
	printf(w, "       hanse-gateway --check-config [--config <path>]\n")
	printf(w, "       hanse-gateway token add <name> [--allow <cap.op>,...] [--config <path>]\n")
	printf(w, "       hanse-gateway token list [--config <path>]\n")
	printf(w, "       hanse-gateway token revoke <name> [--config <path>]\n")
	printf(w, "       hanse-gateway node enroll <id> [--config <path>]\n")
	printf(w, "       hanse-gateway node revoke <id> [--config <path>]\n")
	fs.SetOutput(w)
	fs.PrintDefaults()
}

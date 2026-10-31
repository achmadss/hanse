package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/achmadss/hanse/internal/version"
)

// Options carries the process dependencies of the CLI. Tests replace them.
type Options struct {
	// Stdin supplies the token when login has no --token flag.
	Stdin io.Reader
	// Stdout receives command output.
	Stdout io.Writer
	// Stderr receives warnings and errors.
	Stderr io.Writer
	// ConfigPath overrides the configuration file location. Empty means
	// os.UserConfigDir()/hanse/config.json.
	ConfigPath string
	// StdinIsTerminal reports whether Stdin is an interactive terminal.
	// When false and Stdin is an *os.File, Run detects it instead.
	StdinIsTerminal bool
	// ReadPassword reads one line from an interactive terminal without
	// echo. Empty means term.ReadPassword. Tests replace it.
	ReadPassword func(fd int) ([]byte, error)
}

// Run executes one hanse invocation and returns its exit code. The exit
// codes follow docs/architecture.md section 14; a usage error returns 2.
func Run(ctx context.Context, args []string, opts Options) int {
	if opts.Stdin == nil {
		opts.Stdin = strings.NewReader("")
	}
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}
	app := &app{opts: opts}

	if len(args) == 0 {
		printUsage(opts.Stderr)
		return 2
	}
	if (hasHelpFlag(args) && !runOperationHelpTarget(args)) || args[0] == "help" {
		printUsage(opts.Stdout)
		return 0
	}
	if args[0] == "--version" || args[0] == "-version" {
		_, _ = fmt.Fprintf(opts.Stdout, "hanse %s\n", version.Version)
		return 0
	}

	err := app.run(ctx, args)
	if err == nil {
		return 0
	}
	if errors.Is(err, errWaitInterrupted) {
		// Ctrl+C follows the shell convention: 128+SIGINT. The operation
		// keeps running, so the wait did not finish successfully.
		return interruptExitCode
	}
	var usage *usageError
	if errors.As(err, &usage) {
		_, _ = fmt.Fprintf(opts.Stderr, "hanse: %s\n", usage.message)
		_, _ = fmt.Fprintln(opts.Stderr, usage.hintOr())
		return 2
	}
	_, _ = fmt.Fprintf(opts.Stderr, "hanse: %s\n", err)
	return exitCode(err)
}

// app holds one invocation's options.
type app struct {
	opts Options
}

// run dispatches one command.
func (a *app) run(ctx context.Context, args []string) error {
	switch args[0] {
	case "version":
		return a.versionCommand(args[1:])
	case "login":
		return a.loginCommand(ctx, args[1:])
	case "capabilities":
		return a.capabilitiesCommand(ctx, args[1:])
	case "nodes":
		return a.nodesCommand(ctx, args[1:])
	case "schema":
		return a.schemaCommand(ctx, args[1:])
	case "options":
		return a.optionsCommand(ctx, args[1:])
	case "describe":
		return a.describeCommand(ctx, args[1:])
	case "validate":
		return a.validateCommand(ctx, args[1:])
	case "run":
		return a.runCommand(ctx, args[1:])
	case "operations":
		return a.operationsCommand(ctx, args[1:])
	case "logs":
		return a.logsCommand(ctx, args[1:])
	case "__complete":
		return a.completeCommand(ctx, args[1:])
	default:
		return usagef("unknown command %q", args[0])
	}
}

// usageError is a command line mistake. It maps to exit code 2.
type usageError struct {
	message string
	// hint is the line that tells the user where the usage is. Empty means
	// the command list of "hanse --help".
	hint string
}

// Error implements the error interface.
func (e *usageError) Error() string { return e.message }

// hintOr returns the help hint, or the command list hint when none is set.
func (e *usageError) hintOr() string {
	if e.hint != "" {
		return e.hint
	}
	return `run "hanse --help" for usage`
}

// withUsageHint returns the usage error err with another help hint. An error
// that is not a usage error passes through unchanged.
func withUsageHint(err error, hint string) error {
	var usage *usageError
	if !errors.As(err, &usage) {
		return err
	}
	return &usageError{message: usage.message, hint: hint}
}

// usagef returns a usage error.
func usagef(format string, args ...any) error {
	return &usageError{message: fmt.Sprintf(format, args...)}
}

// hasHelpFlag reports whether args ask for help.
func hasHelpFlag(args []string) bool {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" || arg == "-help" {
			return true
		}
	}
	return false
}

// printUsage writes the command reference.
func printUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `usage: hanse <command> [flags]

Commands:
  login --url <url> [--token <token>]       Save the Gateway URL and token.
  capabilities [show <id>]                  List capabilities or show one.
  capabilities install <git-url|path>       Install a package on the Gateway host (--ref, --yes).
  capabilities remove <id>                  Remove an installed capability (--yes).
  nodes [show <id>]                         List nodes or show one.
  nodes capabilities <id>                   List the capabilities of a node.
  schema <capability> <operation>           Show an operation's argument schema.
  options <capability> <operation> <field>  Show the valid values for an argument.
  describe <capability> <operation>         Show everything needed to build a request.
  validate <capability> <operation>         Validate a request without running it.
  run <capability> <operation> [flags]      Run an operation; its flags come from its schema.
  operations [filters]                      List operations.
  operations show <id>                      Show one operation.
  operations cancel <id>                    Cancel an operation.
  logs <id> [-f]                            Print an operation's log; -f follows it.
  version                                   Print the CLI version.

Every command accepts --format text|json. The default is text.
`)
}

// newFlagSet returns a silent flag set: Run reports its own errors.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parseArgs parses flags that may appear before, between, or after
// positional arguments, and returns the positional arguments. The flag
// package stops at the first non-flag argument, so the loop resumes after
// each one. A "--" terminator keeps every later argument positional, as
// the flag package's own parsing does.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if len(args) > 0 && args[0] == "--" {
			return append(positional, args[1:]...), nil
		}
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, usagef("usage requested")
			}
			return nil, usagef("%s", err)
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// formatFlag registers --format on fs and returns a pointer to its value.
func formatFlag(fs *flag.FlagSet) *string {
	return fs.String("format", string(formatText), "output format: text or json")
}

// outputFormat validates the parsed --format value.
func outputFormat(value string) (format, error) {
	return parseFormat(value)
}

// warnf writes one warning line to stderr.
func (a *app) warnf(format string, args ...any) {
	_, _ = fmt.Fprintf(a.opts.Stderr, "hanse: warning: "+format+"\n", args...)
}

// versionValue returns the CLI version string.
func versionValue() string { return version.Version }

// configPath returns the configuration file location.
func (a *app) configPath() (string, error) {
	if a.opts.ConfigPath != "" {
		return a.opts.ConfigPath, nil
	}
	path, err := defaultConfigPath()
	if err != nil {
		return "", err
	}
	return path, nil
}

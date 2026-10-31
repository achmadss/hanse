package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/achmadss/hanse/internal/client"
	herrors "github.com/achmadss/hanse/internal/errors"
	"golang.org/x/term"
)

// The wait between two status reads starts short and backs off, so a fast
// operation reports at once and a long one does not flood the Gateway.
const (
	initialPollDelay = 100 * time.Millisecond
	maxPollDelay     = 2 * time.Second
	pollBackoff      = 1.5
)

// runValueFlags are the run flags that take a value. runTarget skips their
// value, so the capability and operation are found even when one of these
// flags comes first.
var runValueFlags = map[string]bool{
	"executor":  true,
	"password":  true,
	"format":    true,
	"args-json": true,
	"args-file": true,
}

// runFollow is the hook of the --follow flag. It streams one operation's
// logs and returns when the stream ends or ctx is cancelled.
// waitOperation runs it before its polling loop, so --follow only adds a
// stream to the wait and never changes the exit code.
type runFollow func(ctx context.Context, c *client.Client, operation string) error

// errWaitInterrupted reports that Ctrl+C stopped the wait. The operation
// keeps running, and Run turns the error into exit code 130.
var errWaitInterrupted = errors.New("the wait was interrupted")

// interruptExitCode is 128+SIGINT, the shell convention for a command the
// user stopped. docs/architecture.md section 14 keeps 130 free for it.
const interruptExitCode = 130

// runCommand implements run <capability> <operation>. The argument flags
// come from the operation's schema at run time
// (docs/architecture.md sections 11 and 12), so a new capability works with
// no CLI change.
func (a *app) runCommand(ctx context.Context, args []string) error {
	capability, operation, rest, err := runTarget(args)
	if err != nil {
		return err
	}
	if capability == "" || operation == "" {
		return usagef("usage: hanse run <capability> <operation> [flags]")
	}
	c, _, err := a.open(ctx)
	if err != nil {
		return err
	}
	describe, err := c.Describe(ctx, capability, operation)
	if err != nil {
		return err
	}
	rf, err := newRunFlags(describe)
	if err != nil {
		return err
	}
	if hasHelpFlag(args) {
		return a.writeRunHelp(describe, rf)
	}
	hint := fmt.Sprintf("run \"hanse run %s %s --help\" for usage",
		oneLine(describe.Capability), oneLine(describe.Operation))
	positional, err := parseArgs(rf.fs, rest)
	if err != nil {
		return withUsageHint(err, hint)
	}
	if len(positional) > 0 {
		return withUsageHint(usagef("unexpected argument %q", positional[0]), hint)
	}
	if *rf.detach && *rf.follow {
		return withUsageHint(usagef("--follow and --detach cannot be used together"), hint)
	}
	fm, err := outputFormat(*rf.formatValue)
	if err != nil {
		return withUsageHint(err, hint)
	}
	arguments, err := a.runArguments(rf)
	if err != nil {
		return withUsageHint(err, hint)
	}
	if err := checkRequired(describe, arguments); err != nil {
		return err
	}
	password, err := a.operationPassword(describe, rf)
	if err != nil {
		return err
	}

	request := client.SubmitRequest{
		Capability: describe.Capability,
		Operation:  describe.Operation,
		Arguments:  arguments,
		Executor:   strings.TrimSpace(*rf.executor),
	}
	if password != "" {
		request.Elevation = &client.Elevation{Password: password}
	}
	record, err := c.Submit(ctx, request)
	if err != nil {
		// A conflict names the operation that holds the concurrency slot,
		// so the user can follow that one instead of retrying blindly
		// (docs/architecture.md section 9.4).
		a.printConflict(err)
		a.printValidationProblems(err)
		return handlerFailureOf(err)
	}
	if *rf.detach {
		if err := a.writeRunDetached(fm, record); err != nil {
			return err
		}
		return operationFailure(record)
	}
	// --follow streams the log while the command still waits for the
	// final record, so the exit code and the result are the same as a
	// plain run (docs/architecture.md section 12).
	var follow runFollow
	if *rf.follow {
		follow = func(followCtx context.Context, followClient *client.Client, operation string) error {
			_, err := a.followLogs(followCtx, followClient, operation, fm)
			return err
		}
	}
	record, err = a.waitOperation(ctx, c, record, follow)
	if err != nil {
		if errors.Is(err, errWaitInterrupted) {
			a.warnf("stopped waiting; operation %s is still running: follow it with \"hanse operations show %s\"",
				oneLine(record.ID), oneLine(record.ID))
			return errWaitInterrupted
		}
		return err
	}
	if fm == formatJSON {
		if err := writeJSON(a.opts.Stdout, client.OperationRecordEnvelope{Operation: record}); err != nil {
			return err
		}
	} else if err := a.writeOperation(record); err != nil {
		return err
	}
	return operationFailure(record)
}

// runTarget returns the positional capability and operation of a run
// invocation, and the remaining arguments. The two positionals come before
// the schema's own flags, because their names are not known yet.
func runTarget(args []string) (string, string, []string, error) {
	rest := make([]string, 0, len(args))
	capability, operation := "", ""
	for i := 0; i < len(args); i++ {
		token := args[i]
		if token == "--" {
			rest = append(rest, args[i:]...)
			return capability, operation, rest, nil
		}
		if strings.HasPrefix(token, "-") && token != "-" {
			name := strings.TrimLeft(token, "-")
			if index := strings.IndexByte(name, '='); index >= 0 {
				rest = append(rest, token)
				continue
			}
			rest = append(rest, token)
			if !runValueFlags[name] {
				// An argument flag needs its schema, so it comes after the
				// capability and operation. Everything from here belongs to
				// the flag parse.
				rest = append(rest, args[i+1:]...)
				return capability, operation, rest, nil
			}
			if i+1 < len(args) {
				i++
				rest = append(rest, args[i])
			}
			continue
		}
		switch {
		case capability == "":
			capability = token
		case operation == "":
			operation = token
		default:
			return capability, operation, rest, usagef("unexpected argument %q", token)
		}
	}
	return capability, operation, rest, nil
}

// runOperationHelpTarget reports whether args ask for the help of one
// operation, whose flags come from its schema rather than from the command
// list.
func runOperationHelpTarget(args []string) bool {
	if len(args) < 2 || args[0] != "run" {
		return false
	}
	capability, operation, _, err := runTarget(args[1:])
	return err == nil && capability != "" && operation != ""
}

// writeRunHelp prints the usage, the operation's mode and privilege, and
// every flag, including the argument flags of the schema.
func (a *app) writeRunHelp(describe client.Describe, rf *runFlags) error {
	w := a.opts.Stdout
	if _, err := fmt.Fprintf(w, "usage: hanse run %s %s [flags]\n\n", describe.Capability, describe.Operation); err != nil {
		return err
	}
	if describe.Description != "" {
		if _, err := fmt.Fprintf(w, "%s\n\n", oneLine(describe.Description)); err != nil {
			return err
		}
	}
	rows := [][2]string{
		{"Mode", describe.Mode},
		{"Privileged", describeBool(describe.Privileged)},
		{"Executor required", describeBool(describe.ExecutorRequired)},
	}
	if describe.DefaultExecutor != "" {
		rows = append(rows, [2]string{"Default executor", describe.DefaultExecutor})
	}
	if err := writeKeyValue(w, rows); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "\nFlags:"); err != nil {
		return err
	}
	valueNames := flagValueNames()
	for _, argument := range rf.arguments {
		valueNames[argument.name] = argument.flag.usagePlaceholder()
	}
	names := make([]string, 0, len(valueNames))
	rf.fs.VisitAll(func(f *flag.Flag) {
		if f.Name == "password" && !describe.Privileged {
			return
		}
		names = append(names, f.Name)
	})
	tw := newTable(w)
	for _, name := range names {
		definition := rf.fs.Lookup(name)
		if definition == nil {
			continue
		}
		left := "--" + name
		if value := valueNames[name]; value != "" {
			left += " " + value
		}
		if _, err := fmt.Fprintf(tw, "  %s\t%s\n", left, oneLine(definition.Usage)); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "\nAn explicit argument flag overrides the key of the same name from --args-json or --args-file."); err != nil {
		return err
	}
	objectArgs := runObjectArguments(describe, rf)
	if len(objectArgs) > 0 {
		if _, err := fmt.Fprintf(w, "Pass %s with --args-json or --args-file.\n", strings.Join(objectArgs, ", ")); err != nil {
			return err
		}
	}
	return nil
}

// runObjectArguments lists the schema properties that have no flag, so the
// help says which arguments need JSON input.
func runObjectArguments(describe client.Describe, rf *runFlags) []string {
	registered := make(map[string]bool, len(rf.arguments))
	for _, argument := range rf.arguments {
		registered[argument.name] = true
	}
	names := make([]string, 0, len(registered))
	for name := range schemaProperties(describe.Schema) {
		if !registered[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// operationPassword resolves the password of a privileged operation. A
// --password value is used as given and warned about every time; otherwise
// an interactive terminal is prompted without echo. With no terminal and no
// flag, the command fails with elevation_required before it calls the API,
// as docs/architecture.md section 12 requires. The value never reaches the
// output or the error path.
func (a *app) operationPassword(describe client.Describe, rf *runFlags) (string, error) {
	if flagWasSet(rf.fs, "password") {
		a.warnf("--password puts the password on the command line: the shell can save it in its history and other users of this machine can see it while the command runs")
	}
	password := *rf.password
	if !describe.Privileged {
		if password != "" {
			a.warnf("the operation %q is not privileged: --password is ignored",
				describe.Capability+"."+describe.Operation)
		}
		return "", nil
	}
	if password != "" {
		return password, nil
	}
	if !a.stdinIsTerminal() {
		return "", herrors.New(herrors.ElevationRequired,
			fmt.Sprintf("operation %q needs a password: pass --password or run the command from a terminal",
				describe.Capability+"."+describe.Operation), nil)
	}
	return a.promptPassword()
}

// promptPassword reads the password from an interactive terminal without
// echo. The prompt goes to stderr, so stdout stays clean for the result.
func (a *app) promptPassword() (string, error) {
	fd := 0
	if f, ok := a.opts.Stdin.(*os.File); ok {
		fd = int(f.Fd())
	}
	read := a.opts.ReadPassword
	if read == nil {
		read = term.ReadPassword
	}
	_, _ = fmt.Fprint(a.opts.Stderr, "Password: ")
	data, err := read(fd)
	_, _ = fmt.Fprintln(a.opts.Stderr)
	if err != nil {
		return "", herrors.New(herrors.Internal, "cannot read the password without echo: "+err.Error(), nil)
	}
	password := strings.TrimRight(string(data), "\r\n")
	if password == "" {
		return "", herrors.New(herrors.ElevationRequired, "the password is empty", nil)
	}
	return password, nil
}

// waitOperation waits for a terminal state. A synchronous operation already
// arrives finished, so the first check returns at once. Ctrl+C ends the wait
// and leaves the operation running.
func (a *app) waitOperation(ctx context.Context, c *client.Client, record client.OperationRecord, follow runFollow) (client.OperationRecord, error) {
	if follow != nil {
		if err := follow(ctx, c, record.ID); err != nil {
			return record, err
		}
	}
	delay := initialPollDelay
	for !operationFinished(record) {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return record, errWaitInterrupted
		case <-timer.C:
		}
		delay = nextPollDelay(delay)
		next, err := c.Operation(ctx, record.ID)
		if err != nil {
			if ctx.Err() != nil {
				return record, errWaitInterrupted
			}
			return record, err
		}
		record = next
	}
	return record, nil
}

// nextPollDelay returns the next wait between two status reads.
func nextPollDelay(delay time.Duration) time.Duration {
	next := time.Duration(float64(delay) * pollBackoff)
	if next > maxPollDelay {
		next = maxPollDelay
	}
	return next
}

// operationFinished reports whether a status is terminal
// (docs/architecture.md section 9.1).
func operationFinished(record client.OperationRecord) bool {
	switch record.Status {
	case "completed", "failed", "cancelled":
		return true
	default:
		return false
	}
}

// operationFailure returns the documented error of a finished operation
// that did not complete, so its CLI exit code follows section 14.
func operationFailure(record client.OperationRecord) error {
	if record.Status != "failed" && record.Status != "cancelled" {
		return nil
	}
	if record.Error == nil {
		code := herrors.Internal
		if record.Status == "cancelled" {
			code = herrors.Cancelled
		}
		return herrors.New(code, "the operation ended with status "+record.Status, nil)
	}
	code, ok := client.Code(record.Error.Code)
	if !ok {
		// The final-state code belongs to the capability, not to the shared
		// set, so the CLI exits with handler_failed and keeps the handler's
		// own code, message, and details (docs/architecture.md section 14).
		return handlerFailure(record.ID, record.Error.Code, record.Error.Message, record.Error.Details)
	}
	return herrors.New(code, record.Error.Message, record.Error.Details)
}

// handlerFailure builds the error of a handler that reported its own
// failure. The handler's code and message lead the error message, so the
// terminal shows them, and its details ride inside the error details, as an
// API response does (docs/architecture.md section 14).
func handlerFailure(operationID, code, message string, details map[string]any) error {
	outer := map[string]any{"code": code}
	if operationID != "" {
		outer["operation_id"] = operationID
	}
	if message != "" {
		outer["message"] = message
	}
	if len(details) > 0 {
		outer["details"] = details
	}
	return herrors.New(herrors.HandlerFailed, handlerMessage(code, message), outer)
}

// handlerFailureOf rewrites a handler_failed API error from a synchronous
// run, so the handler's own code and message reach the terminal. The error
// keeps the handler_failed code, so the exit code does not change.
func handlerFailureOf(err error) error {
	var apiErr *herrors.Error
	if !errors.As(err, &apiErr) || !errors.Is(apiErr, herrors.HandlerFailed) {
		return err
	}
	code, _ := apiErr.Details["code"].(string)
	if code == "" {
		return err
	}
	message, _ := apiErr.Details["message"].(string)
	details, _ := apiErr.Details["details"].(map[string]any)
	operationID, _ := apiErr.Details["operation_id"].(string)
	return handlerFailure(operationID, code, message, details)
}

// handlerMessage renders the handler's own code and message as one line.
func handlerMessage(code, message string) string {
	if message == "" {
		return code
	}
	return code + ": " + message
}

// writeRunDetached prints the operation ID and returns without waiting.
func (a *app) writeRunDetached(fm format, record client.OperationRecord) error {
	if fm == formatJSON {
		return writeJSON(a.opts.Stdout, client.OperationRecordEnvelope{Operation: record})
	}
	_, err := fmt.Fprintln(a.opts.Stdout, oneLine(record.ID))
	return err
}

// printConflict writes the running operation's ID of an operation_conflict
// error, so the user can follow that operation instead
// (docs/architecture.md section 9.4).
func (a *app) printConflict(err error) {
	if !errors.Is(err, herrors.OperationConflict) {
		return
	}
	var apiErr *herrors.Error
	if !errors.As(err, &apiErr) {
		return
	}
	id, _ := apiErr.Details["operation_id"].(string)
	if id == "" {
		return
	}
	_, _ = fmt.Fprintf(a.opts.Stderr,
		"hanse: problem: operation %s is already running; follow it with \"hanse operations show %s\"\n",
		oneLine(id), oneLine(id))
}

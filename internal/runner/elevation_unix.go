//go:build unix

package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

// elevationCheckTimeout bounds one password check, so a sudo that waits
// for a terminal cannot block a run.
const elevationCheckTimeout = 30 * time.Second

// elevationCheckFlag asks the supervisor to check its own permission and
// exit, without starting a handler.
const elevationCheckFlag = "--check"

// VerifyCredential checks one elevation password with sudo. It runs
// `sudo -S -k -p "" -- <supervisor> --check`: a command, so `-k` not only
// ignores a cached credential but also creates no new timestamp, and
// nothing the agent starts afterwards can use sudo without a password
// (docs/architecture.md section 8.5). The supervisor's own check mode is
// the command, so a sudoers rule that allows only the supervisor is enough
// for the check; `true` would need an extra rule. The password arrives on
// sudo's own stdin, never in the argument list or the environment
// (docs/agent-protocol.md section 9).
//
// A wrong password returns [ErrPasswordRejected] and counts toward the
// lockout. sudo reads the password only when the account may run the
// command, so a missing sudo, a policy that forbids the command, or
// another failure leaves the password unread and returns another error
// that does not count.
func VerifyCredential(_ string, password string, supervisor ...string) error {
	command, err := supervisorCommand(supervisor)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), elevationCheckTimeout)
	defer cancel()
	argv := append([]string{"-S", "-k", "-p", "", "--"}, command...)
	argv = append(argv, elevationCheckFlag)
	cmd := exec.CommandContext(ctx, "sudo", argv...)
	cmd.Env = sudoEnv()
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("the password pipe cannot be created: %w", err)
	}
	cmd.Stdin = stdinR
	// sudo's diagnostics name the account and the host, and the caller
	// decides what the user sees.
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	line := []byte(password + "\n")
	if _, err := stdinW.Write(line); err != nil {
		closeFile(stdinW)
		closeFile(stdinR)
		return fmt.Errorf("the password cannot be written: %w", err)
	}
	zeroBytes(line)
	closeFile(stdinW)

	runErr := cmd.Run()
	// sudo reads the password byte by byte, so an untouched pipe means it
	// refused the account or the command before it asked for a password.
	unread, certain := readRemaining(stdinR)
	if runErr == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) && certain && unread == 0 {
		return ErrPasswordRejected
	}
	if errors.As(runErr, &exitErr) {
		return fmt.Errorf("sudo refused the check without reading the password (exit status %d)",
			exitErr.ExitCode())
	}
	return fmt.Errorf("sudo cannot run the password check: %w", runErr)
}

// readRemaining returns how many bytes of the password stayed in the pipe
// after sudo ran, and whether the count is certain. A read error leaves
// the count uncertain, so the caller does not count a wrong password.
func readRemaining(r *os.File) (int, bool) {
	if r == nil {
		return 0, false
	}
	_ = r.SetReadDeadline(time.Now().Add(5 * time.Second))
	rest, err := io.ReadAll(r)
	closeFile(r)
	if err != nil && !errors.Is(err, io.EOF) {
		return len(rest), false
	}
	return len(rest), true
}

// closeFile closes one pipe end whose other side has failed or finished.
func closeFile(f *os.File) {
	if f != nil {
		_ = f.Close()
	}
}

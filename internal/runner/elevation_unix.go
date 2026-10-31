//go:build unix

package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// elevationCheckTimeout bounds one password check, so a sudo that waits
// for a terminal cannot block a run.
const elevationCheckTimeout = 30 * time.Second

// elevationCheckFlag asks the supervisor to check its own permission and
// exit, without starting a handler.
const elevationCheckFlag = "--check"

// ElevationPrompt is the prompt the password check gives sudo. sudo writes
// it to its standard error when it asks for a password, so the agent can
// tell a rejected password from a refusal that never asked for one.
const ElevationPrompt = "hanse-elevation-password:"

// The supervisor's check mode reports what it saw on its standard input:
// sudo already read the password line, or the line was still there.
const (
	// SupervisorCheckRead means sudo read the password before it started
	// the check.
	SupervisorCheckRead = "hanse-supervisor-check: password-read"
	// SupervisorCheckUnread means the password was still on standard
	// input, so sudo did not ask for one.
	SupervisorCheckUnread = "hanse-supervisor-check: password-unread"
)

// VerifyCredential checks one elevation password with sudo. It runs
// `sudo -S -k -p <prompt> -- <supervisor> --check`: a command, so `-k` not
// only ignores a cached credential but also creates no new timestamp, and
// nothing the agent starts afterwards can use sudo without a password
// (docs/architecture.md section 8.5). The supervisor's own check mode is
// the command, so a sudoers rule that allows only the supervisor is enough
// for the check; `true` would need an extra rule. The password arrives on
// sudo's own standard input, never in the argument list or the environment
// (docs/agent-protocol.md section 9).
//
// The check mode reports whether sudo read the password. When it did not,
// the sudoers rule does not require one, which would accept any password,
// so the check fails closed. A wrong password returns [ErrPasswordRejected]
// and counts toward the lockout; every other failure is a node or policy
// problem and does not count.
func VerifyCredential(_ string, password string, supervisor ...string) error {
	command, err := supervisorCommand(supervisor)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), elevationCheckTimeout)
	defer cancel()
	argv := append([]string{"-S", "-k", "-p", ElevationPrompt, "--"}, command...)
	argv = append(argv, elevationCheckFlag)
	cmd := exec.CommandContext(ctx, "sudo", argv...)
	cmd.Env = sudoEnv()
	cmd.Stdin = strings.NewReader(password + "\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	out := stdout.String()
	if runErr == nil {
		switch {
		case strings.Contains(out, SupervisorCheckRead):
			return nil
		case strings.Contains(out, SupervisorCheckUnread):
			return errors.New(
				"sudo did not ask for a password: the sudoers rule for the supervisor must not use NOPASSWD")
		default:
			return errors.New("sudo did not report whether it read the password")
		}
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		if strings.Contains(stderr.String(), ElevationPrompt) {
			// sudo asked for a password and rejected it.
			return ErrPasswordRejected
		}
		return fmt.Errorf("sudo refused the check without asking for a password (exit status %d)",
			exitErr.ExitCode())
	}
	return fmt.Errorf("sudo cannot run the password check: %w", runErr)
}

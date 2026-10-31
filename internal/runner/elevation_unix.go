//go:build unix

package runner

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

// elevationCheckTimeout bounds one password check, so a sudo that waits
// for a terminal cannot block a run.
const elevationCheckTimeout = 30 * time.Second

// VerifyCredential checks one elevation password with sudo. It runs
// `sudo -S -k -v`, which invalidates any cached credential first and then
// checks the password of the agent's account
// (docs/architecture.md section 8.5). The password arrives on sudo's own
// stdin, never in the argument list or the environment
// (docs/agent-protocol.md section 9).
func VerifyCredential(_ string, password string) error {
	ctx, cancel := context.WithTimeout(context.Background(), elevationCheckTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sudo", "-S", "-k", "-p", "", "-v")
	cmd.Env = sudoEnv()
	cmd.Stdin = strings.NewReader(password + "\n")
	// sudo's diagnostics are dropped: they name the account and the
	// host, and the caller decides what the user sees.
	var diagnostics bytes.Buffer
	cmd.Stdout = &diagnostics
	cmd.Stderr = &diagnostics
	err := cmd.Run()
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return ErrPasswordRejected
	}
	// sudo is missing or cannot run: that is a broken node, not a wrong
	// password.
	return err
}

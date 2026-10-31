//go:build unix

package runner

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVerifyCredentialKeepsThePasswordOffArgvAndEnv covers the private
// pipe rule of docs/agent-protocol.md section 9: the password reaches
// sudo on its stdin, never in the argument list or the environment.
func TestVerifyCredentialKeepsThePasswordOffArgvAndEnv(t *testing.T) {
	dir := t.TempDir()
	argvLog := filepath.Join(dir, "argv")
	envLog := filepath.Join(dir, "env")
	script := "#!/bin/sh\n" +
		"printf '%s' \"$*\" > " + argvLog + "\n" +
		"env > " + envLog + "\n" +
		"IFS= read -r password\n" +
		"[ \"$password\" = \"placeholder-good-password\" ]\n"
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake sudo: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := VerifyCredential("", "placeholder-good-password"); err != nil {
		t.Fatalf("correct password: %v", err)
	}
	if err := VerifyCredential("", "placeholder-wrong-password"); !errors.Is(err, ErrPasswordRejected) {
		t.Fatalf("wrong password: err = %v, want ErrPasswordRejected", err)
	}
	for _, path := range []string{argvLog, envLog} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if strings.Contains(string(data), "placeholder-good-password") ||
			strings.Contains(string(data), "placeholder-wrong-password") {
			t.Errorf("%s holds the elevation password: %s", path, data)
		}
	}
}

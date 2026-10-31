//go:build unix

package service

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
)

// unixHost carries out a plan on Linux or macOS. It writes files with the
// documented mode and owner, and it runs the service manager's own
// commands.
type unixHost struct {
	stdout io.Writer
	stderr io.Writer
}

// NewHost returns the host of the running operating system. The writers
// receive the output of the service manager's commands.
func NewHost(stdout, stderr io.Writer) Host {
	return &unixHost{stdout: stdout, stderr: stderr}
}

// Account looks the account up in the local account database.
func (h *unixHost) Account(name string) (Account, error) {
	acc, err := user.Lookup(name)
	if err != nil {
		return Account{}, fmt.Errorf("the account %q does not exist", name)
	}
	uid, err := strconv.Atoi(acc.Uid)
	if err != nil {
		return Account{}, fmt.Errorf("the account %q has an invalid user id: %v", name, err)
	}
	gid, err := strconv.Atoi(acc.Gid)
	if err != nil {
		return Account{}, fmt.Errorf("the account %q has an invalid group id: %v", name, err)
	}
	account := Account{Name: acc.Username, UID: uid, GID: gid}
	if group, err := user.LookupGroupId(acc.Gid); err == nil {
		account.Group = group.Name
	}
	return account, nil
}

// Administrator reports whether this process is root.
func (h *unixHost) Administrator() (bool, error) {
	return os.Geteuid() == 0, nil
}

// MkdirAll creates a directory and gives it its mode and owner.
func (h *unixHost) MkdirAll(path string, mode os.FileMode, owner *Owner) error {
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	return h.Chown(path, mode, owner)
}

// WriteFile writes a file through a temporary file in the same directory,
// so a crash never leaves a half-written unit or configuration.
func (h *unixHost) WriteFile(path string, data []byte, mode os.FileMode, owner *Owner, createOnly bool) error {
	if createOnly {
		switch _, err := os.Lstat(path); {
		case err == nil:
			// The file exists: keep its content and set the documented
			// permissions, so the service account can read what an
			// enrollment wrote and nobody else can change it.
			return h.Chown(path, mode, owner)
		case !errors.Is(err, fs.ErrNotExist):
			return err
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".hanse-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := h.Chown(name, mode, owner); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// Chown sets the mode of a file or directory, and its owner when one is
// given.
func (h *unixHost) Chown(path string, mode os.FileMode, owner *Owner) error {
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	if owner == nil {
		return nil
	}
	return os.Chown(path, owner.UID, owner.GID)
}

// RemoveFile removes a file and treats an absent file as success.
func (h *unixHost) RemoveFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Run runs a service manager command and passes its output through.
func (h *unixHost) Run(args []string) error {
	if len(args) == 0 {
		return errors.New("the command is empty")
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout = h.stdout
	cmd.Stderr = h.stderr
	return cmd.Run()
}

// RegisterService is a Windows step.
func (h *unixHost) RegisterService(Spec) error { return errUnsupported }

// StartService is a Windows step.
func (h *unixHost) StartService(string) error { return errUnsupported }

// StopService is a Windows step.
func (h *unixHost) StopService(string) error { return errUnsupported }

// DeleteService is a Windows step.
func (h *unixHost) DeleteService(string) error { return errUnsupported }

// AddGroupMember is a Windows step.
func (h *unixHost) AddGroupMember(string, string) error { return errUnsupported }

// RemoveGroupMember is a Windows step.
func (h *unixHost) RemoveGroupMember(string, string) error { return errUnsupported }

// Grant is a Windows step: POSIX uses the file's mode and group, which
// WriteFile and Chown set.
func (h *unixHost) Grant(string, Access) error { return nil }

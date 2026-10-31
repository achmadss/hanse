package service

import (
	"errors"
	"os"
)

// errUnsupported is what a host returns for a step that belongs to another
// operating system.
var errUnsupported = errors.New("the step is not supported on this operating system")

// Host is the set of operating system calls that a plan makes. Every call
// goes through this interface, so a test records the calls and asserts on
// them without root: no test registers a service, changes a group, or
// writes to /etc.
type Host interface {
	// Account looks up a local account and its primary group.
	Account(name string) (Account, error)
	// Administrator reports whether this process may register a system
	// service.
	Administrator() (bool, error)
	// MkdirAll creates a directory and its parents, and sets its mode and
	// owner.
	MkdirAll(path string, mode os.FileMode, owner *Owner) error
	// WriteFile writes a file with the given mode and owner. When
	// createOnly is set and the file exists, WriteFile keeps the content
	// and still sets the mode and the owner on POSIX, so a configuration
	// that an earlier install wrote stays readable by the service
	// account.
	WriteFile(path string, data []byte, mode os.FileMode, owner *Owner, createOnly bool) error
	// Chown sets the mode and the owner of an existing file.
	Chown(path string, mode os.FileMode, owner *Owner) error
	// RemoveFile removes a file and reports no error when it is absent.
	RemoveFile(path string) error
	// Run runs a service manager command such as systemctl or launchctl.
	Run(args []string) error
	// RunQuiet runs a service manager command and discards its output,
	// for a step that usually finds nothing to do.
	RunQuiet(args []string) error
	// RegisterService creates a Windows service, or updates the
	// definition of an existing one.
	RegisterService(spec Spec) error
	// StartService starts a Windows service.
	StartService(name string) error
	// StopService stops a Windows service and waits for it to stop.
	StopService(name string) error
	// DeleteService removes a Windows service.
	DeleteService(name string) error
	// AddGroupMember adds member to the local group with the given SID.
	AddGroupMember(groupSID, member string) error
	// RemoveGroupMember removes member from that group.
	RemoveGroupMember(groupSID, member string) error
	// Grant gives the service account access to a file or a directory.
	Grant(path string, access Access) error
}

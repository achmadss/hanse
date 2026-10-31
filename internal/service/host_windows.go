//go:build windows

package service

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// The Windows network and security APIs that golang.org/x/sys/windows does
// not export.
var (
	netapi32 = windows.NewLazySystemDLL("netapi32.dll")
	advapi32 = windows.NewLazySystemDLL("advapi32.dll")

	procNetLocalGroupAddMembers = netapi32.NewProc("NetLocalGroupAddMembers")
	procNetLocalGroupDelMembers = netapi32.NewProc("NetLocalGroupDelMembers")
	procLookupAccountName       = advapi32.NewProc("LookupAccountNameW")
)

// Constants of the Windows network API and of the service wait.
const (
	// localGroupMembersLevel3 passes the member as an account name.
	localGroupMembersLevel3 = 3
	// errorMemberInAlias means the account is already a member.
	errorMemberInAlias = syscall.Errno(1378)
	// errorMemberNotInAlias means the account is not a member.
	errorMemberNotInAlias = syscall.Errno(1377)
	// serviceStopTimeout bounds the wait for a service to stop.
	serviceStopTimeout = 60 * time.Second
	// servicePoll is how often the service state is read while waiting.
	servicePoll = 100 * time.Millisecond
)

// localGroupMembersInfo3 is LOCALGROUP_MEMBERS_INFO_3: one member, named
// by account.
type localGroupMembersInfo3 struct {
	domainAndName *uint16
}

// windowsHost carries out a plan on Windows. The service manager keeps the
// definition of the service, so it has no unit file.
type windowsHost struct {
	stdout io.Writer
	stderr io.Writer
}

// NewHost returns the host of the running operating system. The writers
// receive the output of the service manager's commands.
func NewHost(stdout, stderr io.Writer) Host {
	return &windowsHost{stdout: stdout, stderr: stderr}
}

// Account checks that the account exists. Windows keeps no POSIX ids.
func (h *windowsHost) Account(name string) (Account, error) {
	if _, err := lookupSID(name); err != nil {
		return Account{}, fmt.Errorf("the account %q does not exist: %v", name, err)
	}
	return Account{Name: name}, nil
}

// Administrator reports whether this process runs with the
// Administrators group enabled, which an elevated prompt has and a
// standard user or a filtered token does not.
func (h *windowsHost) Administrator() (bool, error) {
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return false, err
	}
	return windows.GetCurrentProcessToken().IsMember(administrators)
}

// MkdirAll creates a directory. The access control list of its parent
// applies until a Grant step replaces it.
func (h *windowsHost) MkdirAll(path string, mode os.FileMode, _ *Owner) error {
	return os.MkdirAll(path, mode)
}

// WriteFile writes a file and keeps an existing one when createOnly is
// set. The Grant step sets the access control list.
func (h *windowsHost) WriteFile(path string, data []byte, mode os.FileMode, _ *Owner, createOnly bool) error {
	if createOnly {
		switch _, err := os.Lstat(path); {
		case err == nil:
			return nil
		case !errors.Is(err, fs.ErrNotExist):
			return err
		}
	}
	return os.WriteFile(path, data, mode)
}

// Chown does nothing: Windows has no POSIX owner, and Grant sets the
// access control list instead.
func (h *windowsHost) Chown(string, os.FileMode, *Owner) error { return nil }

// RemoveFile removes a file and treats an absent file as success.
func (h *windowsHost) RemoveFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Run is a POSIX step: Windows uses the service manager API.
func (h *windowsHost) Run([]string) error { return errUnsupported }

// RegisterService creates the service, or updates its definition when it
// already exists, so a second install succeeds.
func (h *windowsHost) RegisterService(spec Spec) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("cannot connect to the service manager: %v", err)
	}
	defer func() { _ = m.Disconnect() }()
	config := mgr.Config{
		ServiceType:      windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:        mgr.StartAutomatic,
		ErrorControl:     mgr.ErrorNormal,
		BinaryPathName:   spec.CommandLine(),
		ServiceStartName: spec.Account,
		DisplayName:      spec.DisplayName,
		Description:      spec.Description,
		SidType:          windows.SERVICE_SID_TYPE_UNRESTRICTED,
	}
	service, err := m.OpenService(spec.Name)
	switch {
	case err == nil:
		defer func() { _ = service.Close() }()
		if err := service.UpdateConfig(config); err != nil {
			return fmt.Errorf("cannot update the %s service: %v", spec.Name, err)
		}
	case errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST):
		service, err = m.CreateService(spec.Name, spec.Exe, config, spec.Args...)
		if err != nil {
			return fmt.Errorf("cannot create the %s service: %v", spec.Name, err)
		}
		defer func() { _ = service.Close() }()
	default:
		return fmt.Errorf("cannot open the %s service: %v", spec.Name, err)
	}
	return setRecovery(service, spec)
}

// setRecovery makes the service manager restart the agent after a crash.
func setRecovery(service *mgr.Service, spec Spec) error {
	if len(spec.Recovery) == 0 {
		return service.ResetRecoveryActions()
	}
	actions := make([]mgr.RecoveryAction, 0, len(spec.Recovery))
	for _, delay := range spec.Recovery {
		actions = append(actions, mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: delay})
	}
	reset := uint32(spec.RecoveryReset / time.Second)
	if err := service.SetRecoveryActions(actions, reset); err != nil {
		return fmt.Errorf("cannot set the recovery actions of the %s service: %v", spec.Name, err)
	}
	return nil
}

// StartService starts the service and treats an already running service as
// success.
func (h *windowsHost) StartService(name string) error {
	err := withService(name, func(service *mgr.Service) error {
		if err := service.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
			return err
		}
		return nil
	})
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return fmt.Errorf("the %s service is not installed", name)
	}
	if err != nil {
		return fmt.Errorf("cannot start the %s service: %v", name, err)
	}
	return nil
}

// StopService stops the service and waits until the manager reports it
// stopped, so the next step never edits a service that is still running.
func (h *windowsHost) StopService(name string) error {
	err := withService(name, func(service *mgr.Service) error {
		status, err := service.Query()
		if err != nil {
			return err
		}
		if status.State == svc.Stopped {
			return nil
		}
		if _, err := service.Control(svc.Stop); err != nil {
			if errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
				return nil
			}
			return err
		}
		deadline := time.Now().Add(serviceStopTimeout)
		for time.Now().Before(deadline) {
			status, err := service.Query()
			if err != nil {
				return err
			}
			if status.State == svc.Stopped {
				return nil
			}
			time.Sleep(servicePoll)
		}
		return errors.New("it did not stop within " + serviceStopTimeout.String())
	})
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot stop the %s service: %v", name, err)
	}
	return nil
}

// DeleteService removes the service and treats an absent service as
// success.
func (h *windowsHost) DeleteService(name string) error {
	err := withService(name, func(service *mgr.Service) error {
		return service.Delete()
	})
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot delete the %s service: %v", name, err)
	}
	return nil
}

// AddGroupMember adds the service account to the local group. The group is
// named by its SID, because its name is localized, and the account is
// already a member when the installer runs a second time.
func (h *windowsHost) AddGroupMember(groupSID, member string) error {
	group, err := groupName(groupSID)
	if err != nil {
		return err
	}
	if err := changeGroupMember(procNetLocalGroupAddMembers, group, member); err != nil {
		if errors.Is(err, errorMemberInAlias) {
			return nil
		}
		return fmt.Errorf("cannot add %s to %s: %v", member, group, err)
	}
	return nil
}

// RemoveGroupMember removes the service account from the local group and
// treats an account that is not a member as success.
func (h *windowsHost) RemoveGroupMember(groupSID, member string) error {
	group, err := groupName(groupSID)
	if err != nil {
		return err
	}
	if err := changeGroupMember(procNetLocalGroupDelMembers, group, member); err != nil {
		if errors.Is(err, errorMemberNotInAlias) {
			return nil
		}
		return fmt.Errorf("cannot remove %s from %s: %v", member, group, err)
	}
	return nil
}

// Grant replaces the access control list of a file or directory: only
// Administrators and SYSTEM may write it, and the service account gets the
// requested access. Inherited entries are dropped, so a state directory
// keeps the credential away from every other account
// (docs/configuration.md section 7).
func (h *windowsHost) Grant(path string, access Access) error {
	sid, err := lookupSID(WindowsAccount)
	if err != nil {
		return fmt.Errorf("cannot resolve the service account %s: %v", WindowsAccount, err)
	}
	right := "FR"
	if access == AccessModify {
		right = "FA"
	}
	sddl := "D:P(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)(A;OICI;" + right + ";;;" + sid.String() + ")"
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return fmt.Errorf("cannot build the access control list of %s: %v", path, err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return fmt.Errorf("cannot read the access control list of %s: %v", path, err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("cannot set the access control list of %s: %v", path, err)
	}
	return nil
}

// withService opens one service and runs fn on it.
func withService(name string, fn func(*mgr.Service) error) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer func() { _ = m.Disconnect() }()
	service, err := m.OpenService(name)
	if err != nil {
		return err
	}
	defer func() { _ = service.Close() }()
	return fn(service)
}

// groupName returns the localized name of the local group with the given
// SID.
func groupName(sidString string) (string, error) {
	sid, err := windows.StringToSid(sidString)
	if err != nil {
		return "", fmt.Errorf("the group SID %s is not valid: %v", sidString, err)
	}
	name, _, _, err := sid.LookupAccount("")
	if err != nil {
		return "", fmt.Errorf("cannot resolve the group with the SID %s: %v", sidString, err)
	}
	return name, nil
}

// changeGroupMember adds a member to, or removes it from, one local group
// with the network API.
func changeGroupMember(proc *windows.LazyProc, group, member string) error {
	groupPtr, err := windows.UTF16PtrFromString(group)
	if err != nil {
		return err
	}
	memberPtr, err := windows.UTF16PtrFromString(member)
	if err != nil {
		return err
	}
	info := localGroupMembersInfo3{domainAndName: memberPtr}
	status, _, _ := proc.Call(
		0,
		uintptr(unsafe.Pointer(groupPtr)),
		uintptr(localGroupMembersLevel3),
		uintptr(unsafe.Pointer(&info)),
		1,
	)
	if status == 0 {
		return nil
	}
	return syscall.Errno(status)
}

// lookupSID resolves an account name to its SID. A virtual service account
// such as NT SERVICE\hanse-node exists only while its service does, so the
// installer registers the service before it names the account.
func lookupSID(name string) (*windows.SID, error) {
	accountPtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	sidSize := uint32(64)
	domainSize := uint32(64)
	sidBuffer := make([]byte, sidSize)
	domainBuffer := make([]uint16, domainSize)
	var use uint32
	result, _, callErr := procLookupAccountName.Call(
		0,
		uintptr(unsafe.Pointer(accountPtr)),
		uintptr(unsafe.Pointer(&sidBuffer[0])),
		uintptr(unsafe.Pointer(&sidSize)),
		uintptr(unsafe.Pointer(&domainBuffer[0])),
		uintptr(unsafe.Pointer(&domainSize)),
		uintptr(unsafe.Pointer(&use)),
	)
	if result == 0 {
		if callErr == nil || callErr == syscall.Errno(0) {
			callErr = syscall.EINVAL
		}
		return nil, callErr
	}
	return (*windows.SID)(unsafe.Pointer(&sidBuffer[0])).Copy()
}

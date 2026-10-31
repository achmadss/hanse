//go:build windows

package runner

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// advapi32 carries the two token functions that golang.org/x/sys/windows
// does not export: the restricted-token builder and the logon check.
var (
	advapi32             = windows.NewLazySystemDLL("advapi32.dll")
	procCreateRestricted = advapi32.NewProc("CreateRestrictedToken")
	procLogonUser        = advapi32.NewProc("LogonUserW")
)

// Constants of the Windows security API.
const (
	// disableMaxPrivilege removes every privilege from a restricted
	// token.
	disableMaxPrivilege = 0x1
	// logon32LogonNetwork checks a password without loading a profile.
	logon32LogonNetwork = 3
	// logon32ProviderDefault uses the default logon provider.
	logon32ProviderDefault = 0
)

// restrictedToken builds a restricted copy of the agent's token. The
// Administrators group is disabled, so the handler fails every
// administrator check even when the service is an administrator.
func restrictedToken() (windows.Token, error) {
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return 0, err
	}
	disabled := []windows.SIDAndAttributes{{Sid: administrators, Attributes: 0}}
	var out windows.Token
	result, _, callErr := procCreateRestricted.Call(
		uintptr(windows.GetCurrentProcessToken()),
		uintptr(disableMaxPrivilege),
		uintptr(len(disabled)),
		uintptr(unsafe.Pointer(&disabled[0])),
		0, 0,
		0, 0,
		uintptr(unsafe.Pointer(&out)),
	)
	if result == 0 {
		if callErr == nil || callErr == syscall.Errno(0) {
			callErr = syscall.EINVAL
		}
		return 0, callErr
	}
	return out, nil
}

// VerifyCredential checks one administrator password with the Windows
// logon API (docs/architecture.md section 8.5). It returns
// [ErrPasswordRejected] when Windows rejects the password, and another
// error when the check itself cannot run. The password is never stored.
func VerifyCredential(account, password string) error {
	if account == "" {
		return ErrNoElevationAccount
	}
	user, err := windows.UTF16PtrFromString(account)
	if err != nil {
		return err
	}
	secret, err := windows.UTF16PtrFromString(password)
	if err != nil {
		return err
	}
	var token windows.Token
	result, _, callErr := procLogonUser.Call(
		uintptr(unsafe.Pointer(user)),
		0,
		uintptr(unsafe.Pointer(secret)),
		logon32LogonNetwork,
		logon32ProviderDefault,
		uintptr(unsafe.Pointer(&token)),
	)
	if result == 0 {
		if callErr == windows.ERROR_LOGON_FAILURE || callErr == windows.ERROR_PASSWORD_EXPIRED {
			return ErrPasswordRejected
		}
		if callErr == nil || callErr == syscall.Errno(0) {
			callErr = syscall.EINVAL
		}
		return callErr
	}
	_ = token.Close()
	return nil
}

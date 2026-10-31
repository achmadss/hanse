//go:build windows

package runner

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// advapi32 and netapi32 carry the token, logon, and local-group functions
// that golang.org/x/sys/windows does not export.
var (
	advapi32             = windows.NewLazySystemDLL("advapi32.dll")
	netapi32             = windows.NewLazySystemDLL("netapi32.dll")
	procCreateRestricted = advapi32.NewProc("CreateRestrictedToken")
	procLogonUser        = advapi32.NewProc("LogonUserW")
	procNetUserGroups    = netapi32.NewProc("NetUserGetLocalGroups")
	procNetApiBufferFree = netapi32.NewProc("NetApiBufferFree")
)

// Constants of the Windows security and network APIs.
const (
	// disableMaxPrivilege removes every privilege from a restricted
	// token.
	disableMaxPrivilege = 0x1
	// logon32LogonNetwork checks a password without loading a profile.
	logon32LogonNetwork = 3
	// logon32ProviderDefault uses the default logon provider.
	logon32ProviderDefault = 0
	// maxPreferredLength asks NetUserGetLocalGroups for every entry.
	maxPreferredLength = 0xFFFFFFFF
	// lgIncludeIndirect includes the local groups the account belongs to
	// through another group.
	lgIncludeIndirect = 0x1
	// errorAccountRestricted is the account error that
	// golang.org/x/sys/windows does not define.
	errorAccountRestricted = syscall.Errno(1327)
)

// localGroupUsersInfo0 is one entry of NetUserGetLocalGroups level 0.
type localGroupUsersInfo0 struct {
	name *uint16
}

// currentProcessIsAdministrator reports whether the agent's own token holds
// the Administrators group. A standard user or a UAC-filtered process does
// not, so its handlers already run without administrator rights.
func currentProcessIsAdministrator() (bool, error) {
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return false, err
	}
	return windows.GetCurrentProcessToken().IsMember(administrators)
}

// restrictedToken builds a restricted copy of the agent's token. The
// Administrators group is disabled, so the handler fails every
// administrator check even when the service is an administrator.
//
// The agent's own token is a real handle here, not the pseudo-handle that
// GetCurrentProcessToken returns: CreateRestrictedToken needs a real token
// handle, and the restricted token is then the primary token that starts
// the handler. The access rights cover CreateRestrictedToken and the
// CreateProcessAsUser call that Go makes for SysProcAttr.Token.
func restrictedToken() (windows.Token, error) {
	var process windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_DUPLICATE|windows.TOKEN_ASSIGN_PRIMARY|windows.TOKEN_QUERY|
			windows.TOKEN_ADJUST_DEFAULT|windows.TOKEN_ADJUST_SESSIONID,
		&process); err != nil {
		return 0, err
	}
	defer func() { _ = process.Close() }()
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return 0, err
	}
	disabled := []windows.SIDAndAttributes{{Sid: administrators, Attributes: 0}}
	var out windows.Token
	result, _, callErr := procCreateRestricted.Call(
		uintptr(process),
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
// [ErrPasswordRejected] when Windows rejects the password, [ErrNotAdministrator]
// when the account is not a member of the local Administrators group, and
// a clear reason for an account that is disabled, restricted, or locked
// out. Only [ErrPasswordRejected] counts toward the agent's lockout. The
// account's own lockout policy applies to these logon attempts too.
func VerifyCredential(account, password string, _ ...string) error {
	if account == "" {
		return ErrNoElevationAccount
	}
	administrator, err := isLocalAdministrator(account)
	if err != nil {
		return fmt.Errorf("the elevation account cannot be checked: %w", err)
	}
	if !administrator {
		return fmt.Errorf("%w: %s", ErrNotAdministrator, account)
	}
	userPtr, domainPtr, err := logonNames(account)
	if err != nil {
		return err
	}
	secret, err := windows.UTF16PtrFromString(password)
	if err != nil {
		return err
	}
	var token windows.Token
	result, _, callErr := procLogonUser.Call(
		uintptr(unsafe.Pointer(userPtr)),
		uintptr(unsafe.Pointer(domainPtr)),
		uintptr(unsafe.Pointer(secret)),
		logon32LogonNetwork,
		logon32ProviderDefault,
		uintptr(unsafe.Pointer(&token)),
	)
	if result != 0 {
		_ = token.Close()
		return nil
	}
	switch callErr {
	case windows.ERROR_LOGON_FAILURE, windows.ERROR_PASSWORD_EXPIRED:
		return ErrPasswordRejected
	case errorAccountRestricted:
		return fmt.Errorf("%w: the account is restricted", ErrAccountUnusable)
	case windows.ERROR_ACCOUNT_DISABLED:
		return fmt.Errorf("%w: the account is disabled", ErrAccountUnusable)
	case windows.ERROR_ACCOUNT_LOCKED_OUT:
		return fmt.Errorf("%w: the account is locked out", ErrAccountUnusable)
	}
	if callErr == nil || callErr == syscall.Errno(0) {
		callErr = syscall.EINVAL
	}
	return fmt.Errorf("the logon check failed: %w", callErr)
}

// logonNames splits an account name for LogonUserW: DOMAIN\user keeps its
// domain, a UPN keeps its NULL domain, and a bare name uses "." for the
// local account database.
func logonNames(account string) (userPtr, domainPtr *uint16, err error) {
	var domain, user string
	switch {
	case strings.ContainsRune(account, '\\'):
		i := strings.IndexByte(account, '\\')
		domain, user = account[:i], account[i+1:]
	case strings.ContainsRune(account, '@'):
		// A UPN carries its own domain, so LogonUserW takes a NULL
		// domain.
		user = account
	default:
		// A bare name belongs to the local account database.
		domain, user = ".", account
	}
	if user == "" {
		return nil, nil, fmt.Errorf("%w: the account name is empty", ErrNoElevationAccount)
	}
	if userPtr, err = windows.UTF16PtrFromString(user); err != nil {
		return nil, nil, err
	}
	if domain != "" {
		if domainPtr, err = windows.UTF16PtrFromString(domain); err != nil {
			return nil, nil, err
		}
	}
	return userPtr, domainPtr, nil
}

// isLocalAdministrator reports whether the named account belongs to the
// local Administrators group, directly or through another group. It
// compares group names with the localized name of the Administrators group
// and never relies on a logon token: a network-logon token of a local
// administrator can be filtered by UAC, which would hide the membership.
func isLocalAdministrator(account string) (bool, error) {
	name, err := windows.UTF16PtrFromString(account)
	if err != nil {
		return false, err
	}
	var buffer *byte
	var read, total uint32
	result, _, callErr := procNetUserGroups.Call(
		0,
		uintptr(unsafe.Pointer(name)),
		0,
		lgIncludeIndirect,
		uintptr(unsafe.Pointer(&buffer)),
		maxPreferredLength,
		uintptr(unsafe.Pointer(&read)),
		uintptr(unsafe.Pointer(&total)),
	)
	if result != 0 {
		if callErr == nil || callErr == syscall.Errno(0) {
			callErr = syscall.Errno(result)
		}
		return false, callErr
	}
	if buffer == nil {
		return false, nil
	}
	defer func() { _, _, _ = procNetApiBufferFree.Call(uintptr(unsafe.Pointer(buffer))) }()
	group, err := administratorsGroupName()
	if err != nil {
		return false, err
	}
	entries := unsafe.Slice((*localGroupUsersInfo0)(unsafe.Pointer(buffer)), int(read))
	for i := range entries {
		if entries[i].name != nil && strings.EqualFold(windows.UTF16PtrToString(entries[i].name), group) {
			return true, nil
		}
	}
	return false, nil
}

// administratorsGroupName returns the localized name of the local
// Administrators group, so the membership check works on every Windows
// language.
func administratorsGroupName() (string, error) {
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return "", err
	}
	name, _, _, err := administrators.LookupAccount("")
	if err != nil {
		return "", err
	}
	return name, nil
}

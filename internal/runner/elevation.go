package runner

import "errors"

// ErrPasswordRejected reports that the platform rejected an elevation
// password. It carries no password and no platform text, so it is safe to
// pass to a log or an error message.
var ErrPasswordRejected = errors.New("the elevation password was rejected")

// ErrNoElevationAccount reports that the platform needs an account name
// for the password check, but agent.yaml sets none.
var ErrNoElevationAccount = errors.New("no elevation account is configured on this node")

// ErrNotAdministrator reports that the configured elevation account is not
// a member of the local Administrators group. Its password would grant
// full service rights without the account being an administrator, so the
// run is refused without counting a wrong password.
var ErrNotAdministrator = errors.New("the elevation account is not a local administrator")

// ErrAccountUnusable reports that the elevation account cannot log on, for
// example because it is disabled, restricted, or locked out. It is a
// configuration problem, not a wrong password, so it does not count toward
// the agent's lockout.
var ErrAccountUnusable = errors.New("the elevation account cannot log on")

package runner

import "errors"

// ErrPasswordRejected reports that the platform rejected an elevation
// password. It carries no password and no platform text, so it is safe to
// pass to a log or an error message.
var ErrPasswordRejected = errors.New("the elevation password was rejected")

// ErrNoElevationAccount reports that the platform needs an account name
// for the password check, but agent.yaml sets none.
var ErrNoElevationAccount = errors.New("no elevation account is configured on this node")

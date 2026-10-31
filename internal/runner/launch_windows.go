//go:build windows

package runner

import "golang.org/x/sys/windows"

// processOptions is the platform state of one launch that must outlive
// Start. A restricted token is only needed until the process exists.
type processOptions struct {
	token windows.Token
}

// close releases the restricted token. The child holds its own copy.
func (o *processOptions) close() {
	if o != nil && o.token != 0 {
		_ = o.token.Close()
		o.token = 0
	}
}

// preparePrivileged leaves a Windows launch unchanged. The agent service
// already holds its full set of rights, so a privileged handler inherits
// them and no elevation prompt appears (docs/architecture.md section 8.4).
func preparePrivileged(_ *Runner, _ *launchPlan) error { return nil }

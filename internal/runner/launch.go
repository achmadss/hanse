package runner

import (
	"fmt"
	"os"
	"time"

	herrors "github.com/achmadss/hanse/internal/errors"
)

// supervisorSubcommand is the `hanse-node` mode that supervises one
// privileged handler as root (docs/handler-contract.md section 7.2).
const supervisorSubcommand = "supervise"

// launchPlan is one process the runner starts: the entry command, its
// working directory, its environment, and the work files of the call. A
// privileged POSIX run replaces the entry command with sudo and the root
// supervisor, which starts the handler from this plan.
type launchPlan struct {
	Entry      []string
	Dir        string
	Env        []string
	WorkDir    string
	CancelFile string
	Grace      time.Duration
}

// SupervisorControl is the control file that the runner writes for the
// root supervisor of a privileged POSIX run. It holds the handler's
// environment and working directory, never a credential: the password
// reaches sudo on its own stdin (docs/agent-protocol.md section 9).
type SupervisorControl struct {
	Dir string   `json:"dir"`
	Env []string `json:"env"`
}

// supervisor returns the command prefix that starts the root supervisor.
func (r *Runner) supervisor() ([]string, error) {
	if r != nil && len(r.Supervisor) > 0 {
		return append([]string(nil), r.Supervisor...), nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, herrors.New(herrors.Internal,
			fmt.Sprintf("the supervisor executable cannot be found: %s", err), nil)
	}
	return []string{exe, supervisorSubcommand}, nil
}

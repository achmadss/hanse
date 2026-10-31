//go:build windows

package main

import (
	"context"
	"io"

	"golang.org/x/sys/windows/svc"

	"github.com/achmadss/hanse/internal/service"
)

// runAsService runs the agent as a Windows service when the service
// manager started this process (docs/architecture.md section 8.4). The
// manager sends Stop and Shutdown as control requests; the handler cancels
// the agent's context, so the agent stops its handlers and exits before
// the manager kills the process.
func runAsService(configPath string, stderr io.Writer) (bool, int) {
	isService, err := svc.IsWindowsService()
	if err != nil || !isService {
		return false, 0
	}
	handler := &serviceHandler{configPath: configPath, stderr: stderr}
	if err := svc.Run(service.WindowsName, handler); err != nil {
		printf(stderr, "hanse-node: the service manager refused the agent: %v\n", err)
		return true, exitFail
	}
	return true, exitOK
}

// serviceHandler is the agent's interface to the service manager. Its
// agent is injectable, so a test drives the control requests without a
// service manager.
type serviceHandler struct {
	configPath string
	stderr     io.Writer
	// run is the agent. Nil runs the real one.
	run func(ctx context.Context) int
}

// Execute starts the agent and reports its state to the service manager.
// Stop and Shutdown cancel the agent and wait for it to return, and that
// stop is always reported as success, because the manager asked for it and
// a failure would run the recovery restart. An agent that exits on its own
// with a non-zero code is reported as a service failure.
func (h *serviceHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	run := h.run
	if run == nil {
		run = func(ctx context.Context) int { return agentRun(ctx, h.configPath, h.stderr, true) }
	}
	changes <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- run(ctx) }()
	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case code := <-done:
			changes <- svc.Status{State: svc.StopPending}
			return false, uint32(code)
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				changes <- request.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				cancel()
				<-done
				// The agent returns a non-zero code for a cancelled context,
				// but this stop was asked for, so the service stopped
				// cleanly. A non-zero code here would be a failure that the
				// recovery actions may act on, and a requested stop must
				// never restart the agent.
				return false, 0
			}
		}
	}
}

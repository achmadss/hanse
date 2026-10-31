//go:build windows

package main

import (
	"context"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
)

// TestServiceHandlerStopsTheAgent covers a Stop request from the service
// manager: the agent's context is cancelled, the handler waits for the
// agent to return, and the manager sees the service stop.
func TestServiceHandlerStopsTheAgent(t *testing.T) {
	agentStopped := make(chan struct{})
	handler := &serviceHandler{run: func(ctx context.Context) int {
		<-ctx.Done()
		close(agentStopped)
		return exitOK
	}}
	requests := make(chan svc.ChangeRequest, 1)
	changes := make(chan svc.Status, 4)
	type result struct {
		accepted bool
		code     uint32
	}
	finished := make(chan result, 1)
	go func() {
		accepted, code := handler.Execute(nil, requests, changes)
		finished <- result{accepted: accepted, code: code}
	}()

	if status := nextStatus(t, changes); status.State != svc.StartPending {
		t.Fatalf("first status = %d, want StartPending", status.State)
	}
	status := nextStatus(t, changes)
	if status.State != svc.Running {
		t.Fatalf("second status = %d, want Running", status.State)
	}
	if status.Accepts&svc.AcceptStop == 0 {
		t.Error("the service does not accept a stop request")
	}

	requests <- svc.ChangeRequest{Cmd: svc.Stop}
	if status := nextStatus(t, changes); status.State != svc.StopPending {
		t.Fatalf("third status = %d, want StopPending", status.State)
	}
	select {
	case <-agentStopped:
	case <-time.After(time.Second):
		t.Fatal("the agent was not stopped")
	}
	select {
	case got := <-finished:
		if got.accepted {
			t.Error("the handler accepted control requests after a stop")
		}
		if got.code != exitOK {
			t.Errorf("exit code = %d, want %d", got.code, exitOK)
		}
	case <-time.After(time.Second):
		t.Fatal("the handler did not return after the stop")
	}
}

// TestServiceHandlerStopIsNeverAFailure covers an agent that returns a
// non-zero code for a cancelled context: the manager asked for the stop, so
// the service reports success. A non-zero code would look like a failure and
// could start the recovery action.
func TestServiceHandlerStopIsNeverAFailure(t *testing.T) {
	for _, command := range []svc.Cmd{svc.Stop, svc.Shutdown} {
		handler := &serviceHandler{run: func(ctx context.Context) int {
			<-ctx.Done()
			return exitFail
		}}
		requests := make(chan svc.ChangeRequest, 1)
		changes := make(chan svc.Status, 4)
		type result struct {
			accepted bool
			code     uint32
		}
		finished := make(chan result, 1)
		go func() {
			accepted, code := handler.Execute(nil, requests, changes)
			finished <- result{accepted: accepted, code: code}
		}()
		nextStatus(t, changes)
		nextStatus(t, changes)
		requests <- svc.ChangeRequest{Cmd: command}
		select {
		case got := <-finished:
			if got.code != 0 {
				t.Errorf("%d: exit code = %d, want 0 after a requested stop", command, got.code)
			}
		case <-time.After(time.Second):
			t.Fatalf("%d: the handler did not return after the stop", command)
		}
	}
}

// TestServiceHandlerAnswersInterrogate covers the status request that the
// service manager sends while the agent runs.
func TestServiceHandlerAnswersInterrogate(t *testing.T) {
	handler := &serviceHandler{run: func(ctx context.Context) int {
		<-ctx.Done()
		return exitOK
	}}
	requests := make(chan svc.ChangeRequest, 2)
	changes := make(chan svc.Status, 8)
	done := make(chan struct{})
	go func() {
		_, _ = handler.Execute(nil, requests, changes)
		close(done)
	}()
	nextStatus(t, changes)
	nextStatus(t, changes)
	requests <- svc.ChangeRequest{
		Cmd:           svc.Interrogate,
		CurrentStatus: svc.Status{State: svc.Running},
	}
	if status := nextStatus(t, changes); status.State != svc.Running {
		t.Errorf("interrogate answer = %d, want Running", status.State)
	}
	requests <- svc.ChangeRequest{Cmd: svc.Stop}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the handler did not stop")
	}
}

// TestServiceHandlerReportsACrash covers an agent that exits with a
// failure: the handler reports the service as failed, so the service
// manager carries out the configured recovery action.
func TestServiceHandlerReportsACrash(t *testing.T) {
	handler := &serviceHandler{run: func(context.Context) int { return exitFail }}
	requests := make(chan svc.ChangeRequest)
	changes := make(chan svc.Status, 4)
	type result struct {
		accepted bool
		code     uint32
	}
	finished := make(chan result, 1)
	go func() {
		accepted, code := handler.Execute(nil, requests, changes)
		finished <- result{accepted: accepted, code: code}
	}()
	select {
	case got := <-finished:
		if got.code != exitFail {
			t.Errorf("exit code = %d, want %d", got.code, exitFail)
		}
	case <-time.After(time.Second):
		t.Fatal("the handler did not report the crash")
	}
}

// nextStatus reads one status that the handler reports to the service
// manager.
func nextStatus(t *testing.T, changes <-chan svc.Status) svc.Status {
	t.Helper()
	select {
	case status := <-changes:
		return status
	case <-time.After(time.Second):
		t.Fatal("the handler reported no status")
		return svc.Status{}
	}
}

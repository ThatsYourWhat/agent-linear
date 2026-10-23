package main

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

func runProcess(ctx context.Context, cmd *exec.Cmd) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return err
	}
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
			if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				_ = cmd.Process.Kill()
			}
		case <-done:
		}
	}()
	err := cmd.Wait()
	// A completed CLI must not leave children holding the inherited worker lock.
	if killErr := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
		err = errors.Join(err, killErr)
	}
	close(done)
	<-stopped
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

//go:build windows

package main

import (
	"context"
	"os/exec"
	"strconv"
	"time"
)

const processWaitDelay = 2 * time.Second

func configureCommandProcessTree(cmd *exec.Cmd) {
	cmd.WaitDelay = processWaitDelay
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cancelProcessTree(processCleanupTimeout, cmd.Process.Pid, func(ctx context.Context, pid int) error {
			return exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
		}, cmd.Process.Kill)
	}
}

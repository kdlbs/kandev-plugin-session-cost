//go:build windows

package main

import (
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
		if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run(); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}

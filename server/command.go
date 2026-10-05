package main

import (
	"context"
	"os/exec"
	"time"
)

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	configureCommandCancellation(cmd)
	cmd.WaitDelay = 2 * time.Second
	return cmd.Output()
}

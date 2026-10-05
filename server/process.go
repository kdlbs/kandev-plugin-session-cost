package main

import (
	"context"
	"os/exec"
)

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	configureCommandProcessTree(cmd)
	return cmd.Output()
}

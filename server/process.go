package main

import (
	"context"
	"os/exec"
	"time"
)

const processCleanupTimeout = 2 * time.Second

type processTreeCleanup func(context.Context, int) error

// cancelProcessTree bounds tree cleanup independently of the report context.
// The fallback kills the launched process if the platform helper times out or fails.
func cancelProcessTree(timeout time.Duration, pid int, cleanup processTreeCleanup, fallback func() error) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := cleanup(ctx, pid); err != nil {
		return fallback()
	}
	return nil
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	configureCommandProcessTree(cmd)
	return cmd.Output()
}

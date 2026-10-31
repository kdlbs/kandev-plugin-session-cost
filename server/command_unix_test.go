//go:build !windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCommandCancellationKillsDescendants(t *testing.T) {
	dir := t.TempDir()
	started, survived := filepath.Join(dir, "started"), filepath.Join(dir, "survived")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := runCommand(ctx, "sh", "-c", `touch "$1"; (sleep 0.3; touch "$2") & wait`, "fixture", started, survived)
		finished <- err
	}()
	require.Eventually(t, func() bool { _, err := os.Stat(started); return err == nil }, time.Second, time.Millisecond)
	cancel()
	require.Error(t, <-finished)
	// Wait past the child's scheduled write; killing only its parent leaves
	// that write alive and would violate the one-process collection budget.
	time.Sleep(400 * time.Millisecond)
	_, err := os.Stat(survived)
	require.ErrorIs(t, err, os.ErrNotExist)
}

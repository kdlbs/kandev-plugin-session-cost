package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCancelProcessTreeBoundsCleanupAndUsesFallback(t *testing.T) {
	const timeout = 20 * time.Millisecond
	started := time.Now()
	fallbackCalled := false
	err := cancelProcessTree(timeout, 123, func(ctx context.Context, pid int) error {
		require.Equal(t, 123, pid)
		deadline, ok := ctx.Deadline()
		require.True(t, ok, "cleanup must have an independent deadline")
		require.LessOrEqual(t, time.Until(deadline), timeout)
		<-ctx.Done()
		return ctx.Err()
	}, func() error {
		fallbackCalled = true
		return nil
	})

	require.NoError(t, err)
	require.True(t, fallbackCalled)
	require.Less(t, time.Since(started), time.Second, "stalled tree cleanup must not block process cancellation")
}

func TestCancelProcessTreeSkipsFallbackWhenCleanupSucceeds(t *testing.T) {
	fallbackCalled := false
	err := cancelProcessTree(time.Second, 123, func(ctx context.Context, pid int) error {
		require.Equal(t, 123, pid)
		require.NoError(t, ctx.Err())
		return nil
	}, func() error {
		fallbackCalled = true
		return nil
	})

	require.NoError(t, err)
	require.False(t, fallbackCalled)
}

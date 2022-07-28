package testutils

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var (
	ConnectTimeout = 30 * time.Second
)

func WithTimeout(t *testing.T, f func() string) {
	ctx, cancel := context.WithTimeout(context.Background(), ConnectTimeout)
	defer cancel()
	lastErr := ""
	for {
		select {
		case <-ctx.Done():
			require.Empty(t, lastErr)
		case <-time.After(10 * time.Millisecond):
			lastErr = f()
			if lastErr == "" {
				return
			}
		}
	}
}

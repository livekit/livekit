package testutils

import (
	"context"
	"testing"
	"time"

	"github.com/livekit/protocol/logger"
)

var (
	SyncDelay      = 100 * time.Millisecond
	ConnectTimeout = 10 * time.Second
)

func WithTimeout(t *testing.T, description string, f func() bool) bool {
	logger.Infow(description)
	ctx, cancel := context.WithTimeout(context.Background(), ConnectTimeout)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			t.Fatal("timed out: " + description)
			return false
		case <-time.After(10 * time.Millisecond):
			if f() {
				return true
			}
		}
	}
}

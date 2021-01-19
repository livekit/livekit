package routing_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/proto/livekit"
)

func TestIsAvailable(t *testing.T) {
	t.Run("still available", func(t *testing.T) {
		n := &livekit.Node{
			Stats: &livekit.NodeStats{
				UpdatedAt: time.Now().Unix() - 10,
			},
		}
		assert.True(t, routing.IsAvailable(n))
	})

	t.Run("expired", func(t *testing.T) {
		n := &livekit.Node{
			Stats: &livekit.NodeStats{
				UpdatedAt: time.Now().Unix() - 20,
			},
		}
		assert.False(t, routing.IsAvailable(n))
	})
}

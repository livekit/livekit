package selector_test

import (
	"testing"
	"time"

	"github.com/livekit/livekit-server/pkg/routing/selector"
	livekit "github.com/livekit/protocol/proto"
	"github.com/stretchr/testify/require"
)

func TestIsAvailable(t *testing.T) {
	t.Run("still available", func(t *testing.T) {
		n := &livekit.Node{
			Stats: &livekit.NodeStats{
				UpdatedAt: time.Now().Unix() - 3,
			},
		}
		require.True(t, selector.IsAvailable(n))
	})

	t.Run("expired", func(t *testing.T) {
		n := &livekit.Node{
			Stats: &livekit.NodeStats{
				UpdatedAt: time.Now().Unix() - 20,
			},
		}
		require.False(t, selector.IsAvailable(n))
	})
}

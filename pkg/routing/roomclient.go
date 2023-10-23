package routing

import (
	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	protopsrpc "github.com/livekit/protocol/psrpc"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
	"github.com/livekit/psrpc/pkg/middleware"
)

func NewRoomClient(nodeID livekit.NodeID, bus psrpc.MessageBus, config config.PSRPCConfig) (rpc.TypedRoomClient, error) {
	return rpc.NewTypedRoomClient(
		nodeID,
		bus,
		protopsrpc.WithClientLogger(logger.GetLogger()),
		middleware.WithClientMetrics(prometheus.PSRPCMetricsObserver{}),
		psrpc.WithClientChannelSize(config.BufferSize),
		middleware.WithRPCRetries(middleware.RetryOptions{
			MaxAttempts: config.MaxAttempts,
			Timeout:     config.Timeout,
			Backoff:     config.Backoff,
		}),
	)
}

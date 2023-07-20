package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/psrpc"
)

func init() {
	prometheus.Init("node", livekit.NodeType_CONTROLLER, "test")
}

func TestSignal(t *testing.T) {
	bus := psrpc.NewLocalMessageBus()
	cfg := config.SignalRelayConfig{
		Enabled:          false,
		RetryTimeout:     30 * time.Second,
		MinRetryInterval: 500 * time.Millisecond,
		MaxRetryInterval: 5 * time.Second,
		StreamBufferSize: 1000,
	}

	reqMessageIn := &livekit.SignalRequest{
		Message: &livekit.SignalRequest_Ping{Ping: 123},
	}
	resMessageIn := &livekit.SignalResponse{
		Message: &livekit.SignalResponse_Pong{Pong: 321},
	}

	var reqMessageOut proto.Message
	var resErr error
	done := make(chan struct{})

	client, err := routing.NewSignalClient(livekit.NodeID("node0"), bus, cfg)
	require.NoError(t, err)

	server, err := NewSignalServer(livekit.NodeID("node1"), "region", bus, cfg, func(
		ctx context.Context,
		roomName livekit.RoomName,
		pi routing.ParticipantInit,
		connectionID livekit.ConnectionID,
		requestSource routing.MessageSource,
		responseSink routing.MessageSink,
	) error {
		go func() {
			reqMessageOut = <-requestSource.ReadChan()
			resErr = responseSink.WriteMessage(resMessageIn)
			responseSink.Close()
			close(done)
		}()
		return nil
	})
	require.NoError(t, err)

	err = server.Start()
	require.NoError(t, err)

	_, reqSink, resSource, err := client.StartParticipantSignal(
		context.Background(),
		livekit.RoomName("room1"),
		routing.ParticipantInit{},
		livekit.NodeID("node1"),
	)
	require.NoError(t, err)

	err = reqSink.WriteMessage(reqMessageIn)
	require.NoError(t, err)

	<-done
	require.True(t, proto.Equal(reqMessageIn, reqMessageOut), "req message should match %s %s", protojson.Format(reqMessageIn), protojson.Format(reqMessageOut))
	require.NoError(t, resErr)

	resMessageOut := <-resSource.ReadChan()
	require.True(t, proto.Equal(resMessageIn, resMessageOut), "res message should match %s %s", protojson.Format(resMessageIn), protojson.Format(resMessageOut))
}

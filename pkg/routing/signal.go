package routing

import (
	"context"

	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/psrpc"
	"github.com/livekit/psrpc/middleware"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate

//counterfeiter:generate . SignalClient
type SignalClient interface {
	StartParticipantSignal(ctx context.Context, roomName livekit.RoomName, pi ParticipantInit, nodeID livekit.NodeID) (connectionID livekit.ConnectionID, reqSink MessageSink, resSource MessageSource, err error)
}

type signalClient struct {
	nodeID livekit.NodeID
	client rpc.TypedSignalClient
}

func NewSignalClient(nodeID livekit.NodeID, bus psrpc.MessageBus, config config.SignalRelayConfig) (SignalClient, error) {
	ri := middleware.NewStreamRetryInterceptorFactory(middleware.RetryOptions{
		MaxAttempts: config.MaxAttempts,
		Timeout:     config.Timeout,
		Backoff:     config.Backoff,
	})
	c, err := rpc.NewTypedSignalClient(nodeID, bus, psrpc.WithClientStreamInterceptors(ri))
	if err != nil {
		return nil, err
	}

	return &signalClient{
		nodeID: nodeID,
		client: c,
	}, nil
}

func (r *signalClient) StartParticipantSignal(
	ctx context.Context,
	roomName livekit.RoomName,
	pi ParticipantInit,
	nodeID livekit.NodeID,
) (
	connectionID livekit.ConnectionID,
	reqSink MessageSink,
	resSource MessageSource,
	err error,
) {
	connectionID = livekit.ConnectionID(utils.NewGuid("CO_"))
	ss, err := pi.ToStartSession(roomName, connectionID)
	if err != nil {
		return
	}

	logger.Debugw(
		"starting signal connection",
		"room", roomName,
		"reqNodeID", nodeID,
		"participant", pi.Identity,
		"connectionID", connectionID,
	)

	stream, err := r.client.RelaySignal(ctx, nodeID)
	if err != nil {
		return
	}

	err = stream.Send(&rpc.RelaySignalRequest{StartSession: ss})
	if err != nil {
		stream.Close(err)
		return
	}

	resChan := NewDefaultMessageChannel()

	go func() {
		var err error
		for msg := range stream.Channel() {
			if err = resChan.WriteMessage(msg.Response); err != nil {
				break
			}
		}

		logger.Debugw("participant signal stream closed",
			"error", err,
			"room", ss.RoomName,
			"participant", ss.Identity,
			"connectionID", connectionID,
		)

		resChan.Close()
	}()

	return connectionID, &relaySignalRequestSink{stream}, resChan, nil
}

type relaySignalRequestSink struct {
	psrpc.ClientStream[*rpc.RelaySignalRequest, *rpc.RelaySignalResponse]
}

func (s *relaySignalRequestSink) Close() {
	s.ClientStream.Close(nil)
}

func (s *relaySignalRequestSink) IsClosed() bool {
	return s.Context().Err() != nil
}

func (s *relaySignalRequestSink) WriteMessage(msg proto.Message) error {
	return s.Send(&rpc.RelaySignalRequest{Request: msg.(*livekit.SignalRequest)})
}

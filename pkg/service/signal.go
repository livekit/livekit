package service

import (
	"context"

	"github.com/pkg/errors"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
)

type SessionHandler func(
	ctx context.Context,
	roomName livekit.RoomName,
	pi routing.ParticipantInit,
	connectionID livekit.ConnectionID,
	requestSource routing.MessageSource,
	responseSink routing.MessageSink,
) error

type SignalServer struct {
	server rpc.TypedSignalServer
}

func NewSignalServer(
	nodeID livekit.NodeID,
	region string,
	bus psrpc.MessageBus,
	sessionHandler SessionHandler,
) (*SignalServer, error) {
	s, err := rpc.NewTypedSignalServer(nodeID, &signalService{region, sessionHandler}, bus)
	if err != nil {
		return nil, err
	}
	logger.Debugw("starting relay signal server", "topic", nodeID)
	if err := s.RegisterRelaySignalTopic(nodeID); err != nil {
		return nil, err
	}

	return &SignalServer{s}, nil
}

func NewDefaultSignalServer(
	currentNode routing.LocalNode,
	bus psrpc.MessageBus,
	router routing.Router,
	roomManager *RoomManager,
) (r *SignalServer, err error) {
	sessionHandler := func(
		ctx context.Context,
		roomName livekit.RoomName,
		pi routing.ParticipantInit,
		connectionID livekit.ConnectionID,
		requestSource routing.MessageSource,
		responseSink routing.MessageSink,
	) error {
		prometheus.IncrementParticipantRtcInit(1)
		return roomManager.StartSession(ctx, roomName, pi, requestSource, responseSink)
	}

	return NewSignalServer(livekit.NodeID(currentNode.Id), currentNode.Region, bus, sessionHandler)
}

func (r *SignalServer) Stop() {
	r.server.Kill()
}

type signalService struct {
	region         string
	sessionHandler SessionHandler
}

func (r *signalService) RelaySignal(stream psrpc.ServerStream[*rpc.RelaySignalResponse, *rpc.RelaySignalRequest]) (err error) {
	// copy the context to prevent a race between the session handler closing
	// and the delivery of any parting messages from the client. take care to
	// copy the incoming rpc headers to avoid dropping any session vars.
	ctx, cancel := context.WithCancel(psrpc.NewContextWithIncomingHeader(context.Background(), psrpc.IncomingHeader(stream.Context())))
	defer cancel()

	req, ok := <-stream.Channel()
	if !ok {
		return nil
	}

	ss := req.StartSession
	if ss == nil {
		return errors.New("expected start session message")
	}

	pi, err := routing.ParticipantInitFromStartSession(ss, r.region)
	if err != nil {
		return errors.Wrap(err, "failed to read participant from session")
	}

	reqChan := routing.NewDefaultMessageChannel()
	defer reqChan.Close()

	err = r.sessionHandler(
		ctx,
		livekit.RoomName(ss.RoomName),
		*pi,
		livekit.ConnectionID(ss.ConnectionId),
		reqChan,
		&relaySignalResponseSink{stream},
	)
	if err != nil {
		logger.Errorw("could not handle new participant", err,
			"room", ss.RoomName,
			"participant", ss.Identity,
			"connectionID", ss.ConnectionId,
		)
	}

	for msg := range stream.Channel() {
		if err = reqChan.WriteMessage(msg.Request); err != nil {
			break
		}
	}

	logger.Debugw("participant signal stream closed",
		"room", ss.RoomName,
		"participant", ss.Identity,
		"connectionID", ss.ConnectionId,
	)
	return
}

type relaySignalResponseSink struct {
	psrpc.ServerStream[*rpc.RelaySignalResponse, *rpc.RelaySignalRequest]
}

func (s *relaySignalResponseSink) Close() {
	s.ServerStream.Close(nil)
}

func (s *relaySignalResponseSink) IsClosed() bool {
	return s.Context().Err() != nil
}

func (s *relaySignalResponseSink) WriteMessage(msg proto.Message) error {
	return s.Send(&rpc.RelaySignalResponse{Response: msg.(*livekit.SignalResponse)})
}

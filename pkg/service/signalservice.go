package service

import (
	"context"

	"github.com/pkg/errors"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils"
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

type SignalService struct {
	nodeID         livekit.NodeID
	region         string
	sessionHandler SessionHandler
	signalClient   rpc.TypedSignalClient
	signalServer   rpc.TypedSignalServer
}

func NewSignalService(
	nodeID livekit.NodeID,
	region string,
	bus psrpc.MessageBus,
	router routing.Router,
	sessionHandler SessionHandler,
) (r *SignalService, err error) {
	r = &SignalService{
		nodeID:         nodeID,
		region:         region,
		sessionHandler: sessionHandler,
	}

	router.OnNewSignalClient(r.StartParticipantSignal)

	r.signalClient, err = rpc.NewTypedSignalClient(nodeID, bus)
	if err != nil {
		return nil, err
	}

	r.signalServer, err = rpc.NewTypedSignalServer(nodeID, r, bus)
	if err != nil {
		return nil, err
	}
	if err := r.signalServer.RegisterRelaySignalTopic(nodeID); err != nil {
		return nil, err
	}

	return r, nil
}

func NewDefaultSignalService(
	currentNode routing.LocalNode,
	bus psrpc.MessageBus,
	router routing.Router,
	roomManager *RoomManager,
) (r *SignalService, err error) {
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

	return NewSignalService(livekit.NodeID(currentNode.Id), currentNode.Region, bus, router, sessionHandler)
}

func (r *SignalService) Stop() {
	r.signalServer.Kill()
}

func (r *SignalService) StartParticipantSignal(
	roomName livekit.RoomName,
	pi routing.ParticipantInit,
	nodeID livekit.NodeID,
) (
	connectionID livekit.ConnectionID,
	reqSink routing.MessageSink,
	resSource routing.MessageSource,
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

	stream, err := r.signalClient.RelaySignal(context.Background(), nodeID)
	if err != nil {
		return
	}

	err = stream.Send(&rpc.RelaySignalRequest{StartSession: ss})
	if err != nil {
		stream.Close(err)
		return
	}

	resChan := routing.NewDefaultMessageChannel()

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

func (r *SignalService) RelaySignal(stream psrpc.ServerStream[*rpc.RelaySignalResponse, *rpc.RelaySignalRequest]) (err error) {
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
	logger.Debugw("writing signal message", "message", protojson.Format(msg))
	return s.Send(&rpc.RelaySignalRequest{Request: msg.(*livekit.SignalRequest)})
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
	logger.Debugw("writing signal message", "message", protojson.Format(msg))
	return s.Send(&rpc.RelaySignalResponse{Response: msg.(*livekit.SignalResponse)})
}

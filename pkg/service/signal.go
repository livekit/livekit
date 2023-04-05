package service

import (
	"context"
	"fmt"
	"sync"

	"github.com/pkg/errors"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
	"github.com/livekit/psrpc/middleware"
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
	config config.SignalRelayConfig,
	sessionHandler SessionHandler,
) (*SignalServer, error) {
	s, err := rpc.NewTypedSignalServer(
		nodeID,
		&signalService{region, sessionHandler},
		bus,
		middleware.WithServerMetrics(prometheus.PSRPCMetricsObserver{}),
		psrpc.WithServerStreamInterceptors(middleware.NewStreamRetryInterceptorFactory(middleware.RetryOptions{
			MaxAttempts: config.MaxAttempts,
			Timeout:     config.Timeout,
			Backoff:     config.Backoff,
		})),
		psrpc.WithServerChannelSize(config.StreamBufferSize),
	)
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
	config config.SignalRelayConfig,
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

	return NewSignalServer(livekit.NodeID(currentNode.Id), currentNode.Region, bus, config, sessionHandler)
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

	l := logger.GetLogger().WithValues(
		"room", ss.RoomName,
		"participant", ss.Identity,
		"connectionID", ss.ConnectionId,
	)

	reqChan := routing.NewDefaultMessageChannel()
	defer reqChan.Close()

	err = r.sessionHandler(
		ctx,
		livekit.RoomName(ss.RoomName),
		*pi,
		livekit.ConnectionID(ss.ConnectionId),
		reqChan,
		&relaySignalResponseSink{
			ServerStream: stream,
			logger:       l,
		},
	)
	if err != nil {
		l.Errorw("could not handle new participant", err)
	}

	for msg := range stream.Channel() {
		if err = reqChan.WriteMessage(msg.Request); err != nil {
			break
		}
	}

	l.Debugw("participant signal stream closed")
	return
}

type relaySignalResponseSink struct {
	psrpc.ServerStream[*rpc.RelaySignalResponse, *rpc.RelaySignalRequest]
	logger logger.Logger

	mu      sync.Mutex
	queue   []*livekit.SignalResponse
	writing bool
}

func (s *relaySignalResponseSink) Close() {
	s.ServerStream.Close(nil)
}

func (s *relaySignalResponseSink) IsClosed() bool {
	return s.Context().Err() != nil
}

func (s *relaySignalResponseSink) write() {
	for {
		s.mu.Lock()
		var msg *livekit.SignalResponse
		if len(s.queue) != 0 && !s.IsClosed() {
			msg = s.queue[0]
			s.queue = s.queue[1:]
		} else {
			s.writing = false
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()

		if err := s.Send(&rpc.RelaySignalResponse{Response: msg}); err != nil {
			s.logger.Warnw(
				"could not send message to participant", err,
				"messageType", fmt.Sprintf("%T", msg.Message),
			)
		}
	}
}

func (s *relaySignalResponseSink) WriteMessage(msg proto.Message) error {
	if err := s.Context().Err(); err != nil {
		return err
	}

	s.mu.Lock()
	s.queue = append(s.queue, msg.(*livekit.SignalResponse))
	if !s.writing {
		s.writing = true
		go s.write()
	}
	s.mu.Unlock()
	return nil
}

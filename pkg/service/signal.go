package service

import (
	"context"

	"github.com/pkg/errors"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
	"github.com/livekit/psrpc/pkg/metadata"
	"github.com/livekit/psrpc/pkg/middleware"
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
		&signalService{region, sessionHandler, config},
		bus,
		middleware.WithServerMetrics(prometheus.PSRPCMetricsObserver{}),
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

		if rr, ok := router.(*routing.RedisRouter); ok {
			rtcNode, err := router.GetNodeForRoom(ctx, roomName)
			if err != nil {
				return err
			}

			if rtcNode.Id != currentNode.Id {
				err = routing.ErrIncorrectRTCNode
				logger.Errorw("called participant on incorrect node", err,
					"rtcNode", rtcNode,
				)
				return err
			}

			pKey := routing.ParticipantKeyLegacy(roomName, pi.Identity)
			pKeyB62 := routing.ParticipantKey(roomName, pi.Identity)

			// RTC session should start on this node
			if err := rr.SetParticipantRTCNode(pKey, pKeyB62, currentNode.Id); err != nil {
				return err
			}
		}

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
	config         config.SignalRelayConfig
}

func (r *signalService) RelaySignal(stream psrpc.ServerStream[*rpc.RelaySignalResponse, *rpc.RelaySignalRequest]) (err error) {
	// copy the context to prevent a race between the session handler closing
	// and the delivery of any parting messages from the client. take care to
	// copy the incoming rpc headers to avoid dropping any session vars.
	ctx, cancel := context.WithCancel(metadata.NewContextWithIncomingHeader(context.Background(), metadata.IncomingHeader(stream.Context())))
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
		"connID", ss.ConnectionId,
	)

	sink := routing.NewSignalMessageSink(routing.SignalSinkParams[*rpc.RelaySignalResponse, *rpc.RelaySignalRequest]{
		Logger:       l,
		Stream:       stream,
		Config:       r.config,
		Writer:       signalResponseMessageWriter{},
		ConnectionID: livekit.ConnectionID(ss.ConnectionId),
	})
	reqChan := routing.NewDefaultMessageChannel(livekit.ConnectionID(ss.ConnectionId))

	go func() {
		err := routing.CopySignalStreamToMessageChannel[*rpc.RelaySignalResponse, *rpc.RelaySignalRequest](
			stream,
			reqChan,
			signalRequestMessageReader{},
			r.config,
		)
		l.Infow("signal stream closed", "error", err)

		reqChan.Close()
	}()

	err = r.sessionHandler(ctx, livekit.RoomName(ss.RoomName), *pi, livekit.ConnectionID(ss.ConnectionId), reqChan, sink)
	if err != nil {
		l.Errorw("could not handle new participant", err)
		return
	}

	stream.Hijack()
	return
}

type signalResponseMessageWriter struct{}

func (e signalResponseMessageWriter) Write(seq uint64, close bool, msgs []proto.Message) *rpc.RelaySignalResponse {
	r := &rpc.RelaySignalResponse{
		Seq:       seq,
		Responses: make([]*livekit.SignalResponse, 0, len(msgs)),
		Close:     close,
	}
	for _, m := range msgs {
		r.Responses = append(r.Responses, m.(*livekit.SignalResponse))
	}
	return r
}

type signalRequestMessageReader struct{}

func (e signalRequestMessageReader) Read(rm *rpc.RelaySignalRequest) ([]proto.Message, error) {
	msgs := make([]proto.Message, 0, len(rm.Requests))
	for _, m := range rm.Requests {
		msgs = append(msgs, m)
	}
	return msgs, nil
}

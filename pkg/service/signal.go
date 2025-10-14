// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package service

import (
	"context"

	"github.com/pkg/errors"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
	"github.com/livekit/psrpc/pkg/metadata"
	"github.com/livekit/psrpc/pkg/middleware"
)

//counterfeiter:generate . SessionHandler
type SessionHandler interface {
	Logger(ctx context.Context) logger.Logger

	HandleSession(
		ctx context.Context,
		pi routing.ParticipantInit,
		connectionID livekit.ConnectionID,
		requestSource routing.MessageSource,
		responseSink routing.MessageSink,
	) error
}

type SignalServer struct {
	server rpc.TypedSignalServer
	nodeID livekit.NodeID
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
		middleware.WithServerMetrics(rpc.PSRPCMetricsObserver{}),
		psrpc.WithServerChannelSize(config.StreamBufferSize),
	)
	if err != nil {
		return nil, err
	}
	return &SignalServer{s, nodeID}, nil
}

func NewDefaultSignalServer(
	currentNode routing.LocalNode,
	bus psrpc.MessageBus,
	config config.SignalRelayConfig,
	router routing.Router,
	roomManager *RoomManager,
) (r *SignalServer, err error) {
	return NewSignalServer(currentNode.NodeID(), currentNode.Region(), bus, config, &defaultSessionHandler{currentNode, router, roomManager})
}

type defaultSessionHandler struct {
	currentNode routing.LocalNode
	router      routing.Router
	roomManager *RoomManager
}

func (s *defaultSessionHandler) Logger(ctx context.Context) logger.Logger {
	return utils.GetLogger(ctx)
}

func (s *defaultSessionHandler) HandleSession(
	ctx context.Context,
	pi routing.ParticipantInit,
	connectionID livekit.ConnectionID,
	requestSource routing.MessageSource,
	responseSink routing.MessageSink,
) error {
	prometheus.IncrementParticipantRtcInit(1)

	rtcNode, err := s.router.GetNodeForRoom(ctx, livekit.RoomName(pi.CreateRoom.Name))
	if err != nil {
		return err
	}

	if livekit.NodeID(rtcNode.Id) != s.currentNode.NodeID() {
		err = routing.ErrIncorrectRTCNode
		logger.Errorw("called participant on incorrect node", err,
			"rtcNode", rtcNode,
		)
		return err
	}

	return s.roomManager.StartSession(ctx, pi, requestSource, responseSink, false)
}

func (s *SignalServer) Start() error {
	logger.Debugw("starting relay signal server", "topic", s.nodeID)
	return s.server.RegisterAllNodeTopics(s.nodeID)
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

	l := r.sessionHandler.Logger(stream.Context()).WithValues(
		"room", ss.RoomName,
		"participant", ss.Identity,
		"connID", ss.ConnectionId,
	)

	stream.Hijack()
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
			prometheus.RecordSignalResponseSuccess,
			prometheus.RecordSignalResponseFailure,
		)
		l.Debugw("signal stream closed", "error", err)

		reqChan.Close()
	}()

	// copy the context to prevent a race between the session handler closing
	// and the delivery of any parting messages from the client. take care to
	// copy the incoming rpc headers to avoid dropping any session vars.
	ctx := metadata.NewContextWithIncomingHeader(context.Background(), metadata.IncomingHeader(stream.Context()))
	err = r.sessionHandler.HandleSession(ctx, *pi, livekit.ConnectionID(ss.ConnectionId), reqChan, sink)
	if err != nil {
		sink.Close()
		l.Errorw("could not handle new participant", err)
	}
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

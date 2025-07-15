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
	"encoding/json"

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
	"github.com/livekit/psrpc/pkg/middleware"
)

//counterfeiter:generate . ConnectHandler
type ConnectHandler interface {
	Logger(ctx context.Context) logger.Logger

	HandleConnect(
		ctx context.Context,
		lgr logger.Logger,
		grants *auth.ClaimGrants,
		rscr *rpc.RelaySignalv2ConnectRequest,
	) (*rpc.RelaySignalv2ConnectResponse, error)
}

type Signalv2Server struct {
	server rpc.TypedSignalv2Server
	nodeID livekit.NodeID
}

func NewSignalv2Server(
	nodeID livekit.NodeID,
	region string,
	bus psrpc.MessageBus,
	connectHandler ConnectHandler,
) (*Signalv2Server, error) {
	s, err := rpc.NewTypedSignalv2Server(
		nodeID,
		&signalv2Service{region, connectHandler},
		bus,
		middleware.WithServerMetrics(rpc.PSRPCMetricsObserver{}),
	)
	if err != nil {
		return nil, err
	}
	return &Signalv2Server{s, nodeID}, nil
}

func (s *Signalv2Server) Start() error {
	logger.Debugw("starting relay signal v2 server", "topic", s.nodeID)
	return s.server.RegisterAllNodeTopics(s.nodeID)
}

func (r *Signalv2Server) Stop() {
	r.server.Kill()
}

// -------------------------------------------------

func NewDefaultSignalv2Server(
	currentNode routing.LocalNode,
	bus psrpc.MessageBus,
	router routing.Router,
	roomManager *RoomManager,
) (*Signalv2Server, error) {
	return NewSignalv2Server(
		currentNode.NodeID(),
		currentNode.Region(),
		bus,
		&defaultSignalv2Handler{currentNode, router, roomManager},
	)
}

// -------------------------------------------------

type defaultSignalv2Handler struct {
	currentNode routing.LocalNode
	router      routing.Router
	roomManager *RoomManager
}

func (s *defaultSignalv2Handler) Logger(ctx context.Context) logger.Logger {
	return utils.GetLogger(ctx)
}

func (s *defaultSignalv2Handler) HandleConnect(
	ctx context.Context,
	lgr logger.Logger,
	grants *auth.ClaimGrants,
	rscr *rpc.RelaySignalv2ConnectRequest,
) (*rpc.RelaySignalv2ConnectResponse, error) {
	// SIGNALLING-V2-TODO prometheus.IncrementParticipantRtcInit(1)

	rtcNode, err := s.router.GetNodeForRoom(ctx, livekit.RoomName(rscr.CreateRoom.Name))
	if err != nil {
		return nil, err
	}

	if livekit.NodeID(rtcNode.Id) != s.currentNode.NodeID() {
		err = routing.ErrIncorrectRTCNode
		lgr.Errorw(
			"called participant on incorrect node", err,
			"rtcNode", rtcNode,
		)
		return nil, err
	}

	return s.roomManager.HandleConnect(ctx, grants, rscr)
}

// ------------------------------------------

type signalv2Service struct {
	region         string
	connectHandler ConnectHandler
}

func (r *signalv2Service) RelaySignalv2Connect(ctx context.Context, rscr *rpc.RelaySignalv2ConnectRequest) (*rpc.RelaySignalv2ConnectResponse, error) {
	grants := &auth.ClaimGrants{}
	if err := json.Unmarshal([]byte(rscr.GrantsJson), grants); err != nil {
		return nil, err
	}

	lgr := r.connectHandler.Logger(ctx).WithValues(
		"room", grants.Video.Room,
		"participant", grants.Identity,
		// RAJA-TODO - maybe add a connection ID to rscr for tracking only
	)

	resp, err := r.connectHandler.HandleConnect(ctx, lgr, grants, rscr)
	if err != nil {
		lgr.Errorw("could not handle new participant", err)
	}
	return resp, err
}

// ------------------------------------------

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

	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"

	"github.com/livekit/livekit-server/pkg/telemetry"
)

type IOInfoService struct {
	ioServer rpc.IOInfoServer

	es        EgressStore
	is        IngressStore
	ss        SIPStore
	telemetry telemetry.TelemetryService

	shutdown chan struct{}
}

func NewIOInfoService(
	bus psrpc.MessageBus,
	es EgressStore,
	is IngressStore,
	ss SIPStore,
	ts telemetry.TelemetryService,
) (*IOInfoService, error) {
	s := &IOInfoService{
		es:        es,
		is:        is,
		ss:        ss,
		telemetry: ts,
		shutdown:  make(chan struct{}),
	}

	if bus != nil {
		ioServer, err := rpc.NewIOInfoServer(s, bus)
		if err != nil {
			return nil, err
		}
		s.ioServer = ioServer
	}

	return s, nil
}

func (s *IOInfoService) Start() error {
	if s.es != nil {
		rs := s.es.(*RedisStore)
		err := rs.Start()
		if err != nil {
			logger.Errorw("failed to start redis egress worker", err)
			return err
		}
	}

	return nil
}

func (s *IOInfoService) Stop() {
	close(s.shutdown)

	if s.ioServer != nil {
		s.ioServer.Shutdown()
	}
}

func (s *IOInfoService) CreateEgress(ctx context.Context, info *livekit.EgressInfo) (*emptypb.Empty, error) {
	if s.es == nil {
		return nil, ErrEgressNotConnected
	}

	// check if egress already exists to avoid duplicate EgressStarted event
	if _, err := s.es.LoadEgress(ctx, info.EgressId); err == nil {
		return &emptypb.Empty{}, nil
	}

	err := s.es.StoreEgress(ctx, info)
	if err != nil {
		logger.Errorw("could not update egress", err)
		return nil, err
	}

	s.telemetry.EgressStarted(ctx, info)

	return &emptypb.Empty{}, nil
}

func (s *IOInfoService) UpdateEgress(ctx context.Context, info *livekit.EgressInfo) (*emptypb.Empty, error) {
	if s.es == nil {
		return nil, ErrEgressNotConnected
	}

	err := s.es.UpdateEgress(ctx, info)

	switch info.Status {
	case livekit.EgressStatus_EGRESS_ACTIVE,
		livekit.EgressStatus_EGRESS_ENDING:
		s.telemetry.EgressUpdated(ctx, info)

	case livekit.EgressStatus_EGRESS_COMPLETE,
		livekit.EgressStatus_EGRESS_FAILED,
		livekit.EgressStatus_EGRESS_ABORTED,
		livekit.EgressStatus_EGRESS_LIMIT_REACHED:
		s.telemetry.EgressEnded(ctx, info)
	}

	if err != nil {
		logger.Errorw("could not update egress", err)
		return nil, err
	}

	return &emptypb.Empty{}, nil
}

func (s *IOInfoService) GetEgress(ctx context.Context, req *rpc.GetEgressRequest) (*livekit.EgressInfo, error) {
	if s.es == nil {
		return nil, ErrEgressNotConnected
	}

	info, err := s.es.LoadEgress(ctx, req.EgressId)
	if err != nil {
		logger.Errorw("failed to load egress", err)
		return nil, err
	}

	return info, nil
}

func (s *IOInfoService) ListEgress(ctx context.Context, req *livekit.ListEgressRequest) (*livekit.ListEgressResponse, error) {
	if s.es == nil {
		return nil, ErrEgressNotConnected
	}

	if req.EgressId != "" {
		info, err := s.es.LoadEgress(ctx, req.EgressId)
		if err != nil {
			logger.Errorw("failed to load egress", err)
			return nil, err
		}

		return &livekit.ListEgressResponse{Items: []*livekit.EgressInfo{info}}, nil
	}

	items, err := s.es.ListEgress(ctx, livekit.RoomName(req.RoomName), req.Active)
	if err != nil {
		logger.Errorw("failed to list egress", err)
		return nil, err
	}

	return &livekit.ListEgressResponse{Items: items}, nil
}

func (s *IOInfoService) UpdateMetrics(ctx context.Context, req *rpc.UpdateMetricsRequest) (*emptypb.Empty, error) {
	logger.Infow("received egress metrics",
		"egressID", req.Info.EgressId,
		"avgCpu", req.AvgCpuUsage,
		"maxCpu", req.MaxCpuUsage,
	)
	return &emptypb.Empty{}, nil
}

func (s *IOInfoService) UpdateSIPCallState(ctx context.Context, req *rpc.UpdateSIPCallStateRequest) (*emptypb.Empty, error) {
	// TODO: placeholder
	return &emptypb.Empty{}, nil
}

func (s *IOInfoService) RecordCallContext(context.Context, *rpc.RecordCallContextRequest) (*emptypb.Empty, error) {
	// TODO: placeholder
	return &emptypb.Empty{}, nil
}

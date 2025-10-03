// Copyright 2024 LiveKit, Inc.
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
	"errors"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

func (s *IOInfoService) CreateIngress(ctx context.Context, info *livekit.IngressInfo) (*emptypb.Empty, error) {
	if s.is == nil {
		return nil, ErrIngressNotConnected
	}

	err := s.is.StoreIngress(ctx, info)
	if err != nil {
		return nil, err
	}

	s.telemetry.IngressCreated(ctx, info)

	return &emptypb.Empty{}, nil
}

func (s *IOInfoService) GetIngressInfo(ctx context.Context, req *rpc.GetIngressInfoRequest) (*rpc.GetIngressInfoResponse, error) {
	info, err := s.loadIngressFromInfoRequest(req)
	if err != nil {
		return nil, err
	}

	return &rpc.GetIngressInfoResponse{Info: info}, nil
}

func (s *IOInfoService) loadIngressFromInfoRequest(req *rpc.GetIngressInfoRequest) (info *livekit.IngressInfo, err error) {
	if s.is == nil {
		return nil, ErrIngressNotConnected
	}

	if req.IngressId != "" {
		info, err = s.is.LoadIngress(context.Background(), req.IngressId)
	} else if req.StreamKey != "" {
		info, err = s.is.LoadIngressFromStreamKey(context.Background(), req.StreamKey)
	} else {
		err = errors.New("request needs to specify either IngressId or StreamKey")
	}
	return info, err
}

func (s *IOInfoService) UpdateIngressState(ctx context.Context, req *rpc.UpdateIngressStateRequest) (*emptypb.Empty, error) {
	if s.is == nil {
		return nil, ErrIngressNotConnected
	}

	info, err := s.is.LoadIngress(ctx, req.IngressId)
	if err != nil {
		return nil, err
	}

	if err = s.is.UpdateIngressState(ctx, req.IngressId, req.State); err != nil {
		logger.Errorw("could not update ingress", err)
		return nil, err
	}

	if info.State.Status != req.State.Status {
		info.State = req.State

		switch req.State.Status {
		case livekit.IngressState_ENDPOINT_ERROR,
			livekit.IngressState_ENDPOINT_INACTIVE,
			livekit.IngressState_ENDPOINT_COMPLETE:
			s.telemetry.IngressEnded(ctx, info)

			if req.State.Error != "" {
				logger.Infow("ingress failed", "error", req.State.Error, "ingressID", req.IngressId)
			} else {
				logger.Infow("ingress ended", "ingressID", req.IngressId)
			}

		case livekit.IngressState_ENDPOINT_PUBLISHING:
			s.telemetry.IngressStarted(ctx, info)

			logger.Infow("ingress started", "ingressID", req.IngressId)

		case livekit.IngressState_ENDPOINT_BUFFERING:
			s.telemetry.IngressUpdated(ctx, info)

			logger.Infow("ingress buffering", "ingressID", req.IngressId)
		}
	} else {
		// Status didn't change, send Updated event
		info.State = req.State

		s.telemetry.IngressUpdated(ctx, info)

		logger.Infow("ingress state updated", "ingressID", req.IngressId, "status", info.State.Status)
	}

	return &emptypb.Empty{}, nil
}

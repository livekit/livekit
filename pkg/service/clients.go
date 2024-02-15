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

	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate

//counterfeiter:generate . IOClient
type IOClient interface {
	CreateEgress(ctx context.Context, info *livekit.EgressInfo) (*emptypb.Empty, error)
	GetEgress(ctx context.Context, req *rpc.GetEgressRequest) (*livekit.EgressInfo, error)
	ListEgress(ctx context.Context, req *livekit.ListEgressRequest) (*livekit.ListEgressResponse, error)
	CreateIngress(ctx context.Context, req *livekit.IngressInfo) (*emptypb.Empty, error)
	UpdateIngressState(ctx context.Context, req *rpc.UpdateIngressStateRequest) (*emptypb.Empty, error)
}

type egressLauncher struct {
	client rpc.EgressClient
	io     IOClient
}

func NewEgressLauncher(client rpc.EgressClient, io IOClient) rtc.EgressLauncher {
	if client == nil {
		return nil
	}
	return &egressLauncher{
		client: client,
		io:     io,
	}
}

func (s *egressLauncher) StartEgress(ctx context.Context, req *rpc.StartEgressRequest) (*livekit.EgressInfo, error) {
	info, err := s.StartEgressWithClusterId(ctx, "", req)
	if err != nil {
		return nil, err
	}

	_, err = s.io.CreateEgress(ctx, info)
	if err != nil {
		logger.Errorw("failed to create egress", err)
	}

	return info, nil
}

func (s *egressLauncher) StartEgressWithClusterId(ctx context.Context, clusterId string, req *rpc.StartEgressRequest) (*livekit.EgressInfo, error) {
	if s.client == nil {
		return nil, ErrEgressNotConnected
	}

	// Ensure we have an Egress ID
	if req.EgressId == "" {
		req.EgressId = utils.NewGuid(utils.EgressPrefix)
	}

	return s.client.StartEgress(ctx, clusterId, req)
}

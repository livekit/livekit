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

package routing

import (
	"context"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
	"github.com/livekit/psrpc/pkg/middleware"
)

//counterfeiter:generate . RoomManagerClient
type RoomManagerClient interface {
	rpc.TypedRoomManagerClient
}

type roomManagerClient struct {
	config config.RoomConfig
	client rpc.TypedRoomManagerClient
}

func NewRoomManagerClient(clientParams rpc.ClientParams, config config.RoomConfig) (RoomManagerClient, error) {
	c, err := rpc.NewTypedRoomManagerClient(
		clientParams.Bus,
		psrpc.WithClientChannelSize(clientParams.BufferSize),
		middleware.WithClientMetrics(clientParams.Observer),
		rpc.WithClientLogger(clientParams.Logger),
	)
	if err != nil {
		return nil, err
	}

	return &roomManagerClient{
		config: config,
		client: c,
	}, nil
}

func (c *roomManagerClient) CreateRoom(ctx context.Context, nodeID livekit.NodeID, req *livekit.CreateRoomRequest, opts ...psrpc.RequestOption) (*livekit.Room, error) {
	return c.client.CreateRoom(ctx, nodeID, req, append(opts, psrpc.WithRequestInterceptors(middleware.NewRPCRetryInterceptor(middleware.RetryOptions{
		MaxAttempts: c.config.CreateRoomAttempts,
		Timeout:     c.config.CreateRoomTimeout,
	})))...)
}

func (c *roomManagerClient) Close() {
	c.client.Close()
}

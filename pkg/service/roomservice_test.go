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

package service_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/twitchtv/twirp"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/rpc/rpcfakes"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing/routingfakes"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/service/servicefakes"
)

func TestDeleteRoom(t *testing.T) {
	t.Run("missing permissions", func(t *testing.T) {
		svc := newTestRoomService(config.LimitConfig{})
		grant := &auth.ClaimGrants{
			Video: &auth.VideoGrant{},
		}
		ctx := service.WithGrants(context.Background(), grant, "")
		_, err := svc.DeleteRoom(ctx, &livekit.DeleteRoomRequest{
			Room: "testroom",
		})
		require.Error(t, err)
	})
}

func TestMetaDataLimits(t *testing.T) {
	t.Run("metadata exceed limits", func(t *testing.T) {
		svc := newTestRoomService(config.LimitConfig{MaxMetadataSize: 5})
		grant := &auth.ClaimGrants{
			Video: &auth.VideoGrant{},
		}
		ctx := service.WithGrants(context.Background(), grant, "")
		_, err := svc.UpdateParticipant(ctx, &livekit.UpdateParticipantRequest{
			Room:     "testroom",
			Identity: "123",
			Metadata: "abcdefg",
		})
		terr, ok := err.(twirp.Error)
		require.True(t, ok)
		require.Equal(t, twirp.InvalidArgument, terr.Code())

		_, err = svc.UpdateRoomMetadata(ctx, &livekit.UpdateRoomMetadataRequest{
			Room:     "testroom",
			Metadata: "abcdefg",
		})
		terr, ok = err.(twirp.Error)
		require.True(t, ok)
		require.Equal(t, twirp.InvalidArgument, terr.Code())
	})

	notExceedsLimitsSvc := map[string]*TestRoomService{
		"metadata exceeds limits": newTestRoomService(config.LimitConfig{MaxMetadataSize: 5}),
		"metadata no limits":      newTestRoomService(config.LimitConfig{}), // no limits
	}

	for n, s := range notExceedsLimitsSvc {
		svc := s
		t.Run(n, func(t *testing.T) {
			grant := &auth.ClaimGrants{
				Video: &auth.VideoGrant{},
			}
			ctx := service.WithGrants(context.Background(), grant, "")
			_, err := svc.UpdateParticipant(ctx, &livekit.UpdateParticipantRequest{
				Room:     "testroom",
				Identity: "123",
				Metadata: "abc",
			})
			terr, ok := err.(twirp.Error)
			require.True(t, ok)
			require.NotEqual(t, twirp.InvalidArgument, terr.Code())

			_, err = svc.UpdateRoomMetadata(ctx, &livekit.UpdateRoomMetadataRequest{
				Room:     "testroom",
				Metadata: "abc",
			})
			terr, ok = err.(twirp.Error)
			require.True(t, ok)
			require.NotEqual(t, twirp.InvalidArgument, terr.Code())
		})

	}
}

func newTestRoomService(limitConf config.LimitConfig) *TestRoomService {
	router := &routingfakes.FakeRouter{}
	allocator := &servicefakes.FakeRoomAllocator{}
	store := &servicefakes.FakeServiceStore{}
	svc, err := service.NewRoomService(
		limitConf,
		config.APIConfig{ExecutionTimeout: 2},
		router,
		allocator,
		store,
		nil,
		rpc.NewTopicFormatter(),
		&rpcfakes.FakeTypedRoomClient{},
		&rpcfakes.FakeTypedParticipantClient{},
	)
	if err != nil {
		panic(err)
	}
	return &TestRoomService{
		RoomService: *svc,
		router:      router,
		allocator:   allocator,
		store:       store,
	}
}

type TestRoomService struct {
	service.RoomService
	router    *routingfakes.FakeRouter
	allocator *servicefakes.FakeRoomAllocator
	store     *servicefakes.FakeServiceStore
}

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
	"time"

	"github.com/stretchr/testify/require"
	"github.com/twitchtv/twirp"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/rpc/rpcfakes"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/psrpc"

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
	adminCtx := func() context.Context {
		return service.WithGrants(context.Background(), &auth.ClaimGrants{Video: &auth.VideoGrant{}}, "")
	}
	createCtx := func() context.Context {
		return service.WithGrants(context.Background(), &auth.ClaimGrants{Video: &auth.VideoGrant{RoomCreate: true}}, "")
	}
	requireInvalidArg := func(t *testing.T, err error) {
		t.Helper()
		terr, ok := err.(twirp.Error)
		require.True(t, ok, "expected twirp error, got %T (%v)", err, err)
		require.Equal(t, twirp.InvalidArgument, terr.Code())
	}

	t.Run("metadata exceeds limit", func(t *testing.T) {
		svc := newTestRoomService(config.LimitConfig{MaxMetadataSize: 5})

		_, err := svc.UpdateParticipant(adminCtx(), &livekit.UpdateParticipantRequest{
			Room:     "testroom",
			Identity: "123",
			Metadata: "abcdefg",
		})
		requireInvalidArg(t, err)

		_, err = svc.UpdateRoomMetadata(adminCtx(), &livekit.UpdateRoomMetadataRequest{
			Room:     "testroom",
			Metadata: "abcdefg",
		})
		requireInvalidArg(t, err)

		_, err = svc.CreateRoom(createCtx(), &livekit.CreateRoomRequest{
			Name:     "testroom",
			Metadata: "abcdefg",
		})
		requireInvalidArg(t, err)

		_, err = svc.CreateRoom(createCtx(), &livekit.CreateRoomRequest{
			Name: "testroom",
			Agents: []*livekit.RoomAgentDispatch{
				{AgentName: "bot", Metadata: "abcdefg"},
			},
		})
		requireInvalidArg(t, err)
	})

	t.Run("embedded agent dispatch in CreateRoom exceeds attributes limit", func(t *testing.T) {
		svc := newTestRoomService(config.LimitConfig{MaxAttributesSize: 5})
		_, err := svc.CreateRoom(createCtx(), &livekit.CreateRoomRequest{
			Name: "testroom",
			Agents: []*livekit.RoomAgentDispatch{
				{AgentName: "bot", Attributes: map[string]string{"key": "abcdefg"}},
			},
		})
		requireInvalidArg(t, err)
	})

	notExceedsLimitsSvc := map[string]*TestRoomService{
		"metadata exceeds limits": newTestRoomService(config.LimitConfig{
			MaxMetadataSize:   5,
			MaxAttributesSize: 5,
		}),
		"metadata no limits": newTestRoomService(config.LimitConfig{}),
	}

	for n, svc := range notExceedsLimitsSvc {
		t.Run(n, func(t *testing.T) {
			_, err := svc.UpdateParticipant(adminCtx(), &livekit.UpdateParticipantRequest{
				Room:     "testroom",
				Identity: "123",
				Metadata: "abc",
			})
			terr, ok := err.(twirp.Error)
			require.True(t, ok)
			require.NotEqual(t, twirp.InvalidArgument, terr.Code())

			_, err = svc.UpdateRoomMetadata(adminCtx(), &livekit.UpdateRoomMetadataRequest{
				Room:     "testroom",
				Metadata: "abc",
			})
			terr, ok = err.(twirp.Error)
			require.True(t, ok)
			require.NotEqual(t, twirp.InvalidArgument, terr.Code())

			_, err = svc.CreateRoom(createCtx(), &livekit.CreateRoomRequest{
				Name:     "testroom",
				Metadata: "abc",
				Agents: []*livekit.RoomAgentDispatch{
					{AgentName: "bot", Metadata: "abc", Attributes: map[string]string{"k": "v"}},
				},
			})
			if err != nil {
				terr, ok = err.(twirp.Error)
				require.True(t, ok)
				require.NotEqual(t, twirp.InvalidArgument, terr.Code())
			}
		})
	}
}

func TestAgentDispatchMetadataLimits(t *testing.T) {
	ctx := service.WithGrants(context.Background(), &auth.ClaimGrants{
		Video: &auth.VideoGrant{Room: "testroom", RoomAdmin: true},
	}, "")

	t.Run("metadata exceeds limits", func(t *testing.T) {
		svc := newTestAgentDispatchService(config.LimitConfig{MaxMetadataSize: 5})
		_, err := svc.CreateDispatch(ctx, &livekit.CreateAgentDispatchRequest{
			Room:     "testroom",
			Metadata: "abcdefg",
		})
		require.ErrorIs(t, err, service.ErrMetadataExceedsLimits)
	})

	t.Run("attributes exceeds limits", func(t *testing.T) {
		svc := newTestAgentDispatchService(config.LimitConfig{MaxAttributesSize: 5})
		_, err := svc.CreateDispatch(ctx, &livekit.CreateAgentDispatchRequest{
			Room:       "testroom",
			Attributes: map[string]string{"key": "abcdefg"},
		})
		require.ErrorIs(t, err, service.ErrAttributeExceedsLimits)
	})

	t.Run("room name exceeds limits", func(t *testing.T) {
		allocator := &servicefakes.FakeRoomAllocator{}
		allocator.AutoCreateEnabledReturns(true)
		router := &routingfakes.FakeRouter{}
		svc := service.NewAgentDispatchService(
			config.LimitConfig{MaxRoomNameLength: 5},
			&rpcfakes.FakeTypedAgentDispatchInternalClient{},
			rpc.NewTopicFormatter(),
			allocator,
			router,
		)
		ctx := service.WithGrants(context.Background(), &auth.ClaimGrants{
			Video: &auth.VideoGrant{Room: "abcdefg", RoomAdmin: true},
		}, "")
		_, err := svc.CreateDispatch(ctx, &livekit.CreateAgentDispatchRequest{
			Room: "abcdefg",
		})
		require.ErrorIs(t, err, service.ErrRoomNameExceedsLimits)
		require.Zero(t, router.CreateRoomCallCount())
	})
}

func newTestAgentDispatchService(limitConf config.LimitConfig) *service.AgentDispatchService {
	allocator := &servicefakes.FakeRoomAllocator{}
	allocator.AutoCreateEnabledReturns(false)
	return service.NewAgentDispatchService(limitConf, nil, rpc.NewTopicFormatter(), allocator, &routingfakes.FakeRouter{})
}

func newTestRoomService(limitConf config.LimitConfig) *TestRoomService {
	router := &routingfakes.FakeRouter{}
	allocator := &servicefakes.FakeRoomAllocator{}
	store := &servicefakes.FakeServiceStore{}
	participantClient := &rpcfakes.FakeTypedParticipantClient{}
	svc, err := service.NewRoomService(
		limitConf,
		config.APIConfig{ExecutionTimeout: 2},
		router,
		allocator,
		store,
		nil,
		rpc.NewTopicFormatter(),
		&rpcfakes.FakeTypedRoomClient{},
		participantClient,
	)
	if err != nil {
		panic(err)
	}
	return &TestRoomService{
		RoomService:       *svc,
		router:            router,
		allocator:         allocator,
		store:             store,
		participantClient: participantClient,
	}
}

func TestPerformRpcRequestTimeout(t *testing.T) {
	grant := &auth.ClaimGrants{
		Video: &auth.VideoGrant{RoomAdmin: true, Room: "testroom"},
	}
	ctx := service.WithGrants(context.Background(), grant, "")

	requestTimeout := func(t *testing.T, svc *TestRoomService, call int) time.Duration {
		t.Helper()
		_, _, _, opts := svc.participantClient.PerformRpcArgsForCall(call)
		var ro psrpc.RequestOpts
		for _, o := range opts {
			o(&ro)
		}
		return ro.Timeout
	}

	t.Run("default response timeout", func(t *testing.T) {
		svc := newTestRoomService(config.LimitConfig{})
		_, err := svc.PerformRpc(ctx, &livekit.PerformRpcRequest{Room: "testroom", DestinationIdentity: "p", Method: "m"})
		require.NoError(t, err)
		require.Equal(t, utils.DataChannelRpcDefaultResponseTimeout+utils.DataChannelRpcMaxRoundTripLatency, requestTimeout(t, svc, 0))
	})

	t.Run("requested response timeout", func(t *testing.T) {
		svc := newTestRoomService(config.LimitConfig{})
		_, err := svc.PerformRpc(ctx, &livekit.PerformRpcRequest{Room: "testroom", DestinationIdentity: "p", Method: "m", ResponseTimeoutMs: 500})
		require.NoError(t, err)
		require.Equal(t, 500*time.Millisecond+utils.DataChannelRpcMaxRoundTripLatency, requestTimeout(t, svc, 0))
	})
}

type TestRoomService struct {
	service.RoomService
	router            *routingfakes.FakeRouter
	allocator         *servicefakes.FakeRoomAllocator
	store             *servicefakes.FakeServiceStore
	participantClient *rpcfakes.FakeTypedParticipantClient
}

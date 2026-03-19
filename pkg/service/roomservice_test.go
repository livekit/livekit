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
	roomClient := &rpcfakes.FakeTypedRoomClient{}
	participantClient := &rpcfakes.FakeTypedParticipantClient{}
	svc, err := service.NewRoomService(
		limitConf,
		config.APIConfig{ExecutionTimeout: 2},
		router,
		allocator,
		store,
		nil,
		rpc.NewTopicFormatter(),
		roomClient,
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
		roomClient:        roomClient,
		participantClient: participantClient,
	}
}

type TestRoomService struct {
	service.RoomService
	router            *routingfakes.FakeRouter
	allocator         *servicefakes.FakeRoomAllocator
	store             *servicefakes.FakeServiceStore
	roomClient        *rpcfakes.FakeTypedRoomClient
	participantClient *rpcfakes.FakeTypedParticipantClient
}

const testRoom = "mytestroom"

func adminContext(roomName string) context.Context {
	grant := &auth.ClaimGrants{
		Video: &auth.VideoGrant{RoomAdmin: true, Room: roomName},
	}
	return service.WithGrants(context.Background(), grant, "")
}

// --- ListParticipants ---

func TestListParticipants_RoutesViaPSRPC(t *testing.T) {
	t.Run("calls roomClient instead of roomStore", func(t *testing.T) {
		svc := newTestRoomService(config.LimitConfig{})

		expectedParticipants := []*livekit.ParticipantInfo{
			{Sid: "PA_123", Identity: "alice", State: livekit.ParticipantInfo_ACTIVE},
			{Sid: "PA_456", Identity: "bob", State: livekit.ParticipantInfo_ACTIVE},
		}

		svc.roomClient.ListParticipantsReturns(&livekit.ListParticipantsResponse{
			Participants: expectedParticipants,
		}, nil)

		ctx := adminContext(testRoom)
		res, err := svc.ListParticipants(ctx, &livekit.ListParticipantsRequest{
			Room: testRoom,
		})
		require.NoError(t, err)
		require.Len(t, res.Participants, 2)
		require.Equal(t, "alice", res.Participants[0].Identity)
		require.Equal(t, "bob", res.Participants[1].Identity)

		// Verify roomClient was called (PSRPC path)
		require.Equal(t, 1, svc.roomClient.ListParticipantsCallCount())

		// Verify roomStore was NOT called (Redis path)
		require.Equal(t, 0, svc.store.ListParticipantsCallCount())
	})

	t.Run("missing permissions", func(t *testing.T) {
		svc := newTestRoomService(config.LimitConfig{})
		grant := &auth.ClaimGrants{
			Video: &auth.VideoGrant{},
		}
		ctx := service.WithGrants(context.Background(), grant, "")
		_, err := svc.ListParticipants(ctx, &livekit.ListParticipantsRequest{
			Room: testRoom,
		})
		require.Error(t, err)

		// roomClient should NOT have been called — auth failed first
		require.Equal(t, 0, svc.roomClient.ListParticipantsCallCount())
	})
}

// --- GetParticipant ---

func TestGetParticipant_RoutesViaPSRPC(t *testing.T) {
	t.Run("calls roomClient instead of roomStore", func(t *testing.T) {
		svc := newTestRoomService(config.LimitConfig{})

		expectedParticipant := &livekit.ParticipantInfo{
			Sid:      "PA_789",
			Identity: "sip-caller",
			State:    livekit.ParticipantInfo_ACTIVE,
		}

		svc.roomClient.GetParticipantReturns(expectedParticipant, nil)

		ctx := adminContext(testRoom)
		res, err := svc.GetParticipant(ctx, &livekit.RoomParticipantIdentity{
			Room:     testRoom,
			Identity: "sip-caller",
		})
		require.NoError(t, err)
		require.Equal(t, "sip-caller", res.Identity)
		require.Equal(t, "PA_789", res.Sid)

		// Verify roomClient was called (PSRPC path)
		require.Equal(t, 1, svc.roomClient.GetParticipantCallCount())

		// Verify roomStore was NOT called (Redis path)
		require.Equal(t, 0, svc.store.LoadParticipantCallCount())
	})

	t.Run("missing permissions", func(t *testing.T) {
		svc := newTestRoomService(config.LimitConfig{})
		grant := &auth.ClaimGrants{
			Video: &auth.VideoGrant{},
		}
		ctx := service.WithGrants(context.Background(), grant, "")
		_, err := svc.GetParticipant(ctx, &livekit.RoomParticipantIdentity{
			Room:     testRoom,
			Identity: "sip-caller",
		})
		require.Error(t, err)
		require.Equal(t, 0, svc.roomClient.GetParticipantCallCount())
	})
}

// --- RemoveParticipant: Redis guard removed ---

func TestRemoveParticipant_NoRedisGuard(t *testing.T) {
	t.Run("does not check roomStore before forwarding to PSRPC", func(t *testing.T) {
		svc := newTestRoomService(config.LimitConfig{})

		svc.participantClient.RemoveParticipantReturns(&livekit.RemoveParticipantResponse{}, nil)

		ctx := adminContext(testRoom)
		_, err := svc.RemoveParticipant(ctx, &livekit.RoomParticipantIdentity{
			Room:     testRoom,
			Identity: "sip-caller",
		})
		require.NoError(t, err)

		// The PSRPC participant client should have been called
		require.Equal(t, 1, svc.participantClient.RemoveParticipantCallCount())

		// The store should NOT have been called as a guard.
		require.Equal(t, 0, svc.store.LoadParticipantCallCount())
	})
}

// --- UpdateParticipant: Redis guard removed ---

func TestUpdateParticipant_NoRedisGuard(t *testing.T) {
	t.Run("does not check roomStore before forwarding to PSRPC", func(t *testing.T) {
		svc := newTestRoomService(config.LimitConfig{})

		expectedParticipant := &livekit.ParticipantInfo{
			Sid:      "PA_789",
			Identity: "sip-caller",
			Metadata: "new-metadata",
		}
		svc.participantClient.UpdateParticipantReturns(expectedParticipant, nil)

		ctx := adminContext(testRoom)
		res, err := svc.UpdateParticipant(ctx, &livekit.UpdateParticipantRequest{
			Room:     testRoom,
			Identity: "sip-caller",
			Metadata: "new-metadata",
		})
		require.NoError(t, err)
		require.Equal(t, "new-metadata", res.Metadata)

		// The PSRPC participant client should have been called
		require.Equal(t, 1, svc.participantClient.UpdateParticipantCallCount())

		// The store should NOT have been called as a guard.
		require.Equal(t, 0, svc.store.LoadParticipantCallCount())
	})

	t.Run("still validates metadata limits before PSRPC", func(t *testing.T) {
		svc := newTestRoomService(config.LimitConfig{MaxMetadataSize: 5})
		ctx := adminContext(testRoom)
		_, err := svc.UpdateParticipant(ctx, &livekit.UpdateParticipantRequest{
			Room:     testRoom,
			Identity: "sip-caller",
			Metadata: "this-metadata-exceeds-the-5-byte-limit",
		})
		require.Error(t, err)
		// Should fail at validation, never reaching PSRPC
		require.Equal(t, 0, svc.participantClient.UpdateParticipantCallCount())
	})
}

package service_test

import (
	"context"
	"testing"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/require"
	"github.com/twitchtv/twirp"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing/routingfakes"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/service/servicefakes"
)

func TestDeleteRoom(t *testing.T) {
	t.Run("normal deletion", func(t *testing.T) {
		svc := newTestRoomService()
		grant := &auth.ClaimGrants{
			Video: &auth.VideoGrant{
				RoomCreate: true,
			},
		}
		ctx := service.WithGrants(context.Background(), grant)
		svc.store.LoadRoomReturns(nil, service.ErrRoomNotFound)
		_, err := svc.DeleteRoom(ctx, &livekit.DeleteRoomRequest{
			Room: "testroom",
		})
		require.NoError(t, err)
	})

	t.Run("missing permissions", func(t *testing.T) {
		svc := newTestRoomService()
		grant := &auth.ClaimGrants{
			Video: &auth.VideoGrant{},
		}
		ctx := service.WithGrants(context.Background(), grant)
		_, err := svc.DeleteRoom(ctx, &livekit.DeleteRoomRequest{
			Room: "testroom",
		})
		require.Error(t, err)
	})
}

func TestMetaDataLimits(t *testing.T) {
	t.Run("metadata exceed limits", func(t *testing.T) {
		svc := newTestRoomService()
		grant := &auth.ClaimGrants{
			Video: &auth.VideoGrant{},
		}
		ctx := service.WithGrants(context.Background(), grant)
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

	t.Run("metadata not exceed limits", func(t *testing.T) {
		svc := newTestRoomService()
		grant := &auth.ClaimGrants{
			Video: &auth.VideoGrant{},
		}
		ctx := service.WithGrants(context.Background(), grant)
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

func newTestRoomService() *TestRoomService {
	router := &routingfakes.FakeRouter{}
	allocator := &servicefakes.FakeRoomAllocator{}
	store := &servicefakes.FakeServiceStore{}
	conf := config.RoomConfig{MaxMetadataSize: 5}
	svc, err := service.NewRoomService(allocator, store, router, conf)
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

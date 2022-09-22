package service_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/twitchtv/twirp"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing/routingfakes"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/service/servicefakes"
)

func TestDeleteRoom(t *testing.T) {
	t.Run("normal deletion", func(t *testing.T) {
		svc := newTestRoomService(config.RoomConfig{})
		grant := &auth.ClaimGrants{
			Video: &auth.VideoGrant{
				RoomCreate: true,
			},
		}
		ctx := service.WithGrants(context.Background(), grant)
		svc.store.LoadRoomReturns(nil, nil, service.ErrRoomNotFound)
		_, err := svc.DeleteRoom(ctx, &livekit.DeleteRoomRequest{
			Room: "testroom",
		})
		require.NoError(t, err)
	})

	t.Run("missing permissions", func(t *testing.T) {
		svc := newTestRoomService(config.RoomConfig{})
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
		svc := newTestRoomService(config.RoomConfig{MaxMetadataSize: 5})
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

	notExceedsLimitsSvc := map[string]*TestRoomService{
		"metadata noe exceeds limits": newTestRoomService(config.RoomConfig{MaxMetadataSize: 5}),
		"metadata no limits":          newTestRoomService(config.RoomConfig{}), // no limits
	}

	for n, s := range notExceedsLimitsSvc {
		svc := s
		t.Run(n, func(t *testing.T) {
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
}

func newTestRoomService(conf config.RoomConfig) *TestRoomService {
	router := &routingfakes.FakeRouter{}
	allocator := &servicefakes.FakeRoomAllocator{}
	store := &servicefakes.FakeServiceStore{}
	svc, err := service.NewRoomService(conf, router, allocator, store, nil)
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

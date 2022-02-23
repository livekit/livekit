package service_test

import (
	"context"
	"testing"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/routing/routingfakes"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/service/servicefakes"
)

const grantsKey = "grants"

func TestDeleteRoom(t *testing.T) {
	t.Run("normal deletion", func(t *testing.T) {
		svc := newTestRoomService()
		grant := &auth.ClaimGrants{
			Video: &auth.VideoGrant{
				RoomCreate: true,
			},
		}
		ctx := context.WithValue(context.Background(), grantsKey, grant)
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
		ctx := context.WithValue(context.Background(), grantsKey, grant)
		_, err := svc.DeleteRoom(ctx, &livekit.DeleteRoomRequest{
			Room: "testroom",
		})
		require.Error(t, err)
	})
}

func newTestRoomService() *TestRoomService {
	router := &routingfakes.FakeRouter{}
	allocator := &servicefakes.FakeRoomAllocator{}
	store := &servicefakes.FakeServiceStore{}
	svc, err := service.NewRoomService(allocator, store, router)
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

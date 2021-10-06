package service_test

import (
	"context"
	"testing"

	"github.com/livekit/livekit-server/pkg/routing/selector"
	livekit "github.com/livekit/protocol/proto"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/routing/routingfakes"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/service/servicefakes"
)

func TestCreateRoom(t *testing.T) {
	ra, conf := newTestRoomAllocator(t)

	t.Run("ensure default room settings are applied", func(t *testing.T) {
		room, err := ra.CreateRoom(context.Background(), &livekit.CreateRoomRequest{Name: "myroom"})
		require.NoError(t, err)
		require.Equal(t, conf.Room.EmptyTimeout, room.EmptyTimeout)
		require.NotEmpty(t, room.EnabledCodecs)
	})
}

func newTestRoomAllocator(t *testing.T) (*service.RoomAllocator, *config.Config) {
	store := &servicefakes.FakeRoomStore{}
	store.LoadRoomReturns(nil, service.ErrRoomNotFound)
	router := &routingfakes.FakeRouter{}
	conf, err := config.NewConfig("", nil)
	require.NoError(t, err)
	selector := &selector.RandomSelector{}
	node, err := routing.NewLocalNode(conf)
	require.NoError(t, err)

	router.GetNodeForRoomReturns(node, nil)

	ra := service.NewRoomAllocator(conf, router, selector, store)
	return ra, conf
}

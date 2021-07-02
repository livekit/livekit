package service_test

import (
	"testing"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/routing/routingfakes"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/service/servicefakes"
	livekit "github.com/livekit/livekit-server/proto"
	"github.com/stretchr/testify/require"
)

func TestCreateRoom(t *testing.T) {
	manager, conf := newTestRoomManager(t)

	t.Run("ensure default room settings are applied", func(t *testing.T) {
		room, err := manager.CreateRoom(&livekit.CreateRoomRequest{Name: "myroom"})
		require.NoError(t, err)
		require.Equal(t, conf.Room.EmptyTimeout, room.EmptyTimeout)
		require.Len(t, room.EnabledCodecs, 4)
	})
}

func newTestRoomManager(t *testing.T) (*service.RoomManager, *config.Config) {
	store := &servicefakes.FakeRoomStore{}
	store.GetRoomReturns(nil, service.ErrRoomNotFound)
	router := &routingfakes.FakeRouter{}
	conf, err := config.NewConfig("")
	require.NoError(t, err)
	selector := &routing.RandomSelector{}
	node, err := routing.NewLocalNode(conf)
	require.NoError(t, err)

	router.GetNodeForRoomReturns(node, nil)

	rm, err := service.NewRoomManager(store, router, node, selector, conf)
	require.NoError(t, err)

	return rm, conf
}

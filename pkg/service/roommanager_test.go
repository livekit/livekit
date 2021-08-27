package service_test

import (
	"testing"

	livekit "github.com/livekit/protocol/proto"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/routing/routingfakes"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/service/servicefakes"
)

func TestCreateRoom(t *testing.T) {
	manager, conf := newTestRoomManager(t)

	t.Run("ensure default room settings are applied", func(t *testing.T) {
		room, err := manager.CreateRoom(&livekit.CreateRoomRequest{Name: "myroom"})
		require.NoError(t, err)
		require.Equal(t, conf.Room.EmptyTimeout, room.EmptyTimeout)
		require.NotEmpty(t, room.EnabledCodecs)
	})
}

func newTestRoomManager(t *testing.T) (*service.LocalRoomManager, *config.Config) {
	store := &servicefakes.FakeRoomStore{}
	store.LoadRoomReturns(nil, service.ErrRoomNotFound)
	router := &routingfakes.FakeRouter{}
	conf, err := config.NewConfig("", nil)
	require.NoError(t, err)
	selector := &routing.RandomSelector{}
	node, err := routing.NewLocalNode(conf)
	require.NoError(t, err)

	router.GetNodesForRoomReturns([]*livekit.Node{node}, nil)

	rm, err := service.NewLocalRoomManager(store, router, node, selector, nil, conf)
	require.NoError(t, err)

	return rm, conf
}

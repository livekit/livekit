package service_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/routing/routingfakes"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/service/servicefakes"
)

func TestCreateRoom(t *testing.T) {
	t.Run("ensure default room settings are applied", func(t *testing.T) {
		conf, err := config.NewConfig("", true, nil, nil)
		require.NoError(t, err)

		node, err := routing.NewLocalNode(conf)
		require.NoError(t, err)

		ra, conf := newTestRoomAllocator(t, conf, node)

		room, err := ra.CreateRoom(context.Background(), &livekit.CreateRoomRequest{Name: "myroom"})
		require.NoError(t, err)
		require.Equal(t, conf.Room.EmptyTimeout, room.EmptyTimeout)
		require.NotEmpty(t, room.EnabledCodecs)
	})

	t.Run("reject new participants when track limit has been reached", func(t *testing.T) {
		conf, err := config.NewConfig("", true, nil, nil)
		require.NoError(t, err)
		conf.Limit.NumTracks = 10

		node, err := routing.NewLocalNode(conf)
		require.NoError(t, err)
		node.Stats.NumTracksIn = 100
		node.Stats.NumTracksOut = 100

		ra, _ := newTestRoomAllocator(t, conf, node)

		_, err = ra.CreateRoom(context.Background(), &livekit.CreateRoomRequest{Name: "low-limit-room"})
		require.ErrorIs(t, err, routing.ErrNodeLimitReached)
	})

	t.Run("reject new participants when bandwidth limit has been reached", func(t *testing.T) {
		conf, err := config.NewConfig("", true, nil, nil)
		require.NoError(t, err)
		conf.Limit.BytesPerSec = 100

		node, err := routing.NewLocalNode(conf)
		require.NoError(t, err)
		node.Stats.BytesInPerSec = 1000
		node.Stats.BytesOutPerSec = 1000

		ra, _ := newTestRoomAllocator(t, conf, node)

		_, err = ra.CreateRoom(context.Background(), &livekit.CreateRoomRequest{Name: "low-limit-room"})
		require.ErrorIs(t, err, routing.ErrNodeLimitReached)
	})
}

func newTestRoomAllocator(t *testing.T, conf *config.Config, node *livekit.Node) (service.RoomAllocator, *config.Config) {
	store := &servicefakes.FakeObjectStore{}
	store.LoadRoomReturns(nil, nil, service.ErrRoomNotFound)
	router := &routingfakes.FakeRouter{}

	router.GetNodeForRoomReturns(node, nil)

	ra, err := service.NewRoomAllocator(conf, router, store)
	require.NoError(t, err)
	return ra, conf
}

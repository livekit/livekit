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

		ra, conf := newTestRoomAllocator(t, conf, node.Clone())

		room, _, _, err := ra.CreateRoom(context.Background(), &livekit.CreateRoomRequest{Name: "myroom"}, true)
		require.NoError(t, err)
		require.Equal(t, conf.Room.EmptyTimeout, room.EmptyTimeout)
		require.Equal(t, conf.Room.DepartureTimeout, room.DepartureTimeout)
		require.NotEmpty(t, room.EnabledCodecs)
	})
}

func SelectRoomNode(t *testing.T) {
	t.Run("reject new participants when track limit has been reached", func(t *testing.T) {
		conf, err := config.NewConfig("", true, nil, nil)
		require.NoError(t, err)
		conf.Limit.NumTracks = 10

		node, err := routing.NewLocalNode(conf)
		require.NoError(t, err)
		node.SetStats(&livekit.NodeStats{
			NumTracksIn:  100,
			NumTracksOut: 100,
		})

		ra, _ := newTestRoomAllocator(t, conf, node.Clone())

		err = ra.SelectRoomNode(context.Background(), "low-limit-room", "")
		require.ErrorIs(t, err, routing.ErrNodeLimitReached)
	})

	t.Run("reject new participants when bandwidth limit has been reached", func(t *testing.T) {
		conf, err := config.NewConfig("", true, nil, nil)
		require.NoError(t, err)
		conf.Limit.BytesPerSec = 100

		node, err := routing.NewLocalNode(conf)
		require.NoError(t, err)
		node.SetStats(&livekit.NodeStats{
			BytesInPerSec:  1000,
			BytesOutPerSec: 1000,
		})

		ra, _ := newTestRoomAllocator(t, conf, node.Clone())

		err = ra.SelectRoomNode(context.Background(), "low-limit-room", "")
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

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

		ra, _ := newTestRoomAllocator(t, conf, node.Clone())

		room, _, _, err := ra.CreateRoom(context.Background(), &livekit.CreateRoomRequest{Name: "myroom"}, true)
		require.NoError(t, err)
		require.Equal(t, conf.Room.EmptyTimeout, room.EmptyTimeout)
		require.Equal(t, conf.Room.DepartureTimeout, room.DepartureTimeout)
		require.NotEmpty(t, room.EnabledCodecs)
	})
}

func TestSelectRoomNode(t *testing.T) {
	t.Run("reject new participants when track limit has been reached", func(t *testing.T) {
		conf, err := config.NewConfig("", true, nil, nil)
		require.NoError(t, err)
		conf.Limit.NumTracks = 10

		node, err := routing.NewLocalNode(conf)
		require.NoError(t, err)
		node.SetStats(&livekit.NodeStats{
			UpdatedAt:    time.Now().Unix(),
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
			UpdatedAt: time.Now().Unix(),
			// the limit is read off the most recent rate sample, the per-second
			// fields on the stats themselves having been left behind
			Rates: []*livekit.NodeStatsRate{{
				BytesIn:  1000,
				BytesOut: 1000,
			}},
		})

		ra, _ := newTestRoomAllocator(t, conf, node.Clone())

		err = ra.SelectRoomNode(context.Background(), "low-limit-room", "")
		require.ErrorIs(t, err, routing.ErrNodeLimitReached)
	})

	// A node that has begun draining is on its way out, and takes everything on
	// it along. Leaving the room there hands the participants about to join a
	// session that ends when the node does, while a node that is staying up is
	// standing right there.
	t.Run("move the room off a node that has begun draining", func(t *testing.T) {
		conf, err := config.NewConfig("", true, nil, nil)
		require.NoError(t, err)

		draining := registeredNode(t, conf, livekit.NodeState_SHUTTING_DOWN)
		serving := registeredNode(t, conf, livekit.NodeState_SERVING)

		ra, router := newTestRoomAllocator(t, conf, draining)
		router.ListNodesReturns([]*livekit.Node{draining, serving}, nil)

		require.NoError(t, ra.SelectRoomNode(context.Background(), "draining-room", ""))

		require.Equal(t, 1, router.SetNodeForRoomCallCount(), "the room was left on the draining node")
		_, roomName, nodeID := router.SetNodeForRoomArgsForCall(0)
		require.Equal(t, livekit.RoomName("draining-room"), roomName)
		require.Equal(t, livekit.NodeID(serving.Id), nodeID)
	})
}

func newTestRoomAllocator(t *testing.T, conf *config.Config, node *livekit.Node) (service.RoomAllocator, *routingfakes.FakeRouter) {
	t.Helper()

	store := &servicefakes.FakeObjectStore{}
	store.LoadRoomReturns(nil, nil, service.ErrRoomNotFound)
	router := &routingfakes.FakeRouter{}

	router.GetNodeForRoomReturns(node, nil)

	ra, err := service.NewRoomAllocator(conf, router, store)
	require.NoError(t, err)
	return ra, router
}

// registeredNode is a node as the registry would have it: registered a moment
// ago, and so as fresh as any node the router would hand back.
func registeredNode(t *testing.T, conf *config.Config, state livekit.NodeState) *livekit.Node {
	t.Helper()

	node, err := routing.NewLocalNode(conf)
	require.NoError(t, err)

	n := node.Clone()
	n.State = state
	return n
}

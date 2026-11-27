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
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
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

		room, _, _, _, err := ra.CreateRoom(context.Background(), &livekit.CreateRoomRequest{Name: "myroom"}, true)
		require.NoError(t, err)
		require.Equal(t, conf.Room.EmptyTimeout, room.EmptyTimeout)
		require.Equal(t, conf.Room.DepartureTimeout, room.DepartureTimeout)
		require.NotEmpty(t, room.EnabledCodecs)
	})

	t.Run("return the request with the room preset applied", func(t *testing.T) {
		conf, err := config.NewConfig(`
room:
  room_configurations:
    record:
      egress:
        room:
          layout: grid
`, true, nil, nil)
		require.NoError(t, err)

		node, err := routing.NewLocalNode(conf)
		require.NoError(t, err)

		ra, conf := newTestRoomAllocator(t, conf, node.Clone())

		// without a preset, the request itself is returned
		req := &livekit.CreateRoomRequest{Name: "myroom"}
		_, _, applied, _, err := ra.CreateRoom(context.Background(), req, true)
		require.NoError(t, err)
		require.Same(t, req, applied)

		// with a preset, a copy with the preset applied is returned,
		// leaving both the request and the preset unchanged
		req = &livekit.CreateRoomRequest{Name: "myroom", RoomPreset: "record"}
		_, _, applied, _, err = ra.CreateRoom(context.Background(), req, true)
		require.NoError(t, err)
		require.Nil(t, req.Egress)
		require.Equal(t, "grid", applied.GetEgress().GetRoom().GetLayout())
		require.NotSame(t, conf.Room.RoomConfigurations["record"].Egress.Room, applied.Egress.Room)
	})

	t.Run("update a stored room without an internal record", func(t *testing.T) {
		conf, err := config.NewConfig("", true, nil, nil)
		require.NoError(t, err)
		conf.Room.PlayoutDelay = config.PlayoutDelayConfig{Enabled: true, Min: 100, Max: 200}

		node, err := routing.NewLocalNode(conf)
		require.NoError(t, err)

		store := &servicefakes.FakeObjectStore{}
		store.LoadRoomReturns(&livekit.Room{Sid: "RM_existing", Name: "myroom", EmptyTimeout: 77}, nil, nil)
		router := &routingfakes.FakeRouter{}
		router.GetNodeForRoomReturns(node.Clone(), nil)
		ra, err := service.NewRoomAllocator(conf, router, store)
		require.NoError(t, err)

		room, internal, _, created, err := ra.CreateRoom(context.Background(), &livekit.CreateRoomRequest{Name: "myroom", SyncStreams: true}, true)
		require.NoError(t, err)
		require.False(t, created)
		require.True(t, internal.GetSyncStreams())
		// the internal defaults are restored, the stored room keeps its own settings
		require.Equal(t, uint32(100), internal.GetPlayoutDelay().GetMin())
		require.Equal(t, uint32(77), room.EmptyTimeout)
	})
}

func TestImplicitRoomCreateMetrics(t *testing.T) {
	ctx := context.Background()
	conf, err := config.NewConfig("", true, nil, nil)
	require.NoError(t, err)
	roomStore := &servicefakes.FakeObjectStore{}
	allocator, err := service.NewRoomAllocator(conf, &routingfakes.FakeRouter{}, roomStore)
	require.NoError(t, err)
	req := &livekit.CreateRoomRequest{Name: "test-room"}

	before := implicitRoomCreateCounts(t)

	roomStore.LoadRoomReturns(nil, nil, service.ErrRoomNotFound)
	_, _, _, created, err := allocator.CreateRoom(ctx, req, false)
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, before["success"]+1, implicitRoomCreateCounts(t)["success"])

	storeError := errors.New("room store unavailable")
	roomStore.StoreRoomReturns(storeError)
	_, _, _, _, err = allocator.CreateRoom(ctx, req, false)
	require.ErrorIs(t, err, storeError)
	require.Equal(t, before["error/store_room"]+1, implicitRoomCreateCounts(t)["error/store_room"])

	rm := newTestRoomManager(t, conf, nil)
	err = rm.StartSession(ctx, routing.ParticipantInit{
		Identity:   "test-participant",
		CreateRoom: &livekit.CreateRoomRequest{Name: "test-room", RoomPreset: "undefined"},
	}, nil, nil, false)
	require.Error(t, err)

	_, _, _, _, err = allocator.CreateRoom(ctx, req, true)
	require.ErrorIs(t, err, storeError)
	roomStore.StoreRoomReturns(nil)
	_, _, _, _, err = allocator.CreateRoom(ctx, req, true)
	require.NoError(t, err)

	roomStore.LoadRoomReturns(&livekit.Room{Name: req.Name}, &livekit.RoomInternal{}, nil)
	_, _, _, created, err = allocator.CreateRoom(ctx, req, false)
	require.NoError(t, err)
	require.False(t, created)

	roomStore.LoadRoomReturns(nil, nil, storeError)
	_, _, _, _, err = allocator.CreateRoom(ctx, req, false)
	require.ErrorIs(t, err, storeError)

	require.Equal(t, map[string]float64{
		"success":                 before["success"] + 1,
		"error/store_room":        before["error/store_room"] + 1,
		"error/apply_room_preset": before["error/apply_room_preset"] + 1,
	}, implicitRoomCreateCounts(t))
}

func implicitRoomCreateCounts(t *testing.T) map[string]float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	counts := make(map[string]float64)
	for _, family := range families {
		if family.GetName() != "livekit_node_service_operation" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := make(map[string]string)
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["type"] != "implicit_room_create" {
				continue
			}
			key := labels["status"]
			if errorType := labels["error_type"]; errorType != "" {
				key += "/" + errorType
			}
			counts[key] += metric.GetCounter().GetValue()
		}
	}
	return counts
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

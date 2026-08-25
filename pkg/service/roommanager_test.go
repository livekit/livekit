// Copyright 2026 LiveKit, Inc.
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

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/routing/routingfakes"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/telemetry/telemetryfakes"
)

// fakeRoomStore counts the rooms deleted from it. It embeds the interface so
// only the method under test needs to be implemented; any other call would
// panic (and we assert none happen).
type fakeRoomStore struct {
	ObjectStore
	deleteCount atomic.Int32
}

func (f *fakeRoomStore) DeleteRoom(_ context.Context, _ livekit.RoomName) error {
	f.deleteCount.Inc()
	return nil
}

// A room can be taken over by a node that is staying up before the node it was
// on has finished closing its copy. What that node deletes on the way out has
// to be its own: a room's records are keyed by its name and nothing else, so
// the ones it would reach for are the ones being served on the other node.
func TestDeleteRoomState(t *testing.T) {
	room := &livekit.Room{Name: "test-room"}

	newRoomManager := func(cleared bool, err error) (*RoomManager, *fakeRoomStore, *telemetryfakes.FakeTelemetryService) {
		store := &fakeRoomStore{}
		router := &routingfakes.FakeRouter{}
		router.ClearRoomStateReturns(cleared, err)
		fakeTelemetry := &telemetryfakes.FakeTelemetryService{}

		return &RoomManager{
			router:    router,
			roomStore: store,
			telemetry: fakeTelemetry,
			rooms:     map[livekit.RoomName]*rtc.Room{"test-room": {}},
		}, store, fakeTelemetry
	}

	t.Run("delete a room this node was hosting", func(t *testing.T) {
		r, store, telemetry := newRoomManager(true, nil)

		require.NoError(t, r.deleteRoom(context.Background(), room))
		require.EqualValues(t, 1, store.deleteCount.Load(), "the room was left in the store")
		require.NotContains(t, r.rooms, livekit.RoomName("test-room"), "the room is still being served")
		require.Equal(t, 1, telemetry.RoomEndedCallCount(), "the room ended here and was not announced")
	})

	// The webhook goes with the records: a room that has moved has not ended,
	// and the node now serving it is the one that will say when it has.
	t.Run("leave a room that has moved to another node", func(t *testing.T) {
		r, store, telemetry := newRoomManager(false, nil)

		require.NoError(t, r.deleteRoom(context.Background(), room))
		require.Zero(t, store.deleteCount.Load(), "a room being hosted elsewhere was deleted")
		require.NotContains(t, r.rooms, livekit.RoomName("test-room"), "the room is still being served")
		require.Zero(t, telemetry.RoomEndedCallCount(), "a room being hosted elsewhere was announced as ended")
	})

	// Not knowing whether the room is still this node's is a reason to leave it
	// alone, not a reason to go ahead: the records are shared, and the node that
	// may have taken the room over is serving participants out of them.
	t.Run("leave a room whose routing could not be read", func(t *testing.T) {
		outage := errors.New("redis is down")
		r, store, telemetry := newRoomManager(false, outage)

		require.ErrorIs(t, r.deleteRoom(context.Background(), room), outage)
		require.Zero(t, store.deleteCount.Load(), "a room of unknown ownership was deleted")
		require.NotContains(t, r.rooms, livekit.RoomName("test-room"), "the room is still being served")
		require.Zero(t, telemetry.RoomEndedCallCount(), "a room of unknown ownership was announced as ended")
	})
}

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

package service_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/psrpc"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/routing/routingfakes"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/telemetry/telemetryfakes"
)

// fakeEgressLauncher records the egress start requests it receives.
type fakeEgressLauncher struct {
	mu      sync.Mutex
	started []*rpc.StartEgressRequest
}

func (f *fakeEgressLauncher) StartEgress(_ context.Context, req *rpc.StartEgressRequest) (*livekit.EgressInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, req)
	return &livekit.EgressInfo{}, nil
}

func (f *fakeEgressLauncher) StopEgress(context.Context, *livekit.StopEgressRequest) (*livekit.EgressInfo, error) {
	return &livekit.EgressInfo{}, nil
}

func (f *fakeEgressLauncher) roomCompositeRoomNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	var names []string
	for _, req := range f.started {
		if rc := req.GetRoomComposite(); rc != nil {
			names = append(names, rc.RoomName)
		}
	}
	return names
}

func newTestRoomManager(t *testing.T, conf *config.Config, egressLauncher rtc.EgressLauncher) *service.RoomManager {
	// no fixed RTC ports, so that the test does not bind the defaults
	conf.RTC.TCPPort = 0
	conf.RTC.UDPPort = rtcconfig.PortRange{}

	node, err := routing.NewLocalNode(conf)
	require.NoError(t, err)
	router := &routingfakes.FakeRouter{}
	router.GetNodeForRoomReturns(node.Clone(), nil)

	store := service.NewLocalStore()
	ra, err := service.NewRoomAllocator(conf, router, store)
	require.NoError(t, err)

	rm, err := service.NewLocalRoomManager(
		conf,
		store,
		node,
		router,
		ra,
		&telemetryfakes.FakeTelemetryService{},
		nil,
		store,
		egressLauncher,
		utils.NewDefaultTimedVersionGenerator(),
		nil,
		psrpc.NewLocalMessageBus(),
		nil,
	)
	require.NoError(t, err)
	t.Cleanup(rm.Stop)
	return rm
}

func TestRoomCompositeEgressOnCreateRoom(t *testing.T) {
	t.Run("from request", func(t *testing.T) {
		conf, err := config.NewConfig("", true, nil, nil)
		require.NoError(t, err)
		launcher := &fakeEgressLauncher{}
		rm := newTestRoomManager(t, conf, launcher)

		_, err = rm.CreateRoom(context.Background(), &livekit.CreateRoomRequest{
			Name: "room-a",
			Egress: &livekit.RoomEgress{
				Room: &livekit.RoomCompositeEgressRequest{
					Layout:      "grid",
					FileOutputs: []*livekit.EncodedFileOutput{{Filepath: "recordings/{room_name}.mp4"}},
				},
			},
		})
		require.NoError(t, err)
		require.Equal(t, []string{"room-a"}, launcher.roomCompositeRoomNames())
	})

	t.Run("from room preset", func(t *testing.T) {
		conf, err := config.NewConfig(roomPresetConfig, true, nil, nil)
		require.NoError(t, err)
		launcher := &fakeEgressLauncher{}
		rm := newTestRoomManager(t, conf, launcher)

		for _, name := range []string{"room-a", "room-b"} {
			_, err = rm.CreateRoom(context.Background(), &livekit.CreateRoomRequest{
				Name:       name,
				RoomPreset: "record",
			})
			require.NoError(t, err)
		}

		// each room records itself, and the preset is left as configured
		require.Equal(t, []string{"room-a", "room-b"}, launcher.roomCompositeRoomNames())
		require.Empty(t, conf.Room.RoomConfigurations["record"].GetEgress().GetRoom().GetRoomName())
	})

	t.Run("from room preset when a session starts", func(t *testing.T) {
		conf, err := config.NewConfig(roomPresetConfig, true, nil, nil)
		require.NoError(t, err)
		launcher := &fakeEgressLauncher{}
		rm := newTestRoomManager(t, conf, launcher)

		// the join and WHIP paths create the room through StartSession, with the
		// token's room preset; without an identity only the room is created
		err = rm.StartSession(context.Background(), routing.ParticipantInit{
			CreateRoom: &livekit.CreateRoomRequest{
				Name:       "room-a",
				RoomPreset: "record",
			},
		}, nil, nil, false)
		require.NoError(t, err)
		require.Equal(t, []string{"room-a"}, launcher.roomCompositeRoomNames())
	})

	t.Run("from room preset without an egress launcher", func(t *testing.T) {
		conf, err := config.NewConfig(roomPresetConfig, true, nil, nil)
		require.NoError(t, err)
		rm := newTestRoomManager(t, conf, nil)

		_, err = rm.CreateRoom(context.Background(), &livekit.CreateRoomRequest{
			Name:       "room-a",
			RoomPreset: "record",
		})
		require.ErrorIs(t, err, service.ErrEgressNotConnected)
	})
}

const roomPresetConfig = `
room:
  room_configurations:
    record:
      egress:
        room:
          layout: grid
          file_outputs:
            - filepath: recordings/{room_name}.mp4
`

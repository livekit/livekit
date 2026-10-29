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
	"time"

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
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/service/servicefakes"
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
	node, err := routing.NewLocalNode(conf)
	require.NoError(t, err)
	router := &routingfakes.FakeRouter{}
	router.GetNodeForRoomReturns(node.Clone(), nil)

	store := service.NewLocalStore()
	ra, err := service.NewRoomAllocator(conf, router, store)
	require.NoError(t, err)

	return newTestRoomManagerWith(t, conf, node, router, store, ra, egressLauncher)
}

func newTestRoomManagerWith(
	t *testing.T,
	conf *config.Config,
	node routing.LocalNode,
	router routing.Router,
	store service.ObjectStore,
	ra service.RoomAllocator,
	egressLauncher rtc.EgressLauncher,
) *service.RoomManager {
	// no fixed RTC ports, so that the test does not bind the defaults
	conf.RTC.TCPPort = 0
	conf.RTC.UDPPort = rtcconfig.PortRange{}

	// a LocalStore serves as both stores, a fake room store does not implement AgentStore
	agentStore, ok := store.(service.AgentStore)
	if !ok {
		agentStore = service.NewLocalStore()
	}

	rm, err := service.NewLocalRoomManager(
		conf,
		store,
		node,
		router,
		ra,
		&telemetryfakes.FakeTelemetryService{},
		nil,
		agentStore,
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

// TestStartSessionConnectLimits checks that StartSession on the RTC node rejects the same
// over-limit joins as the HTTP join paths, without relying on the caller's validation.
func TestStartSessionConnectLimits(t *testing.T) {
	for _, tc := range service.ConnectLimitCases() {
		t.Run(tc.Name, func(t *testing.T) {
			conf, err := config.NewConfig("", true, nil, nil)
			require.NoError(t, err)
			conf.Limit = tc.Limit
			rm := newTestRoomManager(t, conf, nil)

			g := tc.Grant()
			err = rm.StartSession(context.Background(), routing.ParticipantInit{
				Identity:   livekit.ParticipantIdentity(g.Identity),
				Grants:     g,
				Client:     &livekit.ClientInfo{},
				CreateRoom: &livekit.CreateRoomRequest{Name: g.Video.Room},
			}, &routingfakes.FakeMessageSource{}, &routingfakes.FakeMessageSink{}, false)
			require.ErrorIs(t, err, tc.Err)
		})
	}
}

// newRoomStateTestManager returns a room manager whose router and room store the test controls.
// The router maps rooms to no node until the test says otherwise.
func newRoomStateTestManager(t *testing.T, conf *config.Config) (*service.RoomManager, routing.LocalNode, *routingfakes.FakeRouter, *servicefakes.FakeObjectStore) {
	node, err := routing.NewLocalNode(conf)
	require.NoError(t, err)
	router := &routingfakes.FakeRouter{}
	router.GetNodeForRoomReturns(nil, routing.ErrNotFound)

	store := &servicefakes.FakeObjectStore{}
	store.LoadRoomReturns(nil, nil, service.ErrRoomNotFound)
	ra, err := service.NewRoomAllocator(conf, router, store)
	require.NoError(t, err)

	return newTestRoomManagerWith(t, conf, node, router, store, ra, nil), node, router, store
}

func joinMockParticipant(t *testing.T, room *rtc.Room, identity livekit.ParticipantIdentity) *typesfakes.FakeLocalParticipant {
	p := rtc.NewMockParticipant(identity, types.CurrentProtocol, false, true, room.LocalParticipantListener())
	require.NoError(t, room.Join(p, nil, &rtc.ParticipantOptions{}, nil))
	return p
}

func TestDeleteRoomState(t *testing.T) {
	const roomName = "room"
	otherNode := &livekit.Node{Id: "ND_other"}

	for _, tc := range []struct {
		name          string
		roomNode      func(self *livekit.Node) (*livekit.Node, error)
		lockErr       error
		stateDeletion bool
	}{
		{
			name:          "room mapped to this node",
			roomNode:      func(self *livekit.Node) (*livekit.Node, error) { return self, nil },
			stateDeletion: true,
		},
		{
			name:          "room mapped to no node",
			roomNode:      func(*livekit.Node) (*livekit.Node, error) { return nil, routing.ErrNotFound },
			stateDeletion: true,
		},
		{
			// the room moved, its state belongs to the other node now
			name:     "room mapped to another node",
			roomNode: func(*livekit.Node) (*livekit.Node, error) { return otherNode, nil },
		},
		{
			// without the lock the state is not deleted, and the closed room is not kept either
			name:     "room lock fails",
			roomNode: func(self *livekit.Node) (*livekit.Node, error) { return self, nil },
			lockErr:  service.ErrRoomLockFailed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conf, err := config.NewConfig("", true, nil, nil)
			require.NoError(t, err)
			rm, node, router, store := newRoomStateTestManager(t, conf)

			_, err = rm.CreateRoom(context.Background(), &livekit.CreateRoomRequest{Name: roomName})
			require.NoError(t, err)

			router.GetNodeForRoomReturns(tc.roomNode(node.Clone()))
			store.LockRoomReturns("", tc.lockErr)

			_, err = rm.DeleteRoom(context.Background(), &livekit.DeleteRoomRequest{Room: roomName})
			require.NoError(t, err)

			require.Nil(t, rm.GetRoom(context.Background(), roomName))
			// deleted once, before the close, and not again when the room closes
			expected := 0
			if tc.stateDeletion {
				expected = 1
			}
			require.Equal(t, expected, store.DeleteRoomCallCount())
			require.Equal(t, expected, router.ClearRoomStateCallCount())
		})
	}
}

func TestClaimRoomNodeOnCreate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		roomNode func(self *livekit.Node) (*livekit.Node, error)
		claimed  bool
	}{
		{
			name:     "room mapped to no node",
			roomNode: func(*livekit.Node) (*livekit.Node, error) { return nil, routing.ErrNotFound },
			claimed:  true,
		},
		{
			name:     "room mapped to this node",
			roomNode: func(self *livekit.Node) (*livekit.Node, error) { return self, nil },
		},
		{
			name:     "room mapped to another node",
			roomNode: func(*livekit.Node) (*livekit.Node, error) { return &livekit.Node{Id: "ND_other"}, nil },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conf, err := config.NewConfig("", true, nil, nil)
			require.NoError(t, err)
			rm, node, router, _ := newRoomStateTestManager(t, conf)
			router.GetNodeForRoomReturns(tc.roomNode(node.Clone()))

			_, err = rm.CreateRoom(context.Background(), &livekit.CreateRoomRequest{Name: "room"})
			require.NoError(t, err)

			if !tc.claimed {
				require.Zero(t, router.SetNodeForRoomCallCount())
				return
			}
			require.Equal(t, 1, router.SetNodeForRoomCallCount())
			_, roomName, nodeID := router.SetNodeForRoomArgsForCall(0)
			require.Equal(t, livekit.RoomName("room"), roomName)
			require.Equal(t, node.NodeID(), nodeID)
		})
	}
}

// TestCreateRoomReloadsAfterTeardown checks that a room created while a same-name room is torn
// down reloads its details once the teardown is done.
func TestCreateRoomReloadsAfterTeardown(t *testing.T) {
	type load struct {
		sid     string
		created bool
	}
	for _, tc := range []struct {
		name string
		// what the details load returns while the teardown runs, and after it
		during, after load
	}{
		{
			// the load during the teardown finds the old room before its state is deleted
			name:   "old room loaded during teardown",
			during: load{sid: "RM_old"},
			after:  load{sid: "RM_new", created: true},
		},
		{
			// the load during the teardown creates the room, the reload finds it
			name:   "room created during teardown",
			during: load{sid: "RM_new", created: true},
			after:  load{sid: "RM_new"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const roomName = "room"
			conf, err := config.NewConfig("", true, nil, nil)
			require.NoError(t, err)
			node, err := routing.NewLocalNode(conf)
			require.NoError(t, err)
			router := &routingfakes.FakeRouter{}
			router.GetNodeForRoomReturns(node.Clone(), nil)

			ra := &servicefakes.FakeRoomAllocator{}
			ra.CreateRoomStub = func(_ context.Context, req *livekit.CreateRoomRequest, _ bool) (*livekit.Room, *livekit.RoomInternal, *livekit.CreateRoomRequest, bool, error) {
				return &livekit.Room{Name: req.Name, Sid: "RM_old"}, &livekit.RoomInternal{}, req, true, nil
			}
			launcher := &fakeEgressLauncher{}
			rm := newTestRoomManagerWith(t, conf, node, router, service.NewLocalStore(), ra, launcher)

			_, err = rm.CreateRoom(context.Background(), &livekit.CreateRoomRequest{Name: roomName})
			require.NoError(t, err)
			oldRoom := rm.GetRoom(context.Background(), roomName)

			// hold the old room in its close, closed but still registered
			unblock := make(chan struct{})
			p := joinMockParticipant(t, oldRoom, "p")
			p.CloseStub = func(bool, types.ParticipantCloseReason, bool) error {
				<-unblock
				return nil
			}
			go oldRoom.Close(types.RoomCloseReasonIdleTimeout)
			require.Eventually(t, oldRoom.IsClosed, time.Second, 10*time.Millisecond)

			// the old room finishes its teardown while the new room's details load
			ra.CreateRoomStub = func(_ context.Context, req *livekit.CreateRoomRequest, _ bool) (*livekit.Room, *livekit.RoomInternal, *livekit.CreateRoomRequest, bool, error) {
				l := tc.after
				if ra.CreateRoomCallCount() == 2 {
					close(unblock)
					require.Eventually(t, func() bool { return rm.GetRoom(context.Background(), roomName) == nil }, time.Second, 10*time.Millisecond)
					l = tc.during
				}
				return &livekit.Room{Name: req.Name, Sid: l.sid}, &livekit.RoomInternal{}, req, l.created, nil
			}

			newRoom, err := rm.CreateRoom(context.Background(), &livekit.CreateRoomRequest{
				Name: roomName,
				Egress: &livekit.RoomEgress{
					Room: &livekit.RoomCompositeEgressRequest{Layout: "grid"},
				},
			})
			require.NoError(t, err)
			require.Equal(t, "RM_new", newRoom.Sid)
			require.Equal(t, 3, ra.CreateRoomCallCount())
			// the room's creation starts its egress, whichever load created it
			require.Equal(t, []string{roomName}, launcher.roomCompositeRoomNames())
		})
	}
}

func TestReconnectMovedRooms(t *testing.T) {
	for _, tc := range []struct {
		name       string
		enabled    bool
		roomNode   func(self *livekit.Node) *livekit.Node
		reconnects int
	}{
		{
			name:     "disabled",
			roomNode: func(*livekit.Node) *livekit.Node { return &livekit.Node{Id: "ND_other"} },
		},
		{
			name:     "room mapped to this node",
			enabled:  true,
			roomNode: func(self *livekit.Node) *livekit.Node { return self },
		},
		{
			name:       "room mapped to another node",
			enabled:    true,
			roomNode:   func(*livekit.Node) *livekit.Node { return &livekit.Node{Id: "ND_other"} },
			reconnects: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conf, err := config.NewConfig("", true, nil, nil)
			require.NoError(t, err)
			conf.Room.ReconnectMovedRooms = tc.enabled
			rm, node, router, _ := newRoomStateTestManager(t, conf)
			router.GetNodeForRoomReturns(node.Clone(), nil)

			_, err = rm.CreateRoom(context.Background(), &livekit.CreateRoomRequest{Name: "room"})
			require.NoError(t, err)
			p := joinMockParticipant(t, rm.GetRoom(context.Background(), "room"), "p")
			router.GetNodeForRoomReturns(tc.roomNode(node.Clone()), nil)

			// the second call is within the check interval
			rm.CloseIdleRooms()
			rm.CloseIdleRooms()

			require.Equal(t, tc.reconnects, p.IssueFullReconnectCallCount())
			if tc.reconnects > 0 {
				require.Equal(t, types.ParticipantCloseReasonMigrationRequested, p.IssueFullReconnectArgsForCall(0))
			}
		})
	}
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

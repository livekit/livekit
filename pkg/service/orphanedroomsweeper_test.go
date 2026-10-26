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
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/service"
)

const (
	sweepingNode = livekit.NodeID("ND_sweeping")
	liveNode     = livekit.NodeID("ND_live")
	deadNode     = livekit.NodeID("ND_dead")
)

type orphanedRoomTest struct {
	t      *testing.T
	ctx    context.Context
	rc     redis.UniversalClient
	router *routing.RedisRouter
	store  *service.RedisStore
	hosted []service.HostedRoom
}

func newOrphanedRoomTest(t *testing.T) *orphanedRoomTest {
	rc := redisClientDocker(t)
	ot := &orphanedRoomTest{
		t:      t,
		ctx:    context.Background(),
		rc:     rc,
		router: routing.NewRedisRouter(nil, rc, nil),
		store:  service.NewRedisStore(rc),
	}
	ot.registerNode(sweepingNode, time.Now())
	ot.registerNode(liveNode, time.Now())
	return ot
}

func (ot *orphanedRoomTest) registerNode(nodeID livekit.NodeID, updatedAt time.Time) {
	data, err := proto.Marshal(&livekit.Node{
		Id:    string(nodeID),
		State: livekit.NodeState_SERVING,
		Stats: &livekit.NodeStats{UpdatedAt: updatedAt.Unix()},
	})
	require.NoError(ot.t, err)
	require.NoError(ot.t, ot.rc.HSet(ot.ctx, routing.NodesKey, string(nodeID), data).Err())
}

func (ot *orphanedRoomTest) createRoom(name livekit.RoomName, nodeID livekit.NodeID, createdAt time.Time, emptyTimeout uint32) {
	ot.createRoomWithDepartureTimeout(name, nodeID, createdAt, emptyTimeout, 0)
}

func (ot *orphanedRoomTest) createRoomWithDepartureTimeout(name livekit.RoomName, nodeID livekit.NodeID, createdAt time.Time, emptyTimeout, departureTimeout uint32) {
	require.NoError(ot.t, ot.store.StoreRoom(ot.ctx, &livekit.Room{
		Sid:              "RM_" + string(name),
		Name:             string(name),
		CreationTime:     createdAt.Unix(),
		EmptyTimeout:     emptyTimeout,
		DepartureTimeout: departureTimeout,
	}, nil))
	require.NoError(ot.t, ot.router.SetNodeForRoom(ot.ctx, name, nodeID))
}

func (ot *orphanedRoomTest) sweeper(gracePeriod time.Duration) *service.OrphanedRoomSweeper {
	return service.NewOrphanedRoomSweeper(ot.router, ot.store, sweepingNode, ot.hostedRooms, gracePeriod)
}

func (ot *orphanedRoomTest) hostedRooms() []service.HostedRoom {
	return ot.hosted
}

func (ot *orphanedRoomTest) requireRoom(name livekit.RoomName, nodeID livekit.NodeID) {
	ot.t.Helper()
	exists, err := ot.store.RoomExists(ot.ctx, name)
	require.NoError(ot.t, err)
	require.True(ot.t, exists, "room %s should exist", name)
	roomNodes, err := ot.router.ListRoomNodes(ot.ctx)
	require.NoError(ot.t, err)
	require.Equal(ot.t, nodeID, roomNodes[name])
}

func (ot *orphanedRoomTest) requireNoRoom(name livekit.RoomName) {
	ot.t.Helper()
	exists, err := ot.store.RoomExists(ot.ctx, name)
	require.NoError(ot.t, err)
	require.False(ot.t, exists, "room %s should be deleted", name)
	roomNodes, err := ot.router.ListRoomNodes(ot.ctx)
	require.NoError(ot.t, err)
	require.NotContains(ot.t, roomNodes, name)
}

func TestOrphanedRoomSweeper(t *testing.T) {
	longAgo := time.Now().Add(-time.Hour)

	t.Run("deletes rooms of a node that left the cluster", func(t *testing.T) {
		ot := newOrphanedRoomTest(t)
		ot.createRoom("orphaned", deadNode, longAgo, 300)
		ot.createRoom("hosted", liveNode, longAgo, 300)

		ot.sweeper(0).Sweep(ot.ctx)

		ot.requireNoRoom("orphaned")
		ot.requireRoom("hosted", liveNode)
	})

	t.Run("deletes rooms of a node that stopped updating", func(t *testing.T) {
		ot := newOrphanedRoomTest(t)
		ot.registerNode(deadNode, longAgo)
		ot.createRoom("orphaned", deadNode, longAgo, 300)

		ot.sweeper(0).Sweep(ot.ctx)

		ot.requireNoRoom("orphaned")
	})

	t.Run("keeps a room through the grace period", func(t *testing.T) {
		ot := newOrphanedRoomTest(t)
		ot.createRoom("orphaned", deadNode, longAgo, 300)
		sweeper := ot.sweeper(time.Hour)

		sweeper.Sweep(ot.ctx)
		sweeper.Sweep(ot.ctx)

		ot.requireRoom("orphaned", deadNode)
	})

	t.Run("keeps a room until its empty timeout", func(t *testing.T) {
		ot := newOrphanedRoomTest(t)
		ot.createRoom("awaiting-first-join", deadNode, time.Now(), 300)

		ot.sweeper(0).Sweep(ot.ctx)

		ot.requireRoom("awaiting-first-join", deadNode)
	})

	t.Run("keeps a room until its departure timeout", func(t *testing.T) {
		ot := newOrphanedRoomTest(t)
		ot.createRoomWithDepartureTimeout("awaiting-rejoin", deadNode, longAgo, 300, 300)

		ot.sweeper(0).Sweep(ot.ctx)

		ot.requireRoom("awaiting-rejoin", deadNode)
	})

	t.Run("keeps a room reassigned after it was observed", func(t *testing.T) {
		ot := newOrphanedRoomTest(t)
		ot.createRoom("rejoined", deadNode, longAgo, 300)
		sweeper := service.NewOrphanedRoomSweeper(&reassigningRouter{
			RedisRouter: ot.router,
			roomName:    "rejoined",
			nodeID:      liveNode,
		}, ot.store, sweepingNode, ot.hostedRooms, 0)

		sweeper.Sweep(ot.ctx)

		ot.requireRoom("rejoined", liveNode)
	})

	t.Run("keeps a room whose node recovered during the sweep", func(t *testing.T) {
		ot := newOrphanedRoomTest(t)
		ot.registerNode(deadNode, longAgo)
		ot.createRoom("recovered", deadNode, longAgo, 300)
		sweeper := service.NewOrphanedRoomSweeper(&recoveringRouter{
			RedisRouter: ot.router,
			recover:     func() { ot.registerNode(deadNode, time.Now()) },
		}, ot.store, sweepingNode, ot.hostedRooms, 0)

		sweeper.Sweep(ot.ctx)

		ot.requireRoom("recovered", deadNode)
	})

	t.Run("restores a hosted room deleted while its node was unavailable", func(t *testing.T) {
		ot := newOrphanedRoomTest(t)
		ot.hosted = []service.HostedRoom{&fakeHostedRoom{
			room:         &livekit.Room{Sid: "RM_hosted", Name: "hosted", CreationTime: longAgo.Unix(), EmptyTimeout: 300},
			participants: []*livekit.ParticipantInfo{{Sid: "PA_1", Identity: "p1"}},
			store:        ot.store,
		}}

		ot.sweeper(0).Sweep(ot.ctx)

		ot.requireRoom("hosted", sweepingNode)
		found, err := ot.store.HasParticipant(ot.ctx, "hosted", "p1")
		require.NoError(t, err)
		require.True(t, found)
	})

	t.Run("retries restoring a room whose participants failed to store", func(t *testing.T) {
		ot := newOrphanedRoomTest(t)
		ot.hosted = []service.HostedRoom{&fakeHostedRoom{
			room:         &livekit.Room{Sid: "RM_hosted", Name: "hosted", CreationTime: longAgo.Unix(), EmptyTimeout: 300},
			participants: []*livekit.ParticipantInfo{{Sid: "PA_1", Identity: "p1"}},
		}}
		store := &failingStoreParticipantStore{RedisStore: ot.store, failures: 1}
		ot.hosted[0].(*fakeHostedRoom).store = store
		sweeper := service.NewOrphanedRoomSweeper(ot.router, store, sweepingNode, ot.hostedRooms, 0)

		sweeper.Sweep(ot.ctx)
		sweeper.Sweep(ot.ctx)

		ot.requireRoom("hosted", sweepingNode)
		found, err := ot.store.HasParticipant(ot.ctx, "hosted", "p1")
		require.NoError(t, err)
		require.True(t, found)
	})

	t.Run("leaves no participants when a partly restored room closes", func(t *testing.T) {
		ot := newOrphanedRoomTest(t)
		ot.hosted = []service.HostedRoom{&fakeHostedRoom{
			room: &livekit.Room{Sid: "RM_demo", Name: "demo", CreationTime: longAgo.Unix(), EmptyTimeout: 300},
			participants: []*livekit.ParticipantInfo{
				{Sid: "PA_alice", Identity: "alice"},
				{Sid: "PA_bob", Identity: "bob"},
			},
		}}
		store := &failingStoreParticipantStore{RedisStore: ot.store, failAfter: 1, failures: 1}
		ot.hosted[0].(*fakeHostedRoom).store = store
		service.NewOrphanedRoomSweeper(ot.router, store, sweepingNode, ot.hostedRooms, 0).Sweep(ot.ctx)

		// the room closes before the next sweep, deleting its state
		ot.hosted = nil
		require.NoError(t, ot.store.DeleteRoom(ot.ctx, "demo"))
		require.NoError(t, ot.router.ClearRoomState(ot.ctx, "demo"))

		ot.createRoom("demo", liveNode, time.Now(), 300)
		participants, err := ot.store.ListParticipants(ot.ctx, "demo")
		require.NoError(t, err)
		require.Empty(t, participants)
	})

	t.Run("restores participants of a room whose record was recreated", func(t *testing.T) {
		ot := newOrphanedRoomTest(t)
		// a room update stored the record again before the sweep, while routing is still gone
		ot.createRoom("hosted", sweepingNode, longAgo, 300)
		require.NoError(t, ot.router.ClearRoomState(ot.ctx, "hosted"))
		ot.hosted = []service.HostedRoom{&fakeHostedRoom{
			room:         &livekit.Room{Sid: "RM_hosted", Name: "hosted", CreationTime: longAgo.Unix(), EmptyTimeout: 300},
			participants: []*livekit.ParticipantInfo{{Sid: "PA_1", Identity: "p1"}},
			store:        ot.store,
		}}

		ot.sweeper(0).Sweep(ot.ctx)

		ot.requireRoom("hosted", sweepingNode)
		found, err := ot.store.HasParticipant(ot.ctx, "hosted", "p1")
		require.NoError(t, err)
		require.True(t, found)
	})

	t.Run("restores participants of a room whose routing and record were recreated", func(t *testing.T) {
		ot := newOrphanedRoomTest(t)
		ot.createRoom("hosted", sweepingNode, longAgo, 300)
		ot.hosted = []service.HostedRoom{&fakeHostedRoom{
			room:         &livekit.Room{Sid: "RM_hosted", Name: "hosted", CreationTime: longAgo.Unix(), EmptyTimeout: 300},
			participants: []*livekit.ParticipantInfo{{Sid: "PA_1", Identity: "p1"}},
			store:        ot.store,
		}}

		ot.sweeper(0).Sweep(ot.ctx)

		found, err := ot.store.HasParticipant(ot.ctx, "hosted", "p1")
		require.NoError(t, err)
		require.True(t, found)
	})

	t.Run("does not restore a hosted room reassigned to another node", func(t *testing.T) {
		ot := newOrphanedRoomTest(t)
		require.NoError(t, ot.router.SetNodeForRoom(ot.ctx, "hosted", liveNode))
		ot.hosted = []service.HostedRoom{&fakeHostedRoom{
			room: &livekit.Room{Sid: "RM_hosted", Name: "hosted", CreationTime: longAgo.Unix(), EmptyTimeout: 300},
		}}

		ot.sweeper(0).Sweep(ot.ctx)

		exists, err := ot.store.RoomExists(ot.ctx, "hosted")
		require.NoError(t, err)
		require.False(t, exists)
		roomNodes, err := ot.router.ListRoomNodes(ot.ctx)
		require.NoError(t, err)
		require.Equal(t, liveNode, roomNodes["hosted"])
	})

	t.Run("does not restore a closed room", func(t *testing.T) {
		ot := newOrphanedRoomTest(t)
		ot.hosted = []service.HostedRoom{&fakeHostedRoom{
			room:   &livekit.Room{Sid: "RM_closed", Name: "closed", CreationTime: longAgo.Unix(), EmptyTimeout: 300},
			closed: true,
		}}

		ot.sweeper(0).Sweep(ot.ctx)

		ot.requireNoRoom("closed")
	})

	t.Run("does nothing while its own node is stale", func(t *testing.T) {
		ot := newOrphanedRoomTest(t)
		ot.registerNode(sweepingNode, longAgo)
		ot.createRoom("orphaned", deadNode, longAgo, 300)

		ot.sweeper(0).Sweep(ot.ctx)

		ot.requireRoom("orphaned", deadNode)
	})

	t.Run("deletes a stored room without routing", func(t *testing.T) {
		ot := newOrphanedRoomTest(t)
		ot.createRoom("unrouted", deadNode, longAgo, 300)
		require.NoError(t, ot.router.ClearRoomState(ot.ctx, "unrouted"))

		ot.sweeper(0).Sweep(ot.ctx)

		ot.requireNoRoom("unrouted")
	})

	t.Run("retries a room whose deletion failed", func(t *testing.T) {
		ot := newOrphanedRoomTest(t)
		ot.createRoom("orphaned", deadNode, longAgo, 300)
		store := &failingDeleteStore{RedisStore: ot.store, failures: 1}
		sweeper := service.NewOrphanedRoomSweeper(ot.router, store, sweepingNode, ot.hostedRooms, 0)

		sweeper.Sweep(ot.ctx)
		exists, err := ot.store.RoomExists(ot.ctx, "orphaned")
		require.NoError(t, err)
		require.True(t, exists)

		sweeper.Sweep(ot.ctx)
		ot.requireNoRoom("orphaned")
	})

	t.Run("clears routing left without a room", func(t *testing.T) {
		ot := newOrphanedRoomTest(t)
		require.NoError(t, ot.router.SetNodeForRoom(ot.ctx, "routing-only", deadNode))

		ot.sweeper(0).Sweep(ot.ctx)

		ot.requireNoRoom("routing-only")
	})
}

// reassigningRouter simulates a join moving the room to a live node right after the sweeper observed it
type reassigningRouter struct {
	*routing.RedisRouter
	roomName livekit.RoomName
	nodeID   livekit.NodeID
}

func (r *reassigningRouter) ListRoomNodes(ctx context.Context) (map[livekit.RoomName]livekit.NodeID, error) {
	roomNodes, err := r.RedisRouter.ListRoomNodes(ctx)
	if err != nil {
		return nil, err
	}
	return roomNodes, r.SetNodeForRoom(ctx, r.roomName, r.nodeID)
}

type failingDeleteStore struct {
	*service.RedisStore
	failures int
}

func (s *failingDeleteStore) DeleteRoom(ctx context.Context, roomName livekit.RoomName) error {
	if s.failures > 0 {
		s.failures--
		return errors.New("delete failed")
	}
	return s.RedisStore.DeleteRoom(ctx, roomName)
}

// recoveringRouter simulates a node heartbeating right after the sweeper observed it unavailable
type recoveringRouter struct {
	*routing.RedisRouter
	recover func()
}

func (r *recoveringRouter) ListRoomNodes(ctx context.Context) (map[livekit.RoomName]livekit.NodeID, error) {
	r.recover()
	return r.RedisRouter.ListRoomNodes(ctx)
}

type fakeHostedRoom struct {
	room         *livekit.Room
	participants []*livekit.ParticipantInfo
	store        participantStore
	closed       bool
}

type participantStore interface {
	StoreParticipant(ctx context.Context, roomName livekit.RoomName, participant *livekit.ParticipantInfo) error
}

func (r *fakeHostedRoom) Name() livekit.RoomName          { return livekit.RoomName(r.room.Name) }
func (r *fakeHostedRoom) IsClosed() bool                  { return r.closed }
func (r *fakeHostedRoom) ToProto() *livekit.Room          { return r.room }
func (r *fakeHostedRoom) Internal() *livekit.RoomInternal { return nil }
func (r *fakeHostedRoom) ParticipantCount() int           { return len(r.participants) }

func (r *fakeHostedRoom) StoreParticipants(ctx context.Context) error {
	for _, p := range r.participants {
		if err := r.store.StoreParticipant(ctx, r.Name(), p); err != nil {
			return err
		}
	}
	return nil
}

type failingStoreParticipantStore struct {
	*service.RedisStore
	failAfter int
	failures  int
}

func (s *failingStoreParticipantStore) StoreParticipant(ctx context.Context, roomName livekit.RoomName, participant *livekit.ParticipantInfo) error {
	if s.failAfter > 0 {
		s.failAfter--
	} else if s.failures > 0 {
		s.failures--
		return errors.New("store failed")
	}
	return s.RedisStore.StoreParticipant(ctx, roomName, participant)
}

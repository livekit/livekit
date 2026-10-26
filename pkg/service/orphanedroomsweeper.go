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
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/routing/selector"
)

const (
	OrphanedRoomSweepInterval = 15 * time.Second
	OrphanedRoomGracePeriod   = time.Minute
)

// OrphanedRoomRouter is implemented by routers that share room assignments across nodes
type OrphanedRoomRouter interface {
	ListNodes() ([]*livekit.Node, error)
	ListRoomNodes(ctx context.Context) (map[livekit.RoomName]livekit.NodeID, error)
	ClearRoomStateForNode(ctx context.Context, roomName livekit.RoomName, nodeID livekit.NodeID) (bool, error)
}

// OrphanedRoomStore is implemented by stores shared across nodes
type OrphanedRoomStore interface {
	LockRoom(ctx context.Context, roomName livekit.RoomName, duration time.Duration) (string, error)
	UnlockRoom(ctx context.Context, roomName livekit.RoomName, uid string) error
	LoadRoom(ctx context.Context, roomName livekit.RoomName, includeInternal bool) (*livekit.Room, *livekit.RoomInternal, error)
	ListRoomNames(ctx context.Context) ([]livekit.RoomName, error)
	DeleteRoom(ctx context.Context, roomName livekit.RoomName) error
}

// orphanedRoom is a room assigned to a node that is not available, or stored without assignment
type orphanedRoom struct {
	nodeID livekit.NodeID
	since  time.Time
}

// OrphanedRoomSweeper removes rooms assigned to a node that has left the cluster without closing them,
// e.g. a node killed before it could shut down gracefully. Only the hosting node closes a room, so
// without this such rooms are listed forever. Stored rooms without assignment, such as those left by a
// failed deletion, are removed the same way.
//
// A room is deleted only once it has been seen orphaned for the grace period and its own timeouts have
// passed, i.e. when its node would have closed it too. A join reassigns an orphaned room to a live node,
// which takes it out of the sweeper's hands.
type OrphanedRoomSweeper struct {
	router      OrphanedRoomRouter
	roomStore   OrphanedRoomStore
	nodeID      livekit.NodeID
	gracePeriod time.Duration

	orphaned map[livekit.RoomName]orphanedRoom
}

func NewOrphanedRoomSweeper(router OrphanedRoomRouter, roomStore OrphanedRoomStore, nodeID livekit.NodeID, gracePeriod time.Duration) *OrphanedRoomSweeper {
	return &OrphanedRoomSweeper{
		router:      router,
		roomStore:   roomStore,
		nodeID:      nodeID,
		gracePeriod: gracePeriod,
		orphaned:    make(map[livekit.RoomName]orphanedRoom),
	}
}

// Sweep is not safe for concurrent use
func (s *OrphanedRoomSweeper) Sweep(ctx context.Context) {
	nodes, err := s.router.ListNodes()
	if err != nil {
		logger.Warnw("could not list nodes for orphaned rooms", err)
		return
	}
	liveNodes := make(map[livekit.NodeID]bool, len(nodes))
	for _, n := range nodes {
		if selector.IsAvailable(n) {
			liveNodes[livekit.NodeID(n.Id)] = true
		}
	}
	// when this node's own stats are stale, the message bus is lagging and peers' stale stats
	// do not mean they are gone. start observing again once it recovers.
	if !liveNodes[s.nodeID] {
		clear(s.orphaned)
		return
	}

	roomNodes, err := s.router.ListRoomNodes(ctx)
	if err != nil {
		logger.Warnw("could not list room nodes for orphaned rooms", err)
		return
	}
	roomNames, err := s.roomStore.ListRoomNames(ctx)
	if err != nil {
		logger.Warnw("could not list rooms for orphaned rooms", err)
		return
	}
	for _, roomName := range roomNames {
		if _, ok := roomNodes[roomName]; !ok {
			roomNodes[roomName] = ""
		}
	}

	now := time.Now()
	orphaned := make(map[livekit.RoomName]orphanedRoom)
	for roomName, nodeID := range roomNodes {
		if liveNodes[nodeID] {
			continue
		}
		o, ok := s.orphaned[roomName]
		if !ok || o.nodeID != nodeID {
			o = orphanedRoom{nodeID: nodeID, since: now}
		}
		orphaned[roomName] = o
	}
	s.orphaned = orphaned

	for roomName, o := range orphaned {
		if now.Sub(o.since) < s.gracePeriod {
			continue
		}
		resolved, err := s.deleteIfOrphaned(ctx, roomName, o, now)
		if err != nil {
			logger.Warnw("could not delete orphaned room", err, "room", roomName, "nodeID", o.nodeID)
			continue
		}
		if resolved {
			delete(s.orphaned, roomName)
		}
	}
}

// deleteIfOrphaned returns whether the room is no longer orphaned, either deleted or reassigned
func (s *OrphanedRoomSweeper) deleteIfOrphaned(ctx context.Context, roomName livekit.RoomName, o orphanedRoom, now time.Time) (bool, error) {
	// a node creating or joining the room holds this lock while loading it
	token, err := s.roomStore.LockRoom(ctx, roomName, 5*time.Second)
	if err != nil {
		return false, err
	}
	defer func() {
		_ = s.roomStore.UnlockRoom(ctx, roomName, token)
	}()

	rm, _, err := s.roomStore.LoadRoom(ctx, roomName, false)
	if err != nil && !errors.Is(err, ErrRoomNotFound) {
		return false, err
	}
	if rm != nil {
		// keep the room as long as its node would have, waiting for a first join or for participants to return
		if now.Unix()-rm.CreationTime < int64(rm.EmptyTimeout) || now.Sub(o.since) < time.Duration(rm.DepartureTimeout)*time.Second {
			return false, nil
		}
	}

	cleared, err := s.router.ClearRoomStateForNode(ctx, roomName, o.nodeID)
	if err != nil {
		return false, err
	}
	if !cleared || rm == nil {
		// reassigned to another node since it was observed, or only its routing was left
		return true, nil
	}
	if err = s.roomStore.DeleteRoom(ctx, roomName); err != nil {
		return false, err
	}
	logger.Infow("deleted orphaned room", "room", roomName, "roomID", rm.Sid, "nodeID", o.nodeID, "orphanedFor", now.Sub(o.since))
	return true, nil
}

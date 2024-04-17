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

package service

import (
	"context"
	"errors"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/routing/selector"
)

type StandardRoomAllocator struct {
	config    *config.Config
	router    routing.Router
	selector  selector.NodeSelector
	roomStore ObjectStore
}

func NewRoomAllocator(conf *config.Config, router routing.Router, rs ObjectStore) (RoomAllocator, error) {
	ns, err := selector.CreateNodeSelector(conf)
	if err != nil {
		return nil, err
	}

	return &StandardRoomAllocator{
		config:    conf,
		router:    router,
		selector:  ns,
		roomStore: rs,
	}, nil
}

// CreateRoom creates a new room from a request and allocates it to a node to handle
// it'll also monitor its state, and cleans it up when appropriate
func (r *StandardRoomAllocator) CreateRoom(ctx context.Context, req *livekit.CreateRoomRequest) (*livekit.Room, bool, error) {
	token, err := r.roomStore.LockRoom(ctx, livekit.RoomName(req.Name), 5*time.Second)
	if err != nil {
		return nil, false, err
	}
	defer func() {
		_ = r.roomStore.UnlockRoom(ctx, livekit.RoomName(req.Name), token)
	}()

	// find existing room and update it
	var created bool
	rm, internal, err := r.roomStore.LoadRoom(ctx, livekit.RoomName(req.Name), true)
	if errors.Is(err, ErrRoomNotFound) {
		created = true
		rm = &livekit.Room{
			Sid:          utils.NewGuid(utils.RoomPrefix),
			Name:         req.Name,
			CreationTime: time.Now().Unix(),
			TurnPassword: utils.RandomSecret(),
		}
		internal = &livekit.RoomInternal{}
		applyDefaultRoomConfig(rm, internal, &r.config.Room)
	} else if err != nil {
		return nil, false, err
	}

	if req.EmptyTimeout > 0 {
		rm.EmptyTimeout = req.EmptyTimeout
	}
	if req.DepartureTimeout > 0 {
		rm.DepartureTimeout = req.DepartureTimeout
	}
	if req.MaxParticipants > 0 {
		rm.MaxParticipants = req.MaxParticipants
	}
	if req.Metadata != "" {
		rm.Metadata = req.Metadata
	}
	if req.Egress != nil {
		if req.Egress.Participant != nil {
			internal.ParticipantEgress = req.Egress.Participant
		}
		if req.Egress.Tracks != nil {
			internal.TrackEgress = req.Egress.Tracks
		}
	}
	if req.MinPlayoutDelay > 0 || req.MaxPlayoutDelay > 0 {
		internal.PlayoutDelay = &livekit.PlayoutDelay{
			Enabled: true,
			Min:     req.MinPlayoutDelay,
			Max:     req.MaxPlayoutDelay,
		}
	}
	if req.SyncStreams {
		internal.SyncStreams = true
	}

	if err = r.roomStore.StoreRoom(ctx, rm, internal); err != nil {
		return nil, false, err
	}

	// check if room already assigned
	existing, err := r.router.GetNodeForRoom(ctx, livekit.RoomName(rm.Name))
	if !errors.Is(err, routing.ErrNotFound) && err != nil {
		return nil, false, err
	}

	// if already assigned and still available, keep it on that node
	if err == nil && selector.IsAvailable(existing) {
		// if node hosting the room is full, deny entry
		if selector.LimitsReached(r.config.Limit, existing.Stats) {
			return nil, false, routing.ErrNodeLimitReached
		}

		return rm, created, nil
	}

	// select a new node
	nodeID := livekit.NodeID(req.NodeId)
	if nodeID == "" {
		nodes, err := r.router.ListNodes()
		if err != nil {
			return nil, false, err
		}

		node, err := r.selector.SelectNode(nodes)
		if err != nil {
			return nil, false, err
		}

		nodeID = livekit.NodeID(node.Id)
	}

	logger.Infow("selected node for room", "room", rm.Name, "roomID", rm.Sid, "selectedNodeID", nodeID)
	err = r.router.SetNodeForRoom(ctx, livekit.RoomName(rm.Name), nodeID)
	if err != nil {
		return nil, false, err
	}

	return rm, true, nil
}

func (r *StandardRoomAllocator) ValidateCreateRoom(ctx context.Context, roomName livekit.RoomName) error {
	// when auto create is disabled, we'll check to ensure it's already created
	if !r.config.Room.AutoCreate {
		_, _, err := r.roomStore.LoadRoom(ctx, roomName, false)
		if err != nil {
			return err
		}
	}
	return nil
}

func applyDefaultRoomConfig(room *livekit.Room, internal *livekit.RoomInternal, conf *config.RoomConfig) {
	room.EmptyTimeout = conf.EmptyTimeout
	room.DepartureTimeout = conf.DepartureTimeout
	room.MaxParticipants = conf.MaxParticipants
	for _, codec := range conf.EnabledCodecs {
		room.EnabledCodecs = append(room.EnabledCodecs, &livekit.Codec{
			Mime:     codec.Mime,
			FmtpLine: codec.FmtpLine,
		})
	}
	internal.PlayoutDelay = &livekit.PlayoutDelay{
		Enabled: conf.PlayoutDelay.Enabled,
		Min:     uint32(conf.PlayoutDelay.Min),
		Max:     uint32(conf.PlayoutDelay.Max),
	}
	internal.SyncStreams = conf.SyncStreams
}

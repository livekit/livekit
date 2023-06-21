package service

import (
	"context"
	"time"

	lkutils "github.com/livekit/livekit-server/pkg/utils"
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
func (r *StandardRoomAllocator) CreateRoom(ctx context.Context, req *livekit.CreateRoomRequest, apiKey livekit.ApiKey) (*livekit.Room, error) {
	roomKey := lkutils.RoomKey(livekit.RoomName(req.Name), apiKey)

	token, err := r.roomStore.LockRoom(ctx, roomKey, 5*time.Second)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = r.roomStore.UnlockRoom(ctx, roomKey, token)
	}()

	// find existing room and update it
	rm, internal, _, err := r.roomStore.LoadRoom(ctx, roomKey, true)
	if err == ErrRoomNotFound {
		rm = &livekit.Room{
			Sid:          utils.NewGuid(utils.RoomPrefix),
			Name:         req.Name,
			Key:          string(roomKey),
			CreationTime: time.Now().Unix(),
			TurnPassword: utils.RandomSecret(),
		}
		applyDefaultRoomConfig(rm, &r.config.Room)
	} else if err != nil {
		return nil, err
	}

	if req.EmptyTimeout > 0 {
		rm.EmptyTimeout = req.EmptyTimeout
	}
	if req.MaxParticipants > 0 {
		rm.MaxParticipants = req.MaxParticipants
	}
	if req.Metadata != "" {
		rm.Metadata = req.Metadata
	}
	if req.Egress != nil && req.Egress.Tracks != nil {
		internal = &livekit.RoomInternal{TrackEgress: req.Egress.Tracks}
	}

	if err = r.roomStore.StoreRoom(ctx, rm, roomKey, internal); err != nil {
		return nil, err
	}

	// check if room already assigned
	existing, err := r.router.GetNodeForRoom(ctx, roomKey)
	if err != routing.ErrNotFound && err != nil {
		return nil, err
	}

	// if already assigned and still available, keep it on that node
	if err == nil && selector.IsAvailable(existing) {
		// if node hosting the room is full, deny entry
		if selector.LimitsReached(r.config.Limit, existing.Stats) {
			return nil, routing.ErrNodeLimitReached
		}

		return rm, nil
	}

	// select a new node
	nodeID := livekit.NodeID(req.NodeId)
	if nodeID == "" {
		nodes, err := r.router.ListNodes()
		if err != nil {
			return nil, err
		}

		node, err := r.selector.SelectNode(nodes)
		if err != nil {
			return nil, err
		}

		nodeID = livekit.NodeID(node.Id)
	}

	logger.Infow("selected node for room", "room", rm.Name, "roomID", rm.Sid, "selectedNodeID", nodeID)
	err = r.router.SetNodeForRoom(ctx, roomKey, nodeID)
	if err != nil {
		return nil, err
	}

	return rm, nil
}

func (r *StandardRoomAllocator) ValidateCreateRoom(ctx context.Context, roomKey livekit.RoomKey) error {
	// when auto create is disabled, we'll check to ensure it's already created
	if !r.config.Room.AutoCreate {
		_, _, _, err := r.roomStore.LoadRoom(ctx, roomKey, false)
		if err != nil {
			return err
		}
	}
	return nil
}

func applyDefaultRoomConfig(room *livekit.Room, conf *config.RoomConfig) {
	room.EmptyTimeout = conf.EmptyTimeout
	room.MaxParticipants = conf.MaxParticipants
	for _, codec := range conf.EnabledCodecs {
		room.EnabledCodecs = append(room.EnabledCodecs, &livekit.Codec{
			Mime:     codec.Mime,
			FmtpLine: codec.FmtpLine,
		})
	}
}

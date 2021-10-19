package service

import (
	"context"
	"time"

	livekit "github.com/livekit/protocol/proto"
	"github.com/livekit/protocol/utils"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
)

type RoomAllocator struct {
	config    *config.Config
	router    routing.Router
	roomStore RoomStore
}

func NewRoomAllocator(conf *config.Config, router routing.Router, rs RoomStore) *RoomAllocator {
	return &RoomAllocator{
		config:    conf,
		router:    router,
		roomStore: rs,
	}
}

// CreateRoom creates a new room from a request and allocates it to a node to handle
// it'll also monitor its state, and cleans it up when appropriate
func (r *RoomAllocator) CreateRoom(ctx context.Context, req *livekit.CreateRoomRequest) (*livekit.Room, error) {
	token, err := r.roomStore.LockRoom(ctx, req.Name, 5*time.Second)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = r.roomStore.UnlockRoom(ctx, req.Name, token)
	}()

	// find existing room and update it
	rm, err := r.roomStore.LoadRoom(ctx, req.Name)
	if err == ErrRoomNotFound {
		rm = &livekit.Room{
			Sid:          utils.NewGuid(utils.RoomPrefix),
			Name:         req.Name,
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
	if err := r.roomStore.StoreRoom(ctx, rm); err != nil {
		return nil, err
	}

	err = r.router.SelectNodeForRoom(ctx, rm)
	if err != nil {
		return nil, err
	}

	return rm, nil
}

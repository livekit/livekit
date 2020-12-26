package service

import (
	"context"

	"github.com/twitchtv/twirp"

	"github.com/livekit/livekit-server/pkg/node"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/proto/livekit"
)

// A rooms service that supports a single node
type SimpleRoomService struct {
	localNode *node.Node

	manager *rtc.RoomManager
}

func NewSimpleRoomService(manager *rtc.RoomManager, localNode *node.Node) (svc *SimpleRoomService, err error) {
	svc = &SimpleRoomService{
		localNode: localNode,
		manager:   manager,
	}

	return
}

func (s *SimpleRoomService) CreateRoom(ctx context.Context, req *livekit.CreateRoomRequest) (res *livekit.RoomInfo, err error) {
	if err = EnsureCreatePermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}
	room, err := s.manager.CreateRoom(req)
	if err != nil {
		return
	}

	res = room.ToRoomInfo(&s.localNode.Node)
	return
}

func (s *SimpleRoomService) GetRoom(ctx context.Context, req *livekit.GetRoomRequest) (res *livekit.RoomInfo, err error) {
	onlyName, err := EnsureJoinPermission(ctx)
	if err != nil {
		return nil, twirpAuthError(err)
	}

	room, err := s.manager.GetRoomWithConstraint(req.Room, onlyName)
	if err != nil {
		// TODO: translate error codes to twirp
		return
	}

	res = room.ToRoomInfo(&s.localNode.Node)
	return
}

func (s *SimpleRoomService) DeleteRoom(ctx context.Context, req *livekit.DeleteRoomRequest) (res *livekit.DeleteRoomResponse, err error) {
	if err = EnsureCreatePermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}
	err = s.manager.DeleteRoom(req.Room)
	if err != nil {
		err = twirp.WrapError(twirp.InternalError("could not delete room"), err)
		return
	}
	res = &livekit.DeleteRoomResponse{}
	return
}

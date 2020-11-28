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
	room, err := s.manager.CreateRoom(req)
	if err != nil {
		return
	}

	res = room.ToRoomInfo(&s.localNode.Node)
	return
}

func (s *SimpleRoomService) GetRoom(ctx context.Context, req *livekit.GetRoomRequest) (res *livekit.RoomInfo, err error) {
	room := s.manager.GetRoom(req.RoomId)
	if room == nil {
		err = twirp.NewError(twirp.NotFound, "room does not exist")
		return
	}

	res = room.ToRoomInfo(&s.localNode.Node)
	return
}

func (s *SimpleRoomService) DeleteRoom(ctx context.Context, req *livekit.DeleteRoomRequest) (res *livekit.DeleteRoomResponse, err error) {
	err = s.manager.DeleteRoom(req.RoomId)
	if err != nil {
		err = twirp.WrapError(twirp.InternalError("could not delete room"), err)
		return
	}
	res = &livekit.DeleteRoomResponse{}
	return
}

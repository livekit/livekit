package service

import (
	"context"
	"sync"

	"github.com/twitchtv/twirp"

	"github.com/livekit/livekit-server/pkg/node"
	"github.com/livekit/livekit-server/proto"
)

// A room service that supports a single node
type SimpleRoomService struct {
	localNode *node.Node
	rooms     map[string]bool
	roomLock  sync.Mutex
}

func NewSimpleRoomService(localNode *node.Node) (svc *SimpleRoomService, err error) {
	svc = &SimpleRoomService{
		localNode: localNode,
		rooms:     make(map[string]bool),
		roomLock:  sync.Mutex{},
	}

	return
}

func (s *SimpleRoomService) CreateRoom(ctx context.Context, req *proto.CreateRoomRequest) (res *proto.CreateRoomResponse, err error) {
	s.roomLock.Lock()
	defer s.roomLock.Unlock()

	if s.rooms[req.Room] {
		err = twirp.NewError(twirp.AlreadyExists, "room already exists")
		return
	}

	s.rooms[req.Room] = true

	res = &proto.CreateRoomResponse{
		Room:        req.Room,
		NodeIp:      s.localNode.Ip,
		NodeRtcPort: s.localNode.RtcPort,
	}
	return
}

func (s *SimpleRoomService) JoinRoom(ctx context.Context, req *proto.JoinRoomRequest) (res *proto.JoinRoomResponse, err error) {
	if !s.rooms[req.Room] {
		err = twirp.NewError(twirp.AlreadyExists, "the room does not exist")
		return
	}

	res = &proto.JoinRoomResponse{
		NodeIp:      s.localNode.Ip,
		NodeRtcPort: s.localNode.RtcPort,
	}
	return
}

func (s *SimpleRoomService) DeleteRoom(ctx context.Context, req *proto.DeleteRoomRequest) (res *proto.DeleteRoomResponse, err error) {
	delete(s.rooms, req.Room)
	res = &proto.DeleteRoomResponse{}
	return
}

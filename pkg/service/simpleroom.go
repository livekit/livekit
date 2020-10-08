package service

import (
	"context"
	"sync"

	"github.com/twitchtv/twirp"

	"github.com/livekit/livekit-server/pkg/node"
	"github.com/livekit/livekit-server/pkg/rooms"
	"github.com/livekit/livekit-server/proto/livekit"
)

// A rooms service that supports a single node
type SimpleRoomService struct {
	localNode *node.Node
	rooms     map[string]*livekit.Room
	roomLock  sync.Mutex
}

func NewSimpleRoomService(localNode *node.Node) (svc *SimpleRoomService, err error) {
	svc = &SimpleRoomService{
		localNode: localNode,
		rooms:     make(map[string]*livekit.Room),
		roomLock:  sync.Mutex{},
	}

	return
}

func (s *SimpleRoomService) CreateRoom(ctx context.Context, req *livekit.CreateRoomRequest) (res *livekit.RoomInfo, err error) {
	s.roomLock.Lock()
	defer s.roomLock.Unlock()

	if s.rooms[req.RoomId] != nil {
		err = twirp.NewError(twirp.AlreadyExists, "rooms already exists")
		return
	}

	room, err := rooms.NewRoomForRequest(req)
	if err != nil {
		return
	}
	s.rooms[req.RoomId] = room

	res = rooms.ToRoomInfo(&s.localNode.Node, room)
	return
}

func (s *SimpleRoomService) GetRoom(ctx context.Context, req *livekit.GetRoomRequest) (res *livekit.RoomInfo, err error) {
	room := s.rooms[req.RoomId]
	if room == nil {
		err = twirp.NewError(twirp.NotFound, "the rooms does not exist")
		return
	}

	res = rooms.ToRoomInfo(&s.localNode.Node, room)
	return
}

func (s *SimpleRoomService) DeleteRoom(ctx context.Context, req *livekit.DeleteRoomRequest) (res *livekit.DeleteRoomResponse, err error) {
	delete(s.rooms, req.RoomId)
	res = &livekit.DeleteRoomResponse{}
	return
}

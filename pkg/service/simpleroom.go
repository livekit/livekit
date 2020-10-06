package service

import (
	"context"

	"github.com/livekit/livekit-server/pkg/node"
	"github.com/livekit/livekit-server/proto"
)

// A room service that supports a single node
type SimpleRoomService struct {
	localNode *node.Node
	rooms     map[string]bool
}

func NewRoomService(localNode *node.Node) (svc *SimpleRoomService, err error) {
	svc = &SimpleRoomService{
		localNode: localNode,
		rooms:     make(map[string]bool),
	}

	return
}

func (s *SimpleRoomService) CreateRoom(ctx context.Context, req *proto.CreateRoomRequest) (res *proto.CreateRoomResponse, err error) {

	return
}

func (s *SimpleRoomService) JoinRoom(ctx context.Context, req *proto.JoinRoomRequest) (res *proto.JoinRoomResponse, err error) {
	return
}

func (s *SimpleRoomService) DeleteRoom(ctx context.Context, req *proto.DeleteRoomRequest) (res *proto.DeleteRoomResponse, err error) {
	return
}

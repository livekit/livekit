package service

import (
	"context"
	"time"

	"github.com/twitchtv/twirp"

	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/livekit-server/proto/livekit"
)

// A rooms service that supports a single node
type RoomService struct {
	roomProvider RoomStore
}

func NewRoomService(rp RoomStore) (svc *RoomService, err error) {
	svc = &RoomService{
		roomProvider: rp,
	}

	return
}

func (s *RoomService) CreateRoom(ctx context.Context, req *livekit.CreateRoomRequest) (rm *livekit.Room, err error) {
	if err = EnsureCreatePermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}

	rm = &livekit.Room{
		Sid:             utils.NewGuid(utils.RoomPrefix),
		Name:            req.Name,
		EmptyTimeout:    req.EmptyTimeout,
		MaxParticipants: req.MaxParticipants,
		CreationTime:    time.Now().Unix(),
	}
	err = s.roomProvider.CreateRoom(rm)
	if err != nil {
		return
	}

	return
}

func (s *RoomService) ListRooms(ctx context.Context, req *livekit.ListRoomsRequest) (res *livekit.ListRoomsResponse, err error) {
	err = EnsureListPermission(ctx)
	if err != nil {
		return nil, twirpAuthError(err)
	}

	rooms, err := s.roomProvider.ListRooms()
	if err != nil {
		// TODO: translate error codes to twirp
		return
	}

	res = &livekit.ListRoomsResponse{
		Rooms: rooms,
	}
	return
}

func (s *RoomService) DeleteRoom(ctx context.Context, req *livekit.DeleteRoomRequest) (res *livekit.DeleteRoomResponse, err error) {
	if err = EnsureCreatePermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}
	err = s.roomProvider.DeleteRoom(req.Room)
	if err != nil {
		err = twirp.WrapError(twirp.InternalError("could not delete room"), err)
		return
	}
	res = &livekit.DeleteRoomResponse{}
	return
}

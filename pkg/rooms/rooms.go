package rooms

import (
	"time"

	"github.com/google/uuid"

	"github.com/livekit/livekit-server/proto/livekit"
)

func NewRoomForRequest(req *livekit.CreateRoomRequest) (*livekit.Room, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	return &livekit.Room{
		RoomId:          req.RoomId,
		EmptyTimeout:    req.EmptyTimeout,
		MaxParticipants: req.MaxParticipants,
		CreationTime:    time.Now().Unix(),
		Token:           id.String(),
	}, nil
}

func ToRoomInfo(node *livekit.Node, room *livekit.Room) *livekit.RoomInfo {
	return &livekit.RoomInfo{
		RoomId:       room.RoomId,
		NodeIp:       node.Ip,
		NodeRtcPort:  node.RtcPort,
		CreationTime: room.CreationTime,
		Token:        room.Token,
	}
}

package rtc

import (
	"time"

	"github.com/google/uuid"

	"github.com/livekit/livekit-server/proto/livekit"
)

type Room struct {
	livekit.Room

	// Client ID => list of tracks they are publishing
	tracks map[string][]PeerTrack
}

func NewRoomForRequest(req *livekit.CreateRoomRequest) (*Room, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	return &Room{
		Room: livekit.Room{
			RoomId:          req.RoomId,
			EmptyTimeout:    req.EmptyTimeout,
			MaxParticipants: req.MaxParticipants,
			CreationTime:    time.Now().Unix(),
			Token:           id.String(),
		},
	}, nil
}

func (r *Room) ToRoomInfo(node *livekit.Node) *livekit.RoomInfo {
	return &livekit.RoomInfo{
		RoomId:       r.RoomId,
		NodeIp:       node.Ip,
		NodeRtcPort:  node.RtcPort,
		CreationTime: r.CreationTime,
		Token:        r.Token,
	}
}

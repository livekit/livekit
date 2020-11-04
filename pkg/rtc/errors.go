package rtc

import "errors"

var (
	ErrRoomIdMissing          = errors.New("roomId is not set")
	ErrPeerExists             = errors.New("peer already exists")
	ErrPeerNotConnected       = errors.New("peer has not been connected")
	ErrUnsupportedPayloadType = errors.New("peer does not support payload type")
)

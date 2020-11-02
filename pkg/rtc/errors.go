package rtc

import "errors"

var (
	ErrPeerExists             = errors.New("peer already exists")
	ErrPeerNotConnected       = errors.New("peer has not been connected")
	ErrUnsupportedPayloadType = errors.New("peer does not support payload type")
)

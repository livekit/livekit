package service

import (
	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/proto/livekit"
)

func ToProtoSessionDescription(sd *webrtc.SessionDescription) *livekit.SessionDescription {
	return &livekit.SessionDescription{
		Type: sd.Type.String(),
		Sdp:  sd.SDP,
	}
}

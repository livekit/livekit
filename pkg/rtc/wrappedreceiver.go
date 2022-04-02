package rtc

import (
	"github.com/pion/webrtc/v3"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/sfu"
)

// wrapper around WebRTC receiver, overriding its ID

type WrappedReceiver struct {
	sfu.TrackReceiver
	mainReceiver, altReceiver sfu.TrackReceiver
	trackID                   livekit.TrackID
	streamId                  string
}

func NewWrappedReceiver(mainReceiver, alterReceiver sfu.TrackReceiver, trackID livekit.TrackID, streamId string) *WrappedReceiver {
	return &WrappedReceiver{
		TrackReceiver: mainReceiver,
		mainReceiver:  mainReceiver,
		altReceiver:   alterReceiver,
		trackID:       trackID,
		streamId:      streamId,
	}
}

func (r *WrappedReceiver) TrackID() livekit.TrackID {
	return r.trackID
}

func (r *WrappedReceiver) StreamID() string {
	return r.streamId
}

func (r *WrappedReceiver) DetermineReceiver(codec webrtc.RTPCodecCapability) {
	if codec.MimeType == r.mainReceiver.Codec().MimeType {
		r.TrackReceiver = r.mainReceiver
	} else if codec.MimeType == r.altReceiver.Codec().MimeType {
		r.TrackReceiver = r.altReceiver
	}
}

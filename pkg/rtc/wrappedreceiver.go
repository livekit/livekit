package rtc

import (
	"github.com/livekit/livekit-server/pkg/sfu"
)

// wrapper around WebRTC receiver, overriding its ID

type WrappedReceiver struct {
	sfu.Receiver
	trackId  string
	streamId string
}

func NewWrappedReceiver(receiver sfu.Receiver, trackId string, streamId string) WrappedReceiver {
	return WrappedReceiver{
		Receiver: receiver,
		trackId:  trackId,
		streamId: streamId,
	}
}

func (r WrappedReceiver) TrackID() string {
	return r.trackId
}

func (r WrappedReceiver) StreamID() string {
	return r.streamId
}

package rtc

import (
	"github.com/pion/ion-sfu/pkg/sfu"
)

// wrapper around WebRTC receiver, overriding its ID

type WrappedReceiver struct {
	sfu.Receiver
	trackId string
}

func NewWrappedReceiver(receiver sfu.Receiver, trackId string) WrappedReceiver {
	return WrappedReceiver{
		Receiver: receiver,
		trackId:  trackId,
	}
}

func (r WrappedReceiver) TrackID() string {
	return r.trackId
}

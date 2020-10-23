package rtc

import (
	sfu "github.com/pion/ion-sfu/pkg"
	"github.com/pion/webrtc/v3"
)

// A receiver is responsible for pulling from a track
type Receiver struct {
	track       *webrtc.Track
	rtpReceiver *webrtc.RTPReceiver
	buffer      *sfu.Buffer
}

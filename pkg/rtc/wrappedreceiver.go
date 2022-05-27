package rtc

import (
	"github.com/pion/webrtc/v3"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/sfu"
)

// wrapper around WebRTC receiver, overriding its ID

type WrappedReceiver struct {
	sfu.TrackReceiver
	receivers []sfu.TrackReceiver
	trackID   livekit.TrackID
	streamId  string
}

func NewWrappedReceiver(receivers []*simulcastReceiver, trackID livekit.TrackID, streamId string) *WrappedReceiver {
	sfuReceivers := make([]sfu.TrackReceiver, 0, len(receivers))
	for _, r := range receivers {
		sfuReceivers = append(sfuReceivers, r.TrackReceiver)
	}

	return &WrappedReceiver{
		receivers: sfuReceivers,
		trackID:   trackID,
		streamId:  streamId,
	}
}

func (r *WrappedReceiver) TrackID() livekit.TrackID {
	return r.trackID
}

func (r *WrappedReceiver) StreamID() string {
	return r.streamId
}

func (r *WrappedReceiver) DetermineReceiver(codec webrtc.RTPCodecCapability) {
	for _, receiver := range r.receivers {
		if receiver.Codec().MimeType == codec.MimeType {
			r.TrackReceiver = receiver
			break
		}
	}
}

func (r *WrappedReceiver) Codecs() []webrtc.RTPCodecCapability {
	codecs := make([]webrtc.RTPCodecCapability, 0, len(r.receivers))
	for _, receiver := range r.receivers {
		codecs = append(codecs, receiver.Codec().RTPCodecCapability)
	}
	return codecs
}

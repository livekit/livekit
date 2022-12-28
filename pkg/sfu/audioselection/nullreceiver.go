package audioselection

import (
	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/protocol/livekit"
)

type NullReceiver struct {
	streamID string
	trackID  livekit.TrackID
}

func NewNullReceiver(streamID string, trackID livekit.TrackID) *NullReceiver {
	return &NullReceiver{
		streamID: streamID,
		trackID:  trackID,
	}
}

func (r *NullReceiver) TrackID() livekit.TrackID {
	return r.trackID
}
func (r *NullReceiver) StreamID() string {
	return r.streamID
}

func (r *NullReceiver) Codec() webrtc.RTPCodecParameters {
	return webrtc.RTPCodecParameters{}
}
func (r *NullReceiver) HeaderExtensions() []webrtc.RTPHeaderExtensionParameter {
	return nil
}
func (r *NullReceiver) ReadRTP(buf []byte, layer uint8, sn uint16) (int, error) {
	return 0, nil
}
func (r *NullReceiver) GetLayeredBitrate() sfu.Bitrates {
	return sfu.Bitrates{}
}
func (r *NullReceiver) GetAudioLevel() (float64, bool) {
	return 0, false
}
func (r *NullReceiver) SendPLI(layer int32, force bool) {
}
func (r *NullReceiver) SetUpTrackPaused(paused bool) {
}
func (r *NullReceiver) SetMaxExpectedSpatialLayer(layer int32) {
}
func (r *NullReceiver) AddDownTrack(track sfu.TrackSender) error {
	return nil
}
func (r *NullReceiver) DeleteDownTrack(participantID livekit.ParticipantID) {
}
func (r *NullReceiver) DebugInfo() map[string]interface{} {
	return nil
}
func (r *NullReceiver) GetLayerDimension(layer int32) (uint32, uint32) {
	return 0, 0
}
func (r *NullReceiver) TrackInfo() *livekit.TrackInfo {
	return &livekit.TrackInfo{}
}
func (r *NullReceiver) GetPrimaryReceiverForRed() sfu.TrackReceiver {
	return r
}
func (r *NullReceiver) GetRedReceiver() sfu.TrackReceiver {
	return r
}
func (r *NullReceiver) GetTemporalLayerFpsForSpatial(layer int32) []float32 {
	return nil
}

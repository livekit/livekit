package audioselection

import (
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/protocol/utils"
)

// implements types.DownTrack

/*
ID() string
CloseWithFlush(flush bool)
Resync()
Codec() webrtc.RTPCodecCapability
DebugInfo() map[string]interface{}
GetConnectionScore() float32
SetActivePaddingOnMuteUpTrack()
SetConnected()
SetMaxSpatialLayer(spatialLayer int32)
SetMaxTemporalLayer(temporalLayer int32)
Kind() webrtc.RTPCodecType
Mute(muted bool)
PubMute(pubMuted bool)
CreateSenderReport() *rtcp.SenderReport
CreateSourceDescriptionChunks() []rtcp.SourceDescriptionChunk
*/
type NullAudioDowntrack struct {
	id string
}

func NewNullAudioDowntrack() *NullAudioDowntrack {
	return &NullAudioDowntrack{id: utils.NewGuid("TRAN_")}
}

func (d *NullAudioDowntrack) ID() string {
	return d.id
}

func (d *NullAudioDowntrack) CloseWithFlush(flush bool) {
}

func (d *NullAudioDowntrack) Resync() {
}

func (d *NullAudioDowntrack) Codec() webrtc.RTPCodecCapability {
	return webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1"}
}

func (d *NullAudioDowntrack) DebugInfo() map[string]interface{} {
	return nil
}

func (d *NullAudioDowntrack) GetConnectionScore() float32 {
	return 5.0
}

func (d *NullAudioDowntrack) SetActivePaddingOnMuteUpTrack() {
}

func (d *NullAudioDowntrack) SetConnected() {
}

func (d *NullAudioDowntrack) SetMaxSpatialLayer(spatialLayer int32) {
}

func (d *NullAudioDowntrack) SetMaxTemporalLayer(temporalLayer int32) {
}

func (d *NullAudioDowntrack) Kind() webrtc.RTPCodecType {
	return webrtc.RTPCodecTypeAudio
}

func (d *NullAudioDowntrack) Mute(muted bool) {
}

func (d *NullAudioDowntrack) PubMute(pubMuted bool) {
}

func (d *NullAudioDowntrack) CreateSenderReport() *rtcp.SenderReport {
	return nil
}

func (d *NullAudioDowntrack) CreateSourceDescriptionChunks() []rtcp.SourceDescriptionChunk {
	return nil
}

package rtc

import (
	"errors"
	"sync"

	"github.com/pion/webrtc/v3"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/sfu"
)

// wrapper around WebRTC receiver, overriding its ID

type WrappedReceiver struct {
	sfu.TrackReceiver
	receivers       []sfu.TrackReceiver
	trackID         livekit.TrackID
	streamId        string
	codecs          []webrtc.RTPCodecParameters
	determinedCodec webrtc.RTPCodecCapability
}

func NewWrappedReceiver(receivers []*simulcastReceiver, trackID livekit.TrackID, streamId string, upstreamCodecs []webrtc.RTPCodecParameters) *WrappedReceiver {
	sfuReceivers := make([]sfu.TrackReceiver, 0, len(receivers))
	for _, r := range receivers {
		sfuReceivers = append(sfuReceivers, r.TrackReceiver)
	}

	return &WrappedReceiver{
		receivers: sfuReceivers,
		trackID:   trackID,
		streamId:  streamId,
		codecs:    upstreamCodecs,
	}
}

func (r *WrappedReceiver) TrackID() livekit.TrackID {
	return r.trackID
}

func (r *WrappedReceiver) StreamID() string {
	return r.streamId
}

func (r *WrappedReceiver) DetermineReceiver(codec webrtc.RTPCodecCapability) {
	r.determinedCodec = codec
	for _, receiver := range r.receivers {
		if receiver.Codec().MimeType == codec.MimeType {
			r.TrackReceiver = receiver
			break
		}
	}
}

func (r *WrappedReceiver) Codecs() []webrtc.RTPCodecParameters {
	codecs := make([]webrtc.RTPCodecParameters, len(r.receivers))
	copy(codecs, r.codecs)
	return codecs
}

type DumbReceiver struct {
	receiver         atomic.Value
	trackID          livekit.TrackID
	streamId         string
	codec            webrtc.RTPCodecParameters
	headerExtensions []webrtc.RTPHeaderExtensionParameter
	downtrackLock    sync.Mutex
	downtracks       map[livekit.ParticipantID]sfu.TrackSender
}

func NewDumbReceiver(trackID livekit.TrackID, streamId string, codec webrtc.RTPCodecParameters, headerExtensions []webrtc.RTPHeaderExtensionParameter) *DumbReceiver {
	return &DumbReceiver{
		trackID:          trackID,
		streamId:         streamId,
		codec:            codec,
		headerExtensions: headerExtensions,
		downtracks:       make(map[livekit.ParticipantID]sfu.TrackSender),
	}
}

func (d *DumbReceiver) Receiver() sfu.TrackReceiver {
	r, _ := d.receiver.Load().(sfu.TrackReceiver)
	return r
}

func (d *DumbReceiver) Upgrade(receiver sfu.TrackReceiver) {
	d.downtrackLock.Lock()
	defer d.downtrackLock.Unlock()
	d.receiver.CompareAndSwap(nil, receiver)
	for _, t := range d.downtracks {
		receiver.AddDownTrack(t)
	}
	d.downtracks = make(map[livekit.ParticipantID]sfu.TrackSender)
}

func (d *DumbReceiver) TrackID() livekit.TrackID {
	return d.trackID
}

func (d *DumbReceiver) StreamID() string {
	return d.streamId
}

func (d *DumbReceiver) Codec() webrtc.RTPCodecParameters {
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		return r.Codec()
	}
	return d.codec
}

func (d *DumbReceiver) HeaderExtensions() []webrtc.RTPHeaderExtensionParameter {
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		return r.HeaderExtensions()
	}
	return d.headerExtensions
}

func (d *DumbReceiver) ReadRTP(buf []byte, layer uint8, sn uint16) (int, error) {
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		return r.ReadRTP(buf, layer, sn)
	}
	return 0, errors.New("no receiver")
}

func (d *DumbReceiver) GetBitrateTemporalCumulative() sfu.Bitrates {
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		return r.GetBitrateTemporalCumulative()
	}
	return sfu.Bitrates{}
}

func (d *DumbReceiver) GetAudioLevel() (float64, bool) {
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		return r.GetAudioLevel()
	}
	return 0, false
}

func (d *DumbReceiver) SendPLI(layer int32, force bool) {
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		r.SendPLI(layer, force)
	}
}

func (d *DumbReceiver) SetUpTrackPaused(paused bool) {
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		r.SetUpTrackPaused(paused)
	}
}

func (d *DumbReceiver) SetMaxExpectedSpatialLayer(layer int32) {
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		r.SetMaxExpectedSpatialLayer(layer)
	}
}

func (d *DumbReceiver) AddDownTrack(track sfu.TrackSender) error {
	d.downtrackLock.Lock()
	defer d.downtrackLock.Unlock()
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		r.AddDownTrack(track)
	} else {
		d.downtracks[track.SubscriberID()] = track
	}
	return nil
}

func (d *DumbReceiver) DeleteDownTrack(participantID livekit.ParticipantID) {
	d.downtrackLock.Lock()
	defer d.downtrackLock.Unlock()
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		r.DeleteDownTrack(participantID)
	} else {
		delete(d.downtracks, participantID)
	}
}

func (d *DumbReceiver) DebugInfo() map[string]interface{} {
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		return r.DebugInfo()
	}
	return nil
}

func (d *DumbReceiver) GetLayerDimension(quality int32) (uint32, uint32) {
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		return r.GetLayerDimension(quality)
	}
	return 0, 0
}

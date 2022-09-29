package rtc

import (
	"errors"
	"strings"
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

	codecs := upstreamCodecs
	// if upstream is opus/red, then add opus to match clients that don't support red
	if len(codecs) == 1 && strings.EqualFold(codecs[0].MimeType, sfu.MimeTypeAudioRed) {
		codecs = append(codecs, webrtc.RTPCodecParameters{
			RTPCodecCapability: opusCodecCapability,
			PayloadType:        111,
		})
	}

	return &WrappedReceiver{
		receivers: sfuReceivers,
		trackID:   trackID,
		streamId:  streamId,
		codecs:    codecs,
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
		if c := receiver.Codec(); c.MimeType == codec.MimeType {
			r.TrackReceiver = receiver
			break
		} else if strings.EqualFold(c.MimeType, sfu.MimeTypeAudioRed) && strings.EqualFold(codec.MimeType, webrtc.MimeTypeOpus) {
			// audio opus/red can match opus only
			r.TrackReceiver = receiver.GetPrimaryReceiverForRed()
			break
		}
	}
}

func (r *WrappedReceiver) Codecs() []webrtc.RTPCodecParameters {
	codecs := make([]webrtc.RTPCodecParameters, len(r.codecs))
	copy(codecs, r.codecs)
	return codecs
}

type DummyReceiver struct {
	receiver         atomic.Value
	trackID          livekit.TrackID
	streamId         string
	codec            webrtc.RTPCodecParameters
	headerExtensions []webrtc.RTPHeaderExtensionParameter

	downtrackLock sync.Mutex
	downtracks    map[livekit.ParticipantID]sfu.TrackSender

	settingsLock          sync.Mutex
	maxExpectedLayerValid bool
	maxExpectedLayer      int32
	pausedValid           bool
	paused                bool
}

func NewDummyReceiver(trackID livekit.TrackID, streamId string, codec webrtc.RTPCodecParameters, headerExtensions []webrtc.RTPHeaderExtensionParameter) *DummyReceiver {
	return &DummyReceiver{
		trackID:          trackID,
		streamId:         streamId,
		codec:            codec,
		headerExtensions: headerExtensions,
		downtracks:       make(map[livekit.ParticipantID]sfu.TrackSender),
	}
}

func (d *DummyReceiver) Receiver() sfu.TrackReceiver {
	r, _ := d.receiver.Load().(sfu.TrackReceiver)
	return r
}

func (d *DummyReceiver) Upgrade(receiver sfu.TrackReceiver) {
	d.receiver.CompareAndSwap(nil, receiver)

	d.downtrackLock.Lock()
	for _, t := range d.downtracks {
		receiver.AddDownTrack(t)
	}
	d.downtracks = make(map[livekit.ParticipantID]sfu.TrackSender)
	d.downtrackLock.Unlock()

	d.settingsLock.Lock()
	if d.maxExpectedLayerValid {
		receiver.SetMaxExpectedSpatialLayer(d.maxExpectedLayer)
	}
	d.maxExpectedLayerValid = false

	if d.pausedValid {
		receiver.SetUpTrackPaused(d.paused)
	}
	d.pausedValid = false
	d.settingsLock.Unlock()
}

func (d *DummyReceiver) TrackID() livekit.TrackID {
	return d.trackID
}

func (d *DummyReceiver) StreamID() string {
	return d.streamId
}

func (d *DummyReceiver) Codec() webrtc.RTPCodecParameters {
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		return r.Codec()
	}
	return d.codec
}

func (d *DummyReceiver) HeaderExtensions() []webrtc.RTPHeaderExtensionParameter {
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		return r.HeaderExtensions()
	}
	return d.headerExtensions
}

func (d *DummyReceiver) ReadRTP(buf []byte, layer uint8, sn uint16) (int, error) {
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		return r.ReadRTP(buf, layer, sn)
	}
	return 0, errors.New("no receiver")
}

func (d *DummyReceiver) GetLayeredBitrate() sfu.Bitrates {
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		return r.GetLayeredBitrate()
	}
	return sfu.Bitrates{}
}

func (d *DummyReceiver) GetAudioLevel() (float64, bool) {
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		return r.GetAudioLevel()
	}
	return 0, false
}

func (d *DummyReceiver) SendPLI(layer int32, force bool) {
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		r.SendPLI(layer, force)
	}
}

func (d *DummyReceiver) SetUpTrackPaused(paused bool) {
	d.settingsLock.Lock()
	defer d.settingsLock.Unlock()
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		d.pausedValid = false
		r.SetUpTrackPaused(paused)
	} else {
		d.pausedValid = true
		d.paused = paused
	}
}

func (d *DummyReceiver) SetMaxExpectedSpatialLayer(layer int32) {
	d.settingsLock.Lock()
	defer d.settingsLock.Unlock()
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		d.maxExpectedLayerValid = false
		r.SetMaxExpectedSpatialLayer(layer)
	} else {
		d.maxExpectedLayerValid = true
		d.maxExpectedLayer = layer
	}
}

func (d *DummyReceiver) AddDownTrack(track sfu.TrackSender) error {
	d.downtrackLock.Lock()
	defer d.downtrackLock.Unlock()
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		r.AddDownTrack(track)
	} else {
		d.downtracks[track.SubscriberID()] = track
	}
	return nil
}

func (d *DummyReceiver) DeleteDownTrack(participantID livekit.ParticipantID) {
	d.downtrackLock.Lock()
	defer d.downtrackLock.Unlock()
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		r.DeleteDownTrack(participantID)
	} else {
		delete(d.downtracks, participantID)
	}
}

func (d *DummyReceiver) DebugInfo() map[string]interface{} {
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		return r.DebugInfo()
	}
	return nil
}

func (d *DummyReceiver) GetLayerDimension(quality int32) (uint32, uint32) {
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		return r.GetLayerDimension(quality)
	}
	return 0, 0
}

func (d *DummyReceiver) TrackInfo() *livekit.TrackInfo {
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		return r.TrackInfo()
	}
	return nil
}

func (d *DummyReceiver) GetPrimaryReceiverForRed() sfu.TrackReceiver {
	// DummyReceiver used for video, it should not have RED codec
	return d
}

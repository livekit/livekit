// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package rtc

import (
	"errors"
	"strings"
	"sync"

	"github.com/pion/webrtc/v3"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu"
)

// wrapper around WebRTC receiver, overriding its ID

type WrappedReceiverParams struct {
	Receivers      []*simulcastReceiver
	TrackID        livekit.TrackID
	StreamId       string
	UpstreamCodecs []webrtc.RTPCodecParameters
	Logger         logger.Logger
	DisableRed     bool
}

type WrappedReceiver struct {
	sfu.TrackReceiver
	params          WrappedReceiverParams
	receivers       []sfu.TrackReceiver
	codecs          []webrtc.RTPCodecParameters
	determinedCodec webrtc.RTPCodecCapability
}

func NewWrappedReceiver(params WrappedReceiverParams) *WrappedReceiver {
	sfuReceivers := make([]sfu.TrackReceiver, 0, len(params.Receivers))
	for _, r := range params.Receivers {
		sfuReceivers = append(sfuReceivers, r.TrackReceiver)
	}

	codecs := params.UpstreamCodecs
	if len(codecs) == 1 {
		if strings.EqualFold(codecs[0].MimeType, sfu.MimeTypeAudioRed) {
			// if upstream is opus/red, then add opus to match clients that don't support red
			codecs = append(codecs, webrtc.RTPCodecParameters{
				RTPCodecCapability: opusCodecCapability,
				PayloadType:        111,
			})
		} else if !params.DisableRed && strings.EqualFold(codecs[0].MimeType, webrtc.MimeTypeOpus) {
			// if upstream is opus only and red enabled, add red to match clients that support red
			codecs = append(codecs, webrtc.RTPCodecParameters{
				RTPCodecCapability: redCodecCapability,
				PayloadType:        63,
			})
			// prefer red codec
			codecs[0], codecs[1] = codecs[1], codecs[0]
		}
	}

	return &WrappedReceiver{
		params:    params,
		receivers: sfuReceivers,
		codecs:    codecs,
	}
}

func (r *WrappedReceiver) TrackID() livekit.TrackID {
	return r.params.TrackID
}

func (r *WrappedReceiver) StreamID() string {
	return r.params.StreamId
}

func (r *WrappedReceiver) DetermineReceiver(codec webrtc.RTPCodecCapability) {
	r.determinedCodec = codec
	for _, receiver := range r.receivers {
		if c := receiver.Codec(); strings.EqualFold(c.MimeType, codec.MimeType) {
			r.TrackReceiver = receiver
			break
		} else if strings.EqualFold(c.MimeType, sfu.MimeTypeAudioRed) && strings.EqualFold(codec.MimeType, webrtc.MimeTypeOpus) {
			// audio opus/red can match opus only
			r.TrackReceiver = receiver.GetPrimaryReceiverForRed()
			break
		} else if strings.EqualFold(c.MimeType, webrtc.MimeTypeOpus) && strings.EqualFold(codec.MimeType, sfu.MimeTypeAudioRed) {
			r.TrackReceiver = receiver.GetRedReceiver()
			break
		}
	}
	if r.TrackReceiver == nil {
		r.params.Logger.Errorw("can't determine receiver for codec", nil, "codec", codec.MimeType)
		if len(r.receivers) > 0 {
			r.TrackReceiver = r.receivers[0]
		}
	}
}

func (r *WrappedReceiver) Codecs() []webrtc.RTPCodecParameters {
	codecs := make([]webrtc.RTPCodecParameters, len(r.codecs))
	copy(codecs, r.codecs)
	return codecs
}

func (r *WrappedReceiver) DeleteDownTrack(participantID livekit.ParticipantID) {
	if r.TrackReceiver != nil {
		r.TrackReceiver.DeleteDownTrack(participantID)
	}
}

// --------------------------------------------

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

func (d *DummyReceiver) GetLayeredBitrate() ([]int32, sfu.Bitrates) {
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		return r.GetLayeredBitrate()
	}
	return nil, sfu.Bitrates{}
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

func (d *DummyReceiver) DeleteDownTrack(subscriberID livekit.ParticipantID) {
	d.downtrackLock.Lock()
	defer d.downtrackLock.Unlock()
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		r.DeleteDownTrack(subscriberID)
	} else {
		delete(d.downtracks, subscriberID)
	}
}

func (d *DummyReceiver) DebugInfo() map[string]interface{} {
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		return r.DebugInfo()
	}
	return nil
}

func (d *DummyReceiver) GetTemporalLayerFpsForSpatial(spatial int32) []float32 {
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		return r.GetTemporalLayerFpsForSpatial(spatial)
	}
	return nil
}

func (d *DummyReceiver) TrackInfo() *livekit.TrackInfo {
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		return r.TrackInfo()
	}
	return nil
}

func (d *DummyReceiver) UpdateTrackInfo(ti *livekit.TrackInfo) {
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		r.UpdateTrackInfo(ti)
	}
}

func (d *DummyReceiver) IsClosed() bool {
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		return r.IsClosed()
	}
	return false
}

func (d *DummyReceiver) GetPrimaryReceiverForRed() sfu.TrackReceiver {
	// DummyReceiver used for video, it should not have RED codec
	return d
}

func (d *DummyReceiver) GetRedReceiver() sfu.TrackReceiver {
	return d
}

func (d *DummyReceiver) GetTrackStats() *livekit.RTPStats {
	if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		return r.GetTrackStats()
	}
	return nil
}

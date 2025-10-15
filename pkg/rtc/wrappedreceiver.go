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
	"sync"

	"github.com/pion/webrtc/v4"
	"go.uber.org/atomic"
	"golang.org/x/exp/maps"
	"golang.org/x/exp/slices"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/mime"
)

// wrapper around WebRTC receiver, overriding its ID

type WrappedReceiverParams struct {
	Receivers      []*simulcastReceiver
	TrackID        livekit.TrackID
	StreamId       string
	UpstreamCodecs []webrtc.RTPCodecParameters
	Logger         logger.Logger
	DisableRed     bool
	IsEncrypted    bool
}

type WrappedReceiver struct {
	lock sync.Mutex

	sfu.TrackReceiver
	params           WrappedReceiverParams
	receivers        []sfu.TrackReceiver
	codecs           []webrtc.RTPCodecParameters
	onReadyCallbacks []func()
}

func NewWrappedReceiver(params WrappedReceiverParams) *WrappedReceiver {
	sfuReceivers := make([]sfu.TrackReceiver, 0, len(params.Receivers))
	for _, r := range params.Receivers {
		sfuReceivers = append(sfuReceivers, r)
	}

	codecs := params.UpstreamCodecs
	if len(codecs) == 1 {
		normalizedMimeType := mime.NormalizeMimeType(codecs[0].MimeType)
		if normalizedMimeType == mime.MimeTypeRED {
			// if upstream is opus/red, then add opus to match clients that don't support red
			codecs = append(codecs, OpusCodecParameters)
		} else if !params.DisableRed && normalizedMimeType == mime.MimeTypeOpus {
			// if upstream is opus only and red enabled, add red to match clients that support red
			codecs = append(codecs, RedCodecParameters)
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

// DetermineReceiver determines the receiver of negotiated codec and returns
//
// isAvailable: returns true if given codec is a potential codec from publisher or if an existing published codec can be translated
// needsPublish: indicates if the codec is needed from publisher, some combinations can be achieved via codec translation internally,
//
//	example: unecrypted opus -> RED translation and vice-versa can be done without the need for publisher to send the other codec.
func (r *WrappedReceiver) DetermineReceiver(codec webrtc.RTPCodecCapability) (isAvailable bool, needsPublish bool) {
	r.lock.Lock()

	codecMimeType := mime.NormalizeMimeType(codec.MimeType)
	var trackReceiver sfu.TrackReceiver
	for _, receiver := range r.receivers {
		receiverMimeType := receiver.Mime()
		if receiverMimeType == codecMimeType {
			trackReceiver = receiver
			isAvailable = true
			needsPublish = true
			break
		}

		if !r.params.IsEncrypted {
			if receiverMimeType == mime.MimeTypeRED && codecMimeType == mime.MimeTypeOpus {
				// audio opus/red can match opus only
				trackReceiver = receiver.GetPrimaryReceiverForRed()
				isAvailable = true
				break
			} else if receiverMimeType == mime.MimeTypeOpus && codecMimeType == mime.MimeTypeRED {
				trackReceiver = receiver.GetRedReceiver()
				isAvailable = true
				break
			}
		}
	}
	if trackReceiver == nil {
		r.lock.Unlock()
		r.params.Logger.Warnw("can't determine receiver for codec", nil, "codec", codec.MimeType)
		return
	}
	r.TrackReceiver = trackReceiver

	onReadyCallbacks := r.onReadyCallbacks
	r.onReadyCallbacks = nil
	r.lock.Unlock()

	for _, f := range onReadyCallbacks {
		trackReceiver.AddOnReady(f)
	}

	return
}

func (r *WrappedReceiver) Codecs() []webrtc.RTPCodecParameters {
	return slices.Clone(r.codecs)
}

func (r *WrappedReceiver) DeleteDownTrack(participantID livekit.ParticipantID) {
	r.lock.Lock()
	trackReceiver := r.TrackReceiver
	r.lock.Unlock()

	if trackReceiver != nil {
		trackReceiver.DeleteDownTrack(participantID)
	}
}

func (r *WrappedReceiver) AddOnReady(f func()) {
	r.lock.Lock()
	trackReceiver := r.TrackReceiver
	if trackReceiver == nil {
		r.onReadyCallbacks = append(r.onReadyCallbacks, f)
		r.lock.Unlock()
		return
	}
	r.lock.Unlock()

	trackReceiver.AddOnReady(f)
}

// --------------------------------------------

type DummyReceiver struct {
	receiver         atomic.Value
	trackID          livekit.TrackID
	streamId         string
	codec            webrtc.RTPCodecParameters
	headerExtensions []webrtc.RTPHeaderExtensionParameter

	downTrackLock      sync.Mutex
	downTracks         map[livekit.ParticipantID]sfu.TrackSender
	onReadyCallbacks   []func()
	onCodecStateChange []func(webrtc.RTPCodecParameters, sfu.ReceiverCodecState)

	settingsLock          sync.Mutex
	maxExpectedLayerValid bool
	maxExpectedLayer      int32
	pausedValid           bool
	paused                bool

	redReceiver, primaryReceiver *DummyRedReceiver
}

func NewDummyReceiver(trackID livekit.TrackID, streamId string, codec webrtc.RTPCodecParameters, headerExtensions []webrtc.RTPHeaderExtensionParameter) *DummyReceiver {
	return &DummyReceiver{
		trackID:          trackID,
		streamId:         streamId,
		codec:            codec,
		headerExtensions: headerExtensions,
		downTracks:       make(map[livekit.ParticipantID]sfu.TrackSender),
	}
}

func (d *DummyReceiver) Receiver() sfu.TrackReceiver {
	r, _ := d.receiver.Load().(sfu.TrackReceiver)
	return r
}

func (d *DummyReceiver) Upgrade(receiver sfu.TrackReceiver) {
	if !d.receiver.CompareAndSwap(nil, receiver) {
		return
	}

	d.downTrackLock.Lock()
	for _, t := range d.downTracks {
		receiver.AddDownTrack(t)
	}
	d.downTracks = make(map[livekit.ParticipantID]sfu.TrackSender)

	onReadyCallbacks := d.onReadyCallbacks
	d.onReadyCallbacks = nil

	codecChange := d.onCodecStateChange
	d.onCodecStateChange = nil
	d.downTrackLock.Unlock()

	for _, f := range onReadyCallbacks {
		receiver.AddOnReady(f)
	}

	for _, f := range codecChange {
		receiver.AddOnCodecStateChange(f)
	}

	d.settingsLock.Lock()
	maxExpectedLayerValid := d.maxExpectedLayerValid
	d.maxExpectedLayerValid = false

	pausedValid := d.pausedValid
	d.pausedValid = false
	d.settingsLock.Unlock()

	if maxExpectedLayerValid {
		receiver.SetMaxExpectedSpatialLayer(d.maxExpectedLayer)
	}

	if pausedValid {
		receiver.SetUpTrackPaused(d.paused)
	}

	d.settingsLock.Lock()
	if d.primaryReceiver != nil {
		d.primaryReceiver.upgrade(receiver)
	}
	if d.redReceiver != nil {
		d.redReceiver.upgrade(receiver)
	}
	d.settingsLock.Unlock()
}

func (d *DummyReceiver) TrackID() livekit.TrackID {
	return d.trackID
}

func (d *DummyReceiver) StreamID() string {
	return d.streamId
}

func (d *DummyReceiver) Codec() webrtc.RTPCodecParameters {
	if receiver := d.getReceiver(); receiver != nil {
		return receiver.Codec()
	}
	return d.codec
}

func (d *DummyReceiver) Mime() mime.MimeType {
	if receiver := d.getReceiver(); receiver != nil {
		return receiver.Mime()
	}
	return mime.NormalizeMimeType(d.codec.MimeType)
}

func (d *DummyReceiver) VideoLayerMode() livekit.VideoLayer_Mode {
	if receiver := d.getReceiver(); receiver != nil {
		return receiver.VideoLayerMode()
	}
	return buffer.GetVideoLayerModeForMimeType(d.Mime(), d.TrackInfo())
}

func (d *DummyReceiver) HeaderExtensions() []webrtc.RTPHeaderExtensionParameter {
	if receiver := d.getReceiver(); receiver != nil {
		return receiver.HeaderExtensions()
	}
	return d.headerExtensions
}

func (d *DummyReceiver) ReadRTP(buf []byte, layer uint8, esn uint64) (int, error) {
	if receiver := d.getReceiver(); receiver != nil {
		return receiver.ReadRTP(buf, layer, esn)
	}
	return 0, errors.New("no receiver")
}

func (d *DummyReceiver) GetLayeredBitrate() ([]int32, sfu.Bitrates) {
	if receiver := d.getReceiver(); receiver != nil {
		return receiver.GetLayeredBitrate()
	}
	return nil, sfu.Bitrates{}
}

func (d *DummyReceiver) GetAudioLevel() (float64, bool) {
	if receiver := d.getReceiver(); receiver != nil {
		return receiver.GetAudioLevel()
	}
	return 0, false
}

func (d *DummyReceiver) SendPLI(layer int32, force bool) {
	if receiver := d.getReceiver(); receiver != nil {
		receiver.SendPLI(layer, force)
	}
}

func (d *DummyReceiver) SetUpTrackPaused(paused bool) {
	d.settingsLock.Lock()
	receiver := d.getReceiver()
	if receiver != nil {
		d.pausedValid = false
	} else {
		d.pausedValid = true
		d.paused = paused
	}
	d.settingsLock.Unlock()

	if receiver != nil {
		receiver.SetUpTrackPaused(paused)
	}
}

func (d *DummyReceiver) SetMaxExpectedSpatialLayer(layer int32) {
	d.settingsLock.Lock()
	receiver := d.getReceiver()
	if receiver != nil {
		d.maxExpectedLayerValid = false
	} else {
		d.maxExpectedLayerValid = true
		d.maxExpectedLayer = layer
	}
	d.settingsLock.Unlock()

	if receiver != nil {
		receiver.SetMaxExpectedSpatialLayer(layer)
	}
}

func (d *DummyReceiver) AddDownTrack(track sfu.TrackSender) error {
	d.downTrackLock.Lock()
	defer d.downTrackLock.Unlock()

	if receiver := d.getReceiver(); receiver != nil {
		receiver.AddDownTrack(track)
	} else {
		d.downTracks[track.SubscriberID()] = track
	}
	return nil
}

func (d *DummyReceiver) DeleteDownTrack(subscriberID livekit.ParticipantID) {
	d.downTrackLock.Lock()
	defer d.downTrackLock.Unlock()

	if receiver := d.getReceiver(); receiver != nil {
		receiver.DeleteDownTrack(subscriberID)
	} else {
		delete(d.downTracks, subscriberID)
	}
}

func (d *DummyReceiver) GetDownTracks() []sfu.TrackSender {
	d.downTrackLock.Lock()
	defer d.downTrackLock.Unlock()

	if receiver := d.getReceiver(); receiver != nil {
		return receiver.GetDownTracks()
	}
	return maps.Values(d.downTracks)
}

func (d *DummyReceiver) DebugInfo() map[string]interface{} {
	if receiver := d.getReceiver(); receiver != nil {
		return receiver.DebugInfo()
	}
	return nil
}

func (d *DummyReceiver) GetTemporalLayerFpsForSpatial(spatial int32) []float32 {
	if receiver := d.getReceiver(); receiver != nil {
		return receiver.GetTemporalLayerFpsForSpatial(spatial)
	}
	return nil
}

func (d *DummyReceiver) TrackInfo() *livekit.TrackInfo {
	if receiver := d.getReceiver(); receiver != nil {
		return receiver.TrackInfo()
	}
	return nil
}

func (d *DummyReceiver) UpdateTrackInfo(ti *livekit.TrackInfo) {
	if receiver := d.getReceiver(); receiver != nil {
		receiver.UpdateTrackInfo(ti)
	}
}

func (d *DummyReceiver) IsClosed() bool {
	if receiver := d.getReceiver(); receiver != nil {
		return receiver.IsClosed()
	}
	return false
}

func (d *DummyReceiver) GetPrimaryReceiverForRed() sfu.TrackReceiver {
	d.settingsLock.Lock()
	defer d.settingsLock.Unlock()

	if d.primaryReceiver == nil {
		d.primaryReceiver = NewDummyRedReceiver(d, false)
		if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
			d.primaryReceiver.upgrade(r)
		}
	}
	return d.primaryReceiver
}

func (d *DummyReceiver) GetRedReceiver() sfu.TrackReceiver {
	d.settingsLock.Lock()
	defer d.settingsLock.Unlock()
	if d.redReceiver == nil {
		d.redReceiver = NewDummyRedReceiver(d, true)
		if r, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
			d.redReceiver.upgrade(r)
		}
	}
	return d.redReceiver
}

func (d *DummyReceiver) GetTrackStats() *livekit.RTPStats {
	if receiver := d.getReceiver(); receiver != nil {
		return receiver.GetTrackStats()
	}
	return nil
}

func (d *DummyReceiver) AddOnReady(f func()) {
	d.downTrackLock.Lock()
	receiver := d.getReceiver()
	if receiver == nil {
		d.onReadyCallbacks = append(d.onReadyCallbacks, f)
	}
	d.downTrackLock.Unlock()
	if receiver != nil {
		receiver.AddOnReady(f)
	}
}

func (d *DummyReceiver) AddOnCodecStateChange(f func(codec webrtc.RTPCodecParameters, state sfu.ReceiverCodecState)) {
	d.downTrackLock.Lock()
	receiver := d.getReceiver()
	if receiver == nil {
		d.onCodecStateChange = append(d.onCodecStateChange, f)
	}
	d.downTrackLock.Unlock()
	if receiver != nil {
		receiver.AddOnCodecStateChange(f)
	}
}

func (d *DummyReceiver) CodecState() sfu.ReceiverCodecState {
	if receiver := d.getReceiver(); receiver != nil {
		return receiver.CodecState()
	}
	return sfu.ReceiverCodecStateNormal
}

func (d *DummyReceiver) VideoSizes() []buffer.VideoSize {
	if receiver := d.getReceiver(); receiver != nil {
		return receiver.VideoSizes()
	}

	return nil
}

func (d *DummyReceiver) getReceiver() sfu.TrackReceiver {
	if receiver, ok := d.receiver.Load().(sfu.TrackReceiver); ok {
		return receiver
	}

	return nil
}

// --------------------------------------------

type DummyRedReceiver struct {
	*DummyReceiver
	redReceiver atomic.Value // sfu.TrackReceiver
	// indicates this receiver is for RED encoding receiver of primary codec OR
	// primary decoding receiver of RED codec
	isRedEncoding bool

	downTrackLock sync.Mutex
	downTracks    map[livekit.ParticipantID]sfu.TrackSender
}

func NewDummyRedReceiver(d *DummyReceiver, isRedEncoding bool) *DummyRedReceiver {
	return &DummyRedReceiver{
		DummyReceiver: d,
		isRedEncoding: isRedEncoding,
		downTracks:    make(map[livekit.ParticipantID]sfu.TrackSender),
	}
}

func (d *DummyRedReceiver) AddDownTrack(track sfu.TrackSender) error {
	d.downTrackLock.Lock()
	defer d.downTrackLock.Unlock()

	if r, ok := d.redReceiver.Load().(sfu.TrackReceiver); ok {
		r.AddDownTrack(track)
	} else {
		d.downTracks[track.SubscriberID()] = track
	}
	return nil
}

func (d *DummyRedReceiver) DeleteDownTrack(subscriberID livekit.ParticipantID) {
	d.downTrackLock.Lock()
	defer d.downTrackLock.Unlock()

	if r, ok := d.redReceiver.Load().(sfu.TrackReceiver); ok {
		r.DeleteDownTrack(subscriberID)
	} else {
		delete(d.downTracks, subscriberID)
	}
}

func (d *DummyRedReceiver) GetDownTracks() []sfu.TrackSender {
	d.downTrackLock.Lock()
	defer d.downTrackLock.Unlock()

	if r, ok := d.redReceiver.Load().(sfu.TrackReceiver); ok {
		return r.GetDownTracks()
	}
	return maps.Values(d.downTracks)
}

func (d *DummyRedReceiver) ReadRTP(buf []byte, layer uint8, esn uint64) (int, error) {
	if r, ok := d.redReceiver.Load().(sfu.TrackReceiver); ok {
		return r.ReadRTP(buf, layer, esn)
	}
	return 0, errors.New("no receiver")
}

func (d *DummyRedReceiver) upgrade(receiver sfu.TrackReceiver) {
	var redReceiver sfu.TrackReceiver
	if d.isRedEncoding {
		redReceiver = receiver.GetRedReceiver()
	} else {
		redReceiver = receiver.GetPrimaryReceiverForRed()
	}
	d.redReceiver.Store(redReceiver)

	d.downTrackLock.Lock()
	for _, t := range d.downTracks {
		redReceiver.AddDownTrack(t)
	}
	d.downTracks = make(map[livekit.ParticipantID]sfu.TrackSender)
	d.downTrackLock.Unlock()
}

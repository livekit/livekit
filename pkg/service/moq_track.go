// Copyright 2026 LiveKit, Inc.
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

package service

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/protocol/codecs/mime"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/pion/webrtc/v4"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/packettrailer"
)

type moqTrackKey struct {
	room    livekit.RoomName
	trackID livekit.TrackID
}

type moqTrackRegistry struct {
	params moqTrackRegistryParams

	lock   sync.RWMutex
	tracks map[moqTrackKey]*moqTrackTap
}

type moqTrackRegistryParams struct {
	Config config.MoQConfig
	Logger logger.Logger
}

func newMoQTrackRegistry(params moqTrackRegistryParams) *moqTrackRegistry {
	return &moqTrackRegistry{
		params: params,
		tracks: make(map[moqTrackKey]*moqTrackTap),
	}
}

func (r *moqTrackRegistry) AttachRoom(room *rtc.Room) {
	for _, participant := range room.GetParticipants() {
		for _, track := range participant.GetPublishedTracks() {
			r.AttachTrack(room, participant, track)
		}
	}

	room.AddOnTrackPublished(func(participant types.Participant, track types.MediaTrack) {
		r.AttachTrack(room, participant, track)
	})
}

func (r *moqTrackRegistry) AttachTrack(room *rtc.Room, participant types.Participant, track types.MediaTrack) *moqTrackTap {
	if !isMoQSupportedTrack(track) {
		return nil
	}

	key := moqTrackKey{room: room.Name(), trackID: track.ID()}

	r.lock.Lock()
	tap := r.tracks[key]
	if tap == nil {
		tap = newMoQTrackTap(moqTrackTapParams{
			Config:      r.params.Config,
			Logger:      r.params.Logger,
			Room:        room,
			Participant: participant,
			Track:       track,
		})
		r.tracks[key] = tap
		track.AddOnClose(func(_ bool) {
			r.lock.Lock()
			if r.tracks[key] == tap {
				delete(r.tracks, key)
			}
			r.lock.Unlock()
			tap.Close()
		})
		if observer, ok := track.(interface{ OnSetupReceiver(func(mime.MimeType)) }); ok {
			observer.OnSetupReceiver(func(mime.MimeType) {
				tap.AttachReceivers()
			})
		}
	}
	r.lock.Unlock()

	tap.AttachReceivers()
	return tap
}

func (r *moqTrackRegistry) Get(roomName livekit.RoomName, trackID livekit.TrackID) *moqTrackTap {
	r.lock.RLock()
	defer r.lock.RUnlock()
	return r.tracks[moqTrackKey{room: roomName, trackID: trackID}]
}

func isMoQSupportedTrack(track types.MediaTrack) bool {
	if track.Kind() != livekit.TrackType_VIDEO || track.IsEncrypted() {
		return false
	}
	ti := track.ToProto()
	return ti.GetMimeType() == "" || mime.IsMimeTypeStringEqual(ti.GetMimeType(), mime.MimeTypeH264.String())
}

type moqTrackTapParams struct {
	Config      config.MoQConfig
	Logger      logger.Logger
	Room        *rtc.Room
	Participant types.Participant
	Track       types.MediaTrack
}

type moqTrackTap struct {
	params moqTrackTapParams

	id            string
	subscriberID  livekit.ParticipantID
	qualityNodeID livekit.NodeID

	lock        sync.RWMutex
	receivers   map[sfu.TrackReceiver]struct{}
	layers      map[int32]*moqLayerState
	subscribers map[string]*moqTrackSubscriber
	closed      bool

	sequence atomic.Uint64
}

type moqLayerState struct {
	lock      sync.Mutex
	assembler *h264AccessUnitAssembler
	cache     *moqSample
}

type moqSample struct {
	TrackID         livekit.TrackID
	TrackName       string
	PublisherID     livekit.ParticipantID
	Publisher       livekit.ParticipantIdentity
	MimeType        string
	Width           uint32
	Height          uint32
	Layer           int32
	Sequence        uint64
	RTPTime         uint32
	KeyFrame        bool
	Cached          bool
	SentAtUnixNs    int64
	UserTimestampUs *int64
	FrameID         *uint32
	Payload         []byte
}

func (s *moqSample) Clone(cached bool) *moqSample {
	if s == nil {
		return nil
	}
	clone := *s
	clone.Cached = cached
	if s.UserTimestampUs != nil {
		userTimestampUs := *s.UserTimestampUs
		clone.UserTimestampUs = &userTimestampUs
	}
	if s.FrameID != nil {
		frameID := *s.FrameID
		clone.FrameID = &frameID
	}
	clone.Payload = cloneBytes(s.Payload)
	return &clone
}

type moqTrackSubscriber struct {
	id    string
	layer int32
	queue chan *moqSample
	done  chan struct{}
}

func newMoQTrackTap(params moqTrackTapParams) *moqTrackTap {
	trackID := params.Track.ID()
	return &moqTrackTap{
		params:        params,
		id:            fmt.Sprintf("moq:%s:%s", params.Room.Name(), trackID),
		subscriberID:  livekit.ParticipantID(fmt.Sprintf("MOQ_%s", trackID)),
		qualityNodeID: livekit.NodeID(fmt.Sprintf("moq:%s:%s", params.Room.Name(), trackID)),
		receivers:     make(map[sfu.TrackReceiver]struct{}),
		layers:        make(map[int32]*moqLayerState),
		subscribers:   make(map[string]*moqTrackSubscriber),
	}
}

func (t *moqTrackTap) AttachReceivers() {
	t.lock.Lock()
	if t.closed || len(t.subscribers) == 0 {
		t.lock.Unlock()
		return
	}
	receivers := t.params.Track.Receivers()
	for _, receiver := range receivers {
		if receiver == nil || receiver.IsClosed() || !mime.IsMimeTypeStringEqual(receiver.Mime().String(), mime.MimeTypeH264.String()) {
			continue
		}
		if _, ok := t.receivers[receiver]; ok {
			continue
		}
		if err := receiver.AddDownTrack(t); err != nil {
			t.params.Logger.Warnw("could not attach moq track tap", err, "trackID", t.params.Track.ID())
			continue
		}
		t.receivers[receiver] = struct{}{}
		receiver.SendPLI(0, true)
	}
	t.lock.Unlock()
}

func (t *moqTrackTap) Subscribe(layer int32) (*moqTrackSubscriber, *moqSample, func()) {
	sub := &moqTrackSubscriber{
		id:    fmt.Sprintf("%s:%d", t.id, time.Now().UnixNano()),
		layer: layer,
		queue: make(chan *moqSample, t.params.Config.TrackQueueSize),
		done:  make(chan struct{}),
	}

	var cached *moqSample
	var shouldEnableQuality bool
	t.lock.Lock()
	if t.closed {
		t.lock.Unlock()
		close(sub.done)
		return sub, nil, func() {}
	}
	shouldEnableQuality = len(t.subscribers) == 0
	t.subscribers[sub.id] = sub
	if layerState := t.layers[layer]; layerState != nil {
		layerState.lock.Lock()
		cached = layerState.cache.Clone(true)
		layerState.lock.Unlock()
	}
	t.lock.Unlock()

	if shouldEnableQuality {
		t.setSubscriberNodeQuality(livekit.VideoQuality_HIGH)
	}
	t.AttachReceivers()
	t.lock.RLock()
	for receiver := range t.receivers {
		receiver.SendPLI(layer, false)
	}
	t.lock.RUnlock()

	unsubscribe := func() {
		var shouldDisableQuality bool
		t.lock.Lock()
		if t.subscribers[sub.id] == sub {
			delete(t.subscribers, sub.id)
			close(sub.done)
			shouldDisableQuality = len(t.subscribers) == 0
		}
		t.lock.Unlock()
		if shouldDisableQuality {
			t.detachReceivers()
			t.setSubscriberNodeQuality(livekit.VideoQuality_OFF)
		}
	}
	return sub, cached, unsubscribe
}

func (t *moqTrackTap) setSubscriberNodeQuality(quality livekit.VideoQuality) {
	localTrack, ok := t.params.Track.(types.LocalMediaTrack)
	if !ok {
		return
	}

	localTrack.NotifySubscriberNodeMaxQuality(t.qualityNodeID, []types.SubscribedCodecQuality{
		{
			CodecMime: mime.MimeTypeH264,
			Quality:   quality,
		},
	})
}

func (t *moqTrackTap) detachReceivers() {
	t.lock.Lock()
	for receiver := range t.receivers {
		receiver.DeleteDownTrack(t.subscriberID)
	}
	t.receivers = make(map[sfu.TrackReceiver]struct{})
	t.lock.Unlock()
}

func (t *moqTrackTap) CatalogTracks() []moqWireCatalogTrack {
	ti := t.params.Track.ToProto()
	return []moqWireCatalogTrack{
		{
			TrackID:     string(t.params.Track.ID()),
			TrackName:   t.params.Track.Name(),
			PublisherID: string(t.params.Participant.ID()),
			Publisher:   string(t.params.Participant.Identity()),
			MimeType:    mime.MimeTypeH264.String(),
			Width:       ti.GetWidth(),
			Height:      ti.GetHeight(),
		},
	}
}

func (t *moqTrackTap) Close() {
	t.lock.Lock()
	if t.closed {
		t.lock.Unlock()
		return
	}
	t.closed = true
	for receiver := range t.receivers {
		receiver.DeleteDownTrack(t.subscriberID)
	}
	for _, sub := range t.subscribers {
		close(sub.done)
	}
	t.subscribers = make(map[string]*moqTrackSubscriber)
	t.lock.Unlock()
	t.setSubscriberNodeQuality(livekit.VideoQuality_OFF)
}

func (t *moqTrackTap) UpTrackLayersChange() {}

func (t *moqTrackTap) UpTrackBitrateAvailabilityChange() {}

func (t *moqTrackTap) UpTrackMaxPublishedLayerChange(int32) {}

func (t *moqTrackTap) UpTrackMaxTemporalLayerSeenChange(int32) {}

func (t *moqTrackTap) UpTrackBitrateReport([]int32, sfu.Bitrates) {}

func (t *moqTrackTap) WriteRTP(extPkt *buffer.ExtPacket, layer int32) int32 {
	if extPkt == nil || extPkt.Packet == nil {
		return 0
	}

	payload := cloneBytes(extPkt.Packet.Payload)
	trailerMetadata, strip := packettrailer.ParseTrailer(payload, extPkt.Packet.Marker)
	if strip > 0 {
		payload = payload[:len(payload)-strip]
	}
	layerState := t.getLayerState(layer)

	layerState.lock.Lock()
	au, err := layerState.assembler.Push(payload, extPkt.Packet.Timestamp, extPkt.Packet.Marker)
	layerState.lock.Unlock()
	if err != nil || au == nil {
		return 0
	}

	ti := t.params.Track.ToProto()
	sample := &moqSample{
		TrackID:      t.params.Track.ID(),
		TrackName:    t.params.Track.Name(),
		PublisherID:  t.params.Participant.ID(),
		Publisher:    t.params.Participant.Identity(),
		MimeType:     mime.MimeTypeH264.String(),
		Width:        ti.GetWidth(),
		Height:       ti.GetHeight(),
		Layer:        layer,
		Sequence:     t.sequence.Add(1),
		RTPTime:      au.RTPTime,
		KeyFrame:     extPkt.IsKeyFrame || au.HasIDR,
		SentAtUnixNs: time.Now().UnixNano(),
		Payload:      au.Payload,
	}
	if trailerMetadata.HasUserTimestampUs {
		sample.UserTimestampUs = &trailerMetadata.UserTimestampUs
	}
	if trailerMetadata.HasFrameID {
		sample.FrameID = &trailerMetadata.FrameID
	}

	if sample.KeyFrame && len(sample.Payload) <= t.params.Config.CacheMaxBytes {
		layerState.lock.Lock()
		layerState.cache = sample.Clone(false)
		layerState.lock.Unlock()
	}

	return t.publish(sample)
}

func (t *moqTrackTap) getLayerState(layer int32) *moqLayerState {
	t.lock.Lock()
	defer t.lock.Unlock()

	layerState := t.layers[layer]
	if layerState == nil {
		layerState = &moqLayerState{assembler: newH264AccessUnitAssembler()}
		t.layers[layer] = layerState
	}
	return layerState
}

func (t *moqTrackTap) publish(sample *moqSample) int32 {
	t.lock.RLock()
	if t.closed {
		t.lock.RUnlock()
		return 0
	}
	subscribers := make([]*moqTrackSubscriber, 0, len(t.subscribers))
	for _, sub := range t.subscribers {
		if sub.layer == sample.Layer {
			subscribers = append(subscribers, sub)
		}
	}
	t.lock.RUnlock()

	var written int32
	for _, sub := range subscribers {
		if enqueueMoQSample(sub, sample.Clone(false)) {
			written++
		}
	}
	return written
}

func enqueueMoQSample(sub *moqTrackSubscriber, sample *moqSample) bool {
	select {
	case <-sub.done:
		return false
	case sub.queue <- sample:
		return true
	default:
	}

	if !sample.KeyFrame {
		return false
	}

	select {
	case <-sub.done:
		return false
	case <-sub.queue:
	default:
	}
	select {
	case <-sub.done:
		return false
	case sub.queue <- sample:
		return true
	default:
		return false
	}
}

func (t *moqTrackTap) ID() string {
	return t.id
}

func (t *moqTrackTap) SubscriberID() livekit.ParticipantID {
	return t.subscriberID
}

func (t *moqTrackTap) HandleRTCPSenderReportData(webrtc.PayloadType, int32, *livekit.RTCPSenderReportState) error {
	return nil
}

func (t *moqTrackTap) Resync() {}

func (t *moqTrackTap) SetReceiver(sfu.TrackReceiver) {}

func (t *moqTrackTap) ReceiverRestart(sfu.TrackReceiver) {
	t.AttachReceivers()
}

func (t *moqTrackTap) IsClosed() bool {
	t.lock.RLock()
	defer t.lock.RUnlock()
	return t.closed
}

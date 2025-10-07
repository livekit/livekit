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
	"sync"
	"time"

	"github.com/bep/debounce"
	"github.com/pion/webrtc/v4"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/telemetry"
	sutils "github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/observability/roomobs"
	"github.com/livekit/protocol/utils"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/mime"
)

const (
	subscriptionDebounceInterval = 100 * time.Millisecond
)

var _ types.SubscribedTrack = (*SubscribedTrack)(nil)

type SubscribedTrackParams struct {
	ReceiverConfig               ReceiverConfig
	SubscriberConfig             DirectionConfig
	Subscriber                   types.LocalParticipant
	MediaTrack                   types.MediaTrack
	AdaptiveStream               bool
	Telemetry                    telemetry.TelemetryService
	WrappedReceiver              *WrappedReceiver
	IsRelayed                    bool
	OnDownTrackCreated           func(downTrack *sfu.DownTrack)
	OnDownTrackClosed            func(subscriberID livekit.ParticipantID)
	OnSubscriberMaxQualityChange func(subscriberID livekit.ParticipantID, mime mime.MimeType, layer int32)
	OnSubscriberAudioCodecChange func(subscriberID livekit.ParticipantID, mime mime.MimeType, enabled bool)
}

type SubscribedTrack struct {
	params           SubscribedTrackParams
	logger           logger.Logger
	downTrack        *sfu.DownTrack
	sender           atomic.Pointer[webrtc.RTPSender]
	needsNegotiation atomic.Bool

	versionGenerator utils.TimedVersionGenerator
	settingsLock     sync.Mutex
	settings         *livekit.UpdateTrackSettings
	settingsVersion  utils.TimedVersion

	bindLock        sync.Mutex
	bound           bool
	onBindCallbacks []func(error)

	onClose atomic.Value // func(bool)

	debouncer func(func())

	statsKey telemetry.StatsKey
	reporter roomobs.TrackReporter
}

func NewSubscribedTrack(params SubscribedTrackParams) (*SubscribedTrack, error) {
	s := &SubscribedTrack{
		params: params,
		logger: params.Subscriber.GetLogger().WithComponent(sutils.ComponentSub).WithValues(
			"trackID", params.MediaTrack.ID(),
			"publisherID", params.MediaTrack.PublisherID(),
			"publisher", params.MediaTrack.PublisherIdentity(),
		),
		versionGenerator: utils.NewDefaultTimedVersionGenerator(),
		debouncer:        debounce.New(subscriptionDebounceInterval),
		statsKey: telemetry.StatsKeyForTrack(
			params.Subscriber.GetCountry(),
			livekit.StreamType_DOWNSTREAM,
			params.Subscriber.ID(),
			params.MediaTrack.ID(),
			params.MediaTrack.Source(),
			params.MediaTrack.Kind(),
		),
		reporter: params.Subscriber.GetReporter().WithTrack(params.MediaTrack.ID().String()),
	}

	var rtcpFeedback []webrtc.RTCPFeedback
	var maxTrack int
	switch params.MediaTrack.Kind() {
	case livekit.TrackType_AUDIO:
		rtcpFeedback = params.SubscriberConfig.RTCPFeedback.Audio
		maxTrack = params.ReceiverConfig.PacketBufferSizeAudio
	case livekit.TrackType_VIDEO:
		rtcpFeedback = params.SubscriberConfig.RTCPFeedback.Video
		maxTrack = params.ReceiverConfig.PacketBufferSizeVideo
	default:
		s.logger.Warnw("unexpected track type", nil, "kind", params.MediaTrack.Kind())
	}
	codecs := params.WrappedReceiver.Codecs()
	for _, c := range codecs {
		c.RTCPFeedback = rtcpFeedback
	}

	streamID := params.WrappedReceiver.StreamID()
	if params.Subscriber.SupportsSyncStreamID() && params.MediaTrack.Stream() != "" {
		streamID = PackSyncStreamID(params.MediaTrack.PublisherID(), params.MediaTrack.Stream())
	}

	isEncrypted := params.MediaTrack.IsEncrypted()
	var trailer []byte
	if isEncrypted {
		trailer = params.Subscriber.GetTrailer()
	}
	downTrack, err := sfu.NewDownTrack(sfu.DownTrackParams{
		Codecs:            codecs,
		IsEncrypted:       isEncrypted,
		Source:            params.MediaTrack.Source(),
		Receiver:          params.WrappedReceiver,
		BufferFactory:     params.Subscriber.GetBufferFactory(),
		SubID:             params.Subscriber.ID(),
		StreamID:          streamID,
		MaxTrack:          maxTrack,
		PlayoutDelayLimit: params.Subscriber.GetPlayoutDelayConfig(),
		Pacer:             params.Subscriber.GetPacer(),
		Trailer:           trailer,
		Logger: LoggerWithTrack(
			params.Subscriber.GetLogger().WithComponent(sutils.ComponentSub),
			params.MediaTrack.ID(),
			params.IsRelayed,
		),
		RTCPWriter:                     params.Subscriber.WriteSubscriberRTCP,
		DisableSenderReportPassThrough: params.Subscriber.GetDisableSenderReportPassThrough(),
		SupportsCodecChange:            params.Subscriber.SupportsCodecChange(),
		Listener:                       s,
	})
	if err != nil {
		return nil, err
	}

	if params.OnDownTrackCreated != nil {
		params.OnDownTrackCreated(downTrack)
	}

	downTrack.AddReceiverReportListener(params.Subscriber.HandleReceiverReport)

	s.downTrack = downTrack
	return s, nil
}

func (t *SubscribedTrack) AddOnBind(f func(error)) {
	t.bindLock.Lock()
	bound := t.bound
	if !bound {
		t.onBindCallbacks = append(t.onBindCallbacks, f)
	}
	t.bindLock.Unlock()

	if bound {
		// fire immediately, do not need to persist since bind is a one time event
		go f(nil)
	}
}

// for DownTrack callback to notify us that it's bound
func (t *SubscribedTrack) Bound(err error) {
	t.bindLock.Lock()
	if err == nil {
		t.bound = true
	}
	callbacks := t.onBindCallbacks
	t.onBindCallbacks = nil
	t.bindLock.Unlock()

	if err == nil && t.MediaTrack().Kind() == livekit.TrackType_VIDEO {
		// When AdaptiveStream is enabled, default the subscriber to LOW quality stream
		// we would want LOW instead of OFF for a couple of reasons
		// 1. when a subscriber unsubscribes from a track, we would forget their previously defined settings
		//    depending on client implementation, subscription on/off is kept separately from adaptive stream
		//    So when there are no changes to desired resolution, but the user re-subscribes, we may leave stream at OFF
		// 2. when interacting with dynacast *and* adaptive stream. If the publisher was not publishing at the
		//    time of subscription, we might not be able to trigger adaptive stream updates on the client side
		//    (since there isn't any video frames coming through). this will leave the stream "stuck" on off, without
		//    a trigger to re-enable it
		t.settingsLock.Lock()
		if t.settings != nil {
			if t.params.AdaptiveStream {
				// remove `disabled` flag to force a visibility update
				t.settings.Disabled = false
				t.logger.Debugw("enabling subscriber track settings on bind", "settings", logger.Proto(t.settings))
			}
		} else {
			if t.params.AdaptiveStream {
				t.settings = &livekit.UpdateTrackSettings{Quality: livekit.VideoQuality_LOW}
			} else {
				t.settings = &livekit.UpdateTrackSettings{Quality: livekit.VideoQuality_HIGH}
			}
			t.logger.Debugw("initializing subscriber track settings on bind", "settings", logger.Proto(t.settings))
		}
		t.settingsLock.Unlock()
		t.applySettings()
	}

	for _, cb := range callbacks {
		go cb(err)
	}
}

// for DownTrack callback to notify us that it's closed
func (t *SubscribedTrack) Close(isExpectedToResume bool) {
	if onClose := t.onClose.Load(); onClose != nil {
		go onClose.(func(bool))(isExpectedToResume)
	}
}

func (t *SubscribedTrack) OnClose(f func(bool)) {
	t.onClose.Store(f)
}

func (t *SubscribedTrack) IsBound() bool {
	t.bindLock.Lock()
	defer t.bindLock.Unlock()

	return t.bound
}

func (t *SubscribedTrack) ID() livekit.TrackID {
	return livekit.TrackID(t.downTrack.ID())
}

func (t *SubscribedTrack) PublisherID() livekit.ParticipantID {
	return t.params.MediaTrack.PublisherID()
}

func (t *SubscribedTrack) PublisherIdentity() livekit.ParticipantIdentity {
	return t.params.MediaTrack.PublisherIdentity()
}

func (t *SubscribedTrack) PublisherVersion() uint32 {
	return t.params.MediaTrack.PublisherVersion()
}

func (t *SubscribedTrack) SubscriberID() livekit.ParticipantID {
	return t.params.Subscriber.ID()
}

func (t *SubscribedTrack) SubscriberIdentity() livekit.ParticipantIdentity {
	return t.params.Subscriber.Identity()
}

func (t *SubscribedTrack) Subscriber() types.LocalParticipant {
	return t.params.Subscriber
}

func (t *SubscribedTrack) DownTrack() *sfu.DownTrack {
	return t.downTrack
}

func (t *SubscribedTrack) MediaTrack() types.MediaTrack {
	return t.params.MediaTrack
}

// has subscriber indicated it wants to mute this track
func (t *SubscribedTrack) IsMuted() bool {
	t.settingsLock.Lock()
	defer t.settingsLock.Unlock()

	return t.isMutedLocked()
}

func (t *SubscribedTrack) isMutedLocked() bool {
	if t.settings == nil {
		return false
	}

	return t.settings.Disabled
}

func (t *SubscribedTrack) SetPublisherMuted(muted bool) {
	t.downTrack.PubMute(muted)
}

func (t *SubscribedTrack) UpdateSubscriberSettings(settings *livekit.UpdateTrackSettings, isImmediate bool) {
	t.settingsLock.Lock()
	if proto.Equal(t.settings, settings) {
		t.logger.Debugw("skipping subscriber track settings", "settings", logger.Proto(t.settings))
		t.settingsLock.Unlock()
		return
	}

	isImmediate = isImmediate || (!settings.Disabled && settings.Disabled != t.isMutedLocked())
	t.settings = utils.CloneProto(settings)
	t.logger.Debugw("saving subscriber track settings", "settings", logger.Proto(t.settings))
	t.settingsLock.Unlock()

	if isImmediate {
		t.applySettings()
	} else {
		// avoid frequent changes to mute & video layers, unless it became visible
		t.debouncer(t.applySettings)
	}
}

func (t *SubscribedTrack) UpdateVideoLayer() {
	t.applySettings()
}

func (t *SubscribedTrack) applySettings() {
	t.settingsLock.Lock()
	if t.settings == nil {
		t.settingsLock.Unlock()
		return
	}

	t.settingsVersion = t.versionGenerator.Next()
	settingsVersion := t.settingsVersion
	t.settingsLock.Unlock()

	dt := t.DownTrack()
	spatial := buffer.InvalidLayerSpatial
	temporal := buffer.InvalidLayerTemporal
	if dt.Kind() == webrtc.RTPCodecTypeVideo {
		mt := t.MediaTrack()
		quality := t.settings.Quality
		mimeType := dt.Mime()
		if t.settings.Width > 0 {
			quality = mt.GetQualityForDimension(mimeType, t.settings.Width, t.settings.Height)
		}

		spatial = buffer.GetSpatialLayerForVideoQuality(mimeType, quality, mt.ToProto())
		if t.settings.Fps > 0 {
			temporal = mt.GetTemporalLayerForSpatialFps(mimeType, spatial, t.settings.Fps)
		}
	}

	t.settingsLock.Lock()
	if settingsVersion != t.settingsVersion {
		// a newer settings has superceded this one
		t.settingsLock.Unlock()
		return
	}

	t.logger.Debugw("applying subscriber track settings", "settings", logger.Proto(t.settings))
	if t.settings.Disabled {
		dt.Mute(true)
		t.settingsLock.Unlock()
		return
	} else {
		dt.Mute(false)
	}

	if dt.Kind() == webrtc.RTPCodecTypeVideo {
		dt.SetMaxSpatialLayer(spatial)
		if temporal != buffer.InvalidLayerTemporal {
			dt.SetMaxTemporalLayer(temporal)
		}
	}
	t.settingsLock.Unlock()
}

func (t *SubscribedTrack) NeedsNegotiation() bool {
	return t.needsNegotiation.Load()
}

func (t *SubscribedTrack) SetNeedsNegotiation(needs bool) {
	t.needsNegotiation.Store(needs)
}

func (t *SubscribedTrack) RTPSender() *webrtc.RTPSender {
	return t.sender.Load()
}

func (t *SubscribedTrack) SetRTPSender(sender *webrtc.RTPSender) {
	t.sender.Store(sender)
}

// DownTrackListener implementation
var _ sfu.DownTrackListener = (*SubscribedTrack)(nil)

func (t *SubscribedTrack) OnBindAndConnected() {
	if t.params.Subscriber.Hidden() {
		return
	}

	t.params.MediaTrack.OnTrackSubscribed()
}

func (t *SubscribedTrack) OnStatsUpdate(stat *livekit.AnalyticsStat) {
	t.params.Telemetry.TrackStats(t.statsKey, stat)

	if cs, ok := telemetry.CondenseStat(stat); ok {
		ti := t.params.WrappedReceiver.TrackInfo()
		t.reporter.Tx(func(tx roomobs.TrackTx) {
			tx.ReportName(ti.Name)
			tx.ReportKind(roomobs.TrackKindSub)
			tx.ReportType(roomobs.TrackTypeFromProto(ti.Type))
			tx.ReportSource(roomobs.TrackSourceFromProto(ti.Source))
			tx.ReportMime(mime.NormalizeMimeType(ti.MimeType).ReporterType())
			tx.ReportLayer(roomobs.PackTrackLayer(ti.Height, ti.Width))
			tx.ReportDuration(uint16(cs.EndTime.Sub(cs.StartTime).Milliseconds()))
			tx.ReportFrames(uint16(cs.Frames))
			tx.ReportSendBytes(uint32(cs.Bytes))
			tx.ReportSendPackets(cs.Packets)
			tx.ReportPacketsLost(cs.PacketsLost)
			tx.ReportScore(stat.Score)
		})
	}
}

func (t *SubscribedTrack) OnMaxSubscribedLayerChanged(layer int32) {
	if t.params.OnSubscriberMaxQualityChange != nil {
		t.params.OnSubscriberMaxQualityChange(t.downTrack.SubscriberID(), t.downTrack.Mime(), layer)
	}
}

func (t *SubscribedTrack) OnRttUpdate(rtt uint32) {
	go t.params.Subscriber.UpdateMediaRTT(rtt)
}

func (t *SubscribedTrack) OnCodecNegotiated(codec webrtc.RTPCodecCapability) {
	if isAvailable, needsPublish := t.params.WrappedReceiver.DetermineReceiver(codec); !isAvailable || !needsPublish {
		return
	}

	if t.params.OnSubscriberMaxQualityChange != nil || t.params.OnSubscriberAudioCodecChange != nil {
		go func() {
			mimeType := mime.NormalizeMimeType(codec.MimeType)
			switch t.params.MediaTrack.Kind() {
			case livekit.TrackType_VIDEO:
				spatial := buffer.GetSpatialLayerForVideoQuality(
					mimeType,
					livekit.VideoQuality_HIGH,
					t.params.MediaTrack.ToProto(),
				)
				if t.params.OnSubscriberMaxQualityChange != nil {
					t.params.OnSubscriberMaxQualityChange(t.downTrack.SubscriberID(), mimeType, spatial)
				}

			case livekit.TrackType_AUDIO:
				if t.params.OnSubscriberAudioCodecChange != nil {
					t.params.OnSubscriberAudioCodecChange(t.downTrack.SubscriberID(), mimeType, true)
				}
			}
		}()
	}
}

func (t *SubscribedTrack) OnDownTrackClose(isExpectedToResume bool) {
	// Cache transceiver for potential re-use on resume.
	// To ensure subscription manager does not re-subscribe before caching,
	// delete the subscribed track only after caching.
	if isExpectedToResume {
		if tr := t.downTrack.GetTransceiver(); tr != nil {
			t.params.Subscriber.CacheDownTrack(t.ID(), tr, t.downTrack.GetState())
		}
	}

	go func() {
		if t.params.OnDownTrackClosed != nil {
			t.params.OnDownTrackClosed(t.params.Subscriber.ID())
		}
		t.Close(isExpectedToResume)
	}()
}

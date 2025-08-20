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

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"go.uber.org/atomic"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/mime"
	sutils "github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/observability/roomobs"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/telemetry"
)

var (
	errAlreadySubscribed = errors.New("already subscribed")
	errNotFound          = errors.New("not found")
)

// MediaTrackSubscriptions manages subscriptions of a media track
type MediaTrackSubscriptions struct {
	params MediaTrackSubscriptionsParams

	subscribedTracksMu sync.RWMutex
	subscribedTracks   map[livekit.ParticipantID]types.SubscribedTrack

	onDownTrackCreated           func(downTrack *sfu.DownTrack)
	onSubscriberMaxQualityChange func(subscriberID livekit.ParticipantID, mime mime.MimeType, layer int32)
}

type MediaTrackSubscriptionsParams struct {
	MediaTrack types.MediaTrack
	IsRelayed  bool

	ReceiverConfig   ReceiverConfig
	SubscriberConfig DirectionConfig

	Telemetry telemetry.TelemetryService

	Logger logger.Logger
}

func NewMediaTrackSubscriptions(params MediaTrackSubscriptionsParams) *MediaTrackSubscriptions {
	return &MediaTrackSubscriptions{
		params:           params,
		subscribedTracks: make(map[livekit.ParticipantID]types.SubscribedTrack),
	}
}

func (t *MediaTrackSubscriptions) OnDownTrackCreated(f func(downTrack *sfu.DownTrack)) {
	t.onDownTrackCreated = f
}

func (t *MediaTrackSubscriptions) OnSubscriberMaxQualityChange(f func(subscriberID livekit.ParticipantID, mime mime.MimeType, layer int32)) {
	t.onSubscriberMaxQualityChange = f
}

func (t *MediaTrackSubscriptions) SetMuted(muted bool) {
	// update mute of all subscribed tracks
	for _, st := range t.getAllSubscribedTracks() {
		st.SetPublisherMuted(muted)
	}
}

func (t *MediaTrackSubscriptions) IsSubscriber(subID livekit.ParticipantID) bool {
	t.subscribedTracksMu.RLock()
	defer t.subscribedTracksMu.RUnlock()

	_, ok := t.subscribedTracks[subID]
	return ok
}

// AddSubscriber subscribes sub to current mediaTrack
func (t *MediaTrackSubscriptions) AddSubscriber(sub types.LocalParticipant, wr *WrappedReceiver) (types.SubscribedTrack, error) {
	trackID := t.params.MediaTrack.ID()
	subscriberID := sub.ID()

	// don't subscribe to the same track multiple times
	t.subscribedTracksMu.Lock()
	if _, ok := t.subscribedTracks[subscriberID]; ok {
		t.subscribedTracksMu.Unlock()
		return nil, errAlreadySubscribed
	}
	t.subscribedTracksMu.Unlock()

	var rtcpFeedback []webrtc.RTCPFeedback
	var maxTrack int
	switch t.params.MediaTrack.Kind() {
	case livekit.TrackType_AUDIO:
		rtcpFeedback = t.params.SubscriberConfig.RTCPFeedback.Audio
		maxTrack = t.params.ReceiverConfig.PacketBufferSizeAudio
	case livekit.TrackType_VIDEO:
		rtcpFeedback = t.params.SubscriberConfig.RTCPFeedback.Video
		maxTrack = t.params.ReceiverConfig.PacketBufferSizeVideo
	default:
		t.params.Logger.Warnw("unexpected track type", nil, "kind", t.params.MediaTrack.Kind())
	}
	codecs := wr.Codecs()
	for _, c := range codecs {
		c.RTCPFeedback = rtcpFeedback
	}

	streamID := wr.StreamID()
	if sub.SupportsSyncStreamID() && t.params.MediaTrack.Stream() != "" {
		streamID = PackSyncStreamID(t.params.MediaTrack.PublisherID(), t.params.MediaTrack.Stream())
	}

	var trailer []byte
	if t.params.MediaTrack.IsEncrypted() {
		trailer = sub.GetTrailer()
	}

	downTrack, err := sfu.NewDownTrack(sfu.DowntrackParams{
		Codecs:                         codecs,
		Source:                         t.params.MediaTrack.Source(),
		Receiver:                       wr,
		BufferFactory:                  sub.GetBufferFactory(),
		SubID:                          subscriberID,
		StreamID:                       streamID,
		MaxTrack:                       maxTrack,
		PlayoutDelayLimit:              sub.GetPlayoutDelayConfig(),
		Pacer:                          sub.GetPacer(),
		Trailer:                        trailer,
		Logger:                         LoggerWithTrack(sub.GetLogger().WithComponent(sutils.ComponentSub), trackID, t.params.IsRelayed),
		RTCPWriter:                     sub.WriteSubscriberRTCP,
		DisableSenderReportPassThrough: sub.GetDisableSenderReportPassThrough(),
		SupportsCodecChange:            sub.SupportsCodecChange(),
	})
	if err != nil {
		return nil, err
	}

	if t.onDownTrackCreated != nil {
		t.onDownTrackCreated(downTrack)
	}

	subTrack := NewSubscribedTrack(SubscribedTrackParams{
		PublisherID:       t.params.MediaTrack.PublisherID(),
		PublisherIdentity: t.params.MediaTrack.PublisherIdentity(),
		PublisherVersion:  t.params.MediaTrack.PublisherVersion(),
		Subscriber:        sub,
		MediaTrack:        t.params.MediaTrack,
		DownTrack:         downTrack,
		AdaptiveStream:    sub.GetAdaptiveStream(),
	})

	if !sub.Hidden() {
		downTrack.OnBindAndConnected(func() {
			t.params.MediaTrack.OnTrackSubscribed()
		})
	}

	// Bind callback can happen from replaceTrack, so set it up early
	var reusingTransceiver atomic.Bool
	var dtState sfu.DownTrackState
	downTrack.OnCodecNegotiated(func(codec webrtc.RTPCodecCapability) {
		if !wr.DetermineReceiver(codec) {
			if t.onSubscriberMaxQualityChange != nil {
				go func() {
					mimeType := mime.NormalizeMimeType(codec.MimeType)
					spatial := buffer.GetSpatialLayerForVideoQuality(
						mimeType,
						livekit.VideoQuality_HIGH,
						t.params.MediaTrack.ToProto(),
					)
					t.onSubscriberMaxQualityChange(downTrack.SubscriberID(), mimeType, spatial)
				}()
			}
		}
	})
	downTrack.OnBinding(func(err error) {
		if err != nil {
			go subTrack.Bound(err)
			return
		}
		if reusingTransceiver.Load() {
			sub.GetLogger().Debugw("seeding downtrack state", "trackID", trackID)
			downTrack.SeedState(dtState)
		}
		if err = wr.AddDownTrack(downTrack); err != nil && err != sfu.ErrReceiverClosed {
			sub.GetLogger().Errorw(
				"could not add down track", err,
				"publisher", subTrack.PublisherIdentity(),
				"publisherID", subTrack.PublisherID(),
				"trackID", trackID,
			)
		}

		go subTrack.Bound(nil)

		subTrack.SetPublisherMuted(t.params.MediaTrack.IsMuted())
	})

	statsKey := telemetry.StatsKeyForTrack(
		sub.GetCountry(),
		livekit.StreamType_DOWNSTREAM,
		subscriberID,
		trackID,
		t.params.MediaTrack.Source(),
		t.params.MediaTrack.Kind(),
	)
	reporter := sub.GetReporter().WithTrack(trackID.String())
	downTrack.OnStatsUpdate(func(_ *sfu.DownTrack, stat *livekit.AnalyticsStat) {
		t.params.Telemetry.TrackStats(statsKey, stat)

		if cs, ok := telemetry.CondenseStat(stat); ok {
			ti := wr.TrackInfo()
			reporter.Tx(func(tx roomobs.TrackTx) {
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
	})

	downTrack.OnMaxLayerChanged(func(dt *sfu.DownTrack, layer int32) {
		if t.onSubscriberMaxQualityChange != nil {
			t.onSubscriberMaxQualityChange(dt.SubscriberID(), dt.Mime(), layer)
		}
	})

	downTrack.OnRttUpdate(func(_ *sfu.DownTrack, rtt uint32) {
		go sub.UpdateMediaRTT(rtt)
	})

	downTrack.AddReceiverReportListener(func(dt *sfu.DownTrack, report *rtcp.ReceiverReport) {
		sub.HandleReceiverReport(dt, report)
	})

	var transceiver *webrtc.RTPTransceiver
	var sender *webrtc.RTPSender

	// try cached RTP senders for a chance to replace track
	var existingTransceiver *webrtc.RTPTransceiver
	replacedTrack := false
	existingTransceiver, dtState = sub.GetCachedDownTrack(trackID)
	if existingTransceiver != nil {
		sub.GetLogger().Debugw(
			"trying to use existing transceiver",
			"publisher", subTrack.PublisherIdentity(),
			"publisherID", subTrack.PublisherID(),
			"trackID", trackID,
		)
		reusingTransceiver.Store(true)
		rtpSender := existingTransceiver.Sender()
		if rtpSender != nil {
			// replaced track will bind immediately without negotiation, SetTransceiver first before bind
			downTrack.SetTransceiver(existingTransceiver)
			err := rtpSender.ReplaceTrack(downTrack)
			if err == nil {
				sender = rtpSender
				transceiver = existingTransceiver
				replacedTrack = true
				sub.GetLogger().Debugw(
					"track replaced",
					"publisher", subTrack.PublisherIdentity(),
					"publisherID", subTrack.PublisherID(),
					"trackID", trackID,
				)
			}
		}

		if !replacedTrack {
			// Could not re-use cached transceiver for this track.
			// Stop the transceiver so that it is at least not active.
			// It is not usable once stopped,
			//
			// Adding down track will create a new transceiver (or re-use
			// an inactive existing one). In either case, a renegotiation
			// will happen and that will notify remote of this stopped
			// transceiver
			existingTransceiver.Stop()
			reusingTransceiver.Store(false)
		}
	}

	// if cannot replace, find an unused transceiver or add new one
	if transceiver == nil {
		info := t.params.MediaTrack.ToProto()
		addTrackParams := types.AddTrackParams{
			Stereo: info.Stereo,
			Red:    !info.DisableRed,
		}
		if addTrackParams.Red && (len(codecs) == 1 && mime.IsMimeTypeStringOpus(codecs[0].MimeType)) {
			addTrackParams.Red = false
		}

		sub.VerifySubscribeParticipantInfo(subTrack.PublisherID(), subTrack.PublisherVersion())
		if sub.SupportsTransceiverReuse() {
			//
			// AddTrack will create a new transceiver or re-use an unused one
			// if the attributes match. This prevents SDP from bloating
			// because of dormant transceivers building up.
			//
			sender, transceiver, err = sub.AddTrackLocal(downTrack, addTrackParams)
			if err != nil {
				return nil, err
			}
		} else {
			sender, transceiver, err = sub.AddTransceiverFromTrackLocal(downTrack, addTrackParams)
			if err != nil {
				return nil, err
			}
		}
	}

	// whether re-using or stopping remove transceiver from cache
	// NOTE: safety net, if somehow a cached transceiver is re-used by a different track
	sub.UncacheDownTrack(transceiver)

	// negotiation isn't required if we've replaced track
	// ONE-SHOT-SIGNALLING-MODE: this should not be needed, but that mode information is not available here,
	//   but it is not detrimental to set this, needs clean up when participants modes are separated out better.
	subTrack.SetNeedsNegotiation(!replacedTrack)
	subTrack.SetRTPSender(sender)

	// it is possible that subscribed track is closed before subscription manager sets
	// the `OnClose` callback. That handler in subscription manager removes the track
	// from the peer connection.
	//
	// But, the subscription could be removed early if the published track is closed
	// while adding subscription. In those cases, subscription manager would not have set
	// the `OnClose` callback. So, set it here to handle cases of early close.
	subTrack.OnClose(func(isExpectedToResume bool) {
		if !isExpectedToResume {
			if err := sub.RemoveTrackLocal(sender); err != nil {
				t.params.Logger.Warnw("could not remove track from peer connection", err)
			}
		}
	})

	downTrack.SetTransceiver(transceiver)

	downTrack.OnCloseHandler(func(isExpectedToResume bool) {
		t.downTrackClosed(subscriberID, sub, subTrack, isExpectedToResume)
	})

	t.subscribedTracksMu.Lock()
	t.subscribedTracks[subscriberID] = subTrack
	t.subscribedTracksMu.Unlock()

	return subTrack, nil
}

// RemoveSubscriber removes participant from subscription
// stop all forwarders to the client
func (t *MediaTrackSubscriptions) RemoveSubscriber(subscriberID livekit.ParticipantID, isExpectedToResume bool) error {
	subTrack := t.getSubscribedTrack(subscriberID)
	if subTrack == nil {
		return errNotFound
	}

	t.params.Logger.Debugw("removing subscriber", "subscriberID", subscriberID, "isExpectedToResume", isExpectedToResume)
	t.closeSubscribedTrack(subTrack, isExpectedToResume)
	return nil
}

func (t *MediaTrackSubscriptions) closeSubscribedTrack(subTrack types.SubscribedTrack, isExpectedToResume bool) {
	dt := subTrack.DownTrack()
	if dt == nil {
		return
	}

	if isExpectedToResume {
		dt.CloseWithFlush(false)
	} else {
		// flushing blocks, avoid blocking when publisher removes all its subscribers
		go dt.CloseWithFlush(true)
	}
}

func (t *MediaTrackSubscriptions) GetAllSubscribers() []livekit.ParticipantID {
	t.subscribedTracksMu.RLock()
	defer t.subscribedTracksMu.RUnlock()

	subs := make([]livekit.ParticipantID, 0, len(t.subscribedTracks))
	for id := range t.subscribedTracks {
		subs = append(subs, id)
	}
	return subs
}

func (t *MediaTrackSubscriptions) GetAllSubscribersForMime(mime mime.MimeType) []livekit.ParticipantID {
	t.subscribedTracksMu.RLock()
	defer t.subscribedTracksMu.RUnlock()

	subs := make([]livekit.ParticipantID, 0, len(t.subscribedTracks))
	for id, subTrack := range t.subscribedTracks {
		if subTrack.DownTrack().Mime() != mime {
			continue
		}

		subs = append(subs, id)
	}
	return subs
}

func (t *MediaTrackSubscriptions) GetNumSubscribers() int {
	t.subscribedTracksMu.RLock()
	defer t.subscribedTracksMu.RUnlock()

	return len(t.subscribedTracks)
}

func (t *MediaTrackSubscriptions) UpdateVideoLayers() {
	for _, st := range t.getAllSubscribedTracks() {
		st.UpdateVideoLayer()
	}
}

func (t *MediaTrackSubscriptions) getSubscribedTrack(subscriberID livekit.ParticipantID) types.SubscribedTrack {
	t.subscribedTracksMu.RLock()
	defer t.subscribedTracksMu.RUnlock()

	return t.subscribedTracks[subscriberID]
}

func (t *MediaTrackSubscriptions) getAllSubscribedTracks() []types.SubscribedTrack {
	t.subscribedTracksMu.RLock()
	defer t.subscribedTracksMu.RUnlock()

	return t.getAllSubscribedTracksLocked()
}

func (t *MediaTrackSubscriptions) getAllSubscribedTracksLocked() []types.SubscribedTrack {
	subTracks := make([]types.SubscribedTrack, 0, len(t.subscribedTracks))
	for _, subTrack := range t.subscribedTracks {
		subTracks = append(subTracks, subTrack)
	}
	return subTracks
}

func (t *MediaTrackSubscriptions) DebugInfo() []map[string]interface{} {
	subscribedTrackInfo := make([]map[string]interface{}, 0)
	for _, val := range t.getAllSubscribedTracks() {
		if st, ok := val.(*SubscribedTrack); ok {
			subscribedTrackInfo = append(subscribedTrackInfo, st.DownTrack().DebugInfo())
		}
	}

	return subscribedTrackInfo
}

func (t *MediaTrackSubscriptions) downTrackClosed(
	subscriberID livekit.ParticipantID,
	sub types.LocalParticipant,
	subTrack types.SubscribedTrack,
	isExpectedToResume bool,
) {
	// Cache transceiver for potential re-use on resume.
	// To ensure subscription manager does not re-subscribe before caching,
	// delete the subscribed track only after caching.
	if isExpectedToResume {
		dt := subTrack.DownTrack()
		if tr := dt.GetTransceiver(); tr != nil {
			sub.CacheDownTrack(subTrack.ID(), tr, dt.GetState())
		}
	}

	go func() {
		t.subscribedTracksMu.Lock()
		delete(t.subscribedTracks, subscriberID)
		t.subscribedTracksMu.Unlock()
		subTrack.Close(isExpectedToResume)
	}()
}

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
	"slices"
	"sync"

	"github.com/livekit/livekit-server/pkg/sfu/mime"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/pion/webrtc/v4"
	"go.uber.org/atomic"

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
	onSubscriberAudioCodecChange func(subscriberID livekit.ParticipantID, mime mime.MimeType, enabled bool)
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

func (t *MediaTrackSubscriptions) OnSubscriberAudioCodecChange(f func(subscriberID livekit.ParticipantID, mime mime.MimeType, enabled bool)) {
	t.onSubscriberAudioCodecChange = f
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

	subTrack, err := NewSubscribedTrack(SubscribedTrackParams{
		ReceiverConfig:     t.params.ReceiverConfig,
		SubscriberConfig:   t.params.SubscriberConfig,
		Subscriber:         sub,
		MediaTrack:         t.params.MediaTrack,
		AdaptiveStream:     sub.GetAdaptiveStream(),
		Telemetry:          t.params.Telemetry,
		WrappedReceiver:    wr,
		IsRelayed:          t.params.IsRelayed,
		OnDownTrackCreated: t.onDownTrackCreated,
		OnDownTrackClosed: func(subscriberID livekit.ParticipantID) {
			t.subscribedTracksMu.Lock()
			delete(t.subscribedTracks, subscriberID)
			t.subscribedTracksMu.Unlock()
		},
		OnSubscriberMaxQualityChange: t.onSubscriberMaxQualityChange,
		OnSubscriberAudioCodecChange: t.onSubscriberAudioCodecChange,
	})
	if err != nil {
		return nil, err
	}

	// Bind callback can happen from replaceTrack, so set it up early
	var reusingTransceiver atomic.Bool
	var dtState sfu.DownTrackState
	downTrack := subTrack.DownTrack()
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
			Stereo: slices.Contains(info.AudioFeatures, livekit.AudioTrackFeature_TF_STEREO),
			Red:    IsRedEnabled(info),
		}
		codecs := wr.Codecs()
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

package rtc

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/bep/debounce"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/rtcerr"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/telemetry"
)

const (
	initialQualityUpdateWait = 10 * time.Second
)

// MediaTrackSubscriptions manages subscriptions of a media track
type MediaTrackSubscriptions struct {
	params MediaTrackSubscriptionsParams

	subscribedTracksMu sync.RWMutex
	subscribedTracks   map[livekit.ParticipantID]types.SubscribedTrack
	pendingClose       map[livekit.ParticipantID]types.SubscribedTrack

	onNoSubscribers func()

	// quality level enable/disable
	maxQualityLock               sync.RWMutex
	maxSubscriberQuality         map[livekit.ParticipantID]livekit.VideoQuality
	maxSubscriberNodeQuality     map[livekit.NodeID]livekit.VideoQuality
	maxSubscribedQuality         livekit.VideoQuality
	maxSubscribedQualityDebounce func(func())
	onSubscribedMaxQualityChange func(subscribedQualities []*livekit.SubscribedQuality, maxSubscribedQuality livekit.VideoQuality)
	maxQualityTimer              *time.Timer
}

type MediaTrackSubscriptionsParams struct {
	MediaTrack types.MediaTrack

	BufferFactory    *buffer.Factory
	ReceiverConfig   ReceiverConfig
	SubscriberConfig DirectionConfig
	VideoConfig      config.VideoConfig

	Telemetry telemetry.TelemetryService

	Logger logger.Logger
}

func NewMediaTrackSubscriptions(params MediaTrackSubscriptionsParams) *MediaTrackSubscriptions {
	t := &MediaTrackSubscriptions{
		params:                       params,
		subscribedTracks:             make(map[livekit.ParticipantID]types.SubscribedTrack),
		pendingClose:                 make(map[livekit.ParticipantID]types.SubscribedTrack),
		maxSubscriberQuality:         make(map[livekit.ParticipantID]livekit.VideoQuality),
		maxSubscriberNodeQuality:     make(map[livekit.NodeID]livekit.VideoQuality),
		maxSubscribedQuality:         livekit.VideoQuality_HIGH,
		maxSubscribedQualityDebounce: debounce.New(params.VideoConfig.DynacastPauseDelay),
	}

	return t
}

func (t *MediaTrackSubscriptions) Start() {
	t.startMaxQualityTimer(false)
}

func (t *MediaTrackSubscriptions) Restart() {
	t.startMaxQualityTimer(true)
}

func (t *MediaTrackSubscriptions) Close() {
	t.stopMaxQualityTimer()
}

func (t *MediaTrackSubscriptions) OnNoSubscribers(f func()) {
	t.onNoSubscribers = f
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
func (t *MediaTrackSubscriptions) AddSubscriber(sub types.LocalParticipant, codec webrtc.RTPCodecCapability, wr WrappedReceiver) (*sfu.DownTrack, error) {
	trackID := t.params.MediaTrack.ID()
	subscriberID := sub.ID()

	// don't subscribe to the same track multiple times
	t.subscribedTracksMu.Lock()
	if _, ok := t.subscribedTracks[subscriberID]; ok {
		t.subscribedTracksMu.Unlock()
		return nil, nil
	}
	t.subscribedTracksMu.Unlock()

	var rtcpFeedback []webrtc.RTCPFeedback
	switch t.params.MediaTrack.Kind() {
	case livekit.TrackType_AUDIO:
		rtcpFeedback = t.params.SubscriberConfig.RTCPFeedback.Audio
	case livekit.TrackType_VIDEO:
		rtcpFeedback = t.params.SubscriberConfig.RTCPFeedback.Video
	}
	downTrack, err := sfu.NewDownTrack(
		webrtc.RTPCodecCapability{
			MimeType:     codec.MimeType,
			ClockRate:    codec.ClockRate,
			Channels:     codec.Channels,
			SDPFmtpLine:  codec.SDPFmtpLine,
			RTCPFeedback: rtcpFeedback,
		},
		wr,
		t.params.BufferFactory,
		subscriberID,
		t.params.ReceiverConfig.PacketBufferSize,
		LoggerWithTrack(sub.GetLogger(), trackID),
	)
	if err != nil {
		return nil, err
	}

	subTrack := NewSubscribedTrack(SubscribedTrackParams{
		PublisherID:       t.params.MediaTrack.PublisherID(),
		PublisherIdentity: t.params.MediaTrack.PublisherIdentity(),
		SubscriberID:      subscriberID,
		MediaTrack:        t.params.MediaTrack,
		DownTrack:         downTrack,
		AdaptiveStream:    sub.GetAdaptiveStream(),
	})

	var transceiver *webrtc.RTPTransceiver
	var sender *webrtc.RTPSender
	if sub.ProtocolVersion().SupportsTransceiverReuse() {
		//
		// AddTrack will create a new transceiver or re-use an unused one
		// if the attributes match. This prevents SDP from bloating
		// because of dormant transceivers building up.
		//
		sender, err = sub.SubscriberPC().AddTrack(downTrack)
		if err != nil {
			return nil, err
		}

		// as there is no way to get transceiver from sender, search
		for _, tr := range sub.SubscriberPC().GetTransceivers() {
			if tr.Sender() == sender {
				transceiver = tr
				break
			}
		}
		if transceiver == nil {
			// cannot add, no transceiver
			return nil, errors.New("cannot subscribe without a transceiver in place")
		}
	} else {
		transceiver, err = sub.SubscriberPC().AddTransceiverFromTrack(downTrack, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionSendonly,
		})
		if err != nil {
			return nil, err
		}

		sender = transceiver.Sender()
		if sender == nil {
			// cannot add, no sender
			return nil, errors.New("cannot subscribe without a sender in place")
		}
	}

	sendParameters := sender.GetParameters()
	downTrack.SetRTPHeaderExtensions(sendParameters.HeaderExtensions)

	downTrack.SetTransceiver(transceiver)

	// when out track is bound, start loop to send reports
	downTrack.OnBind(func() {
		go subTrack.Bound()
		go t.sendDownTrackBindingReports(sub)
	})

	downTrack.OnStatsUpdate(func(_ *sfu.DownTrack, stat *livekit.AnalyticsStat) {
		t.params.Telemetry.TrackStats(livekit.StreamType_DOWNSTREAM, subscriberID, trackID, stat)
	})

	downTrack.OnMaxLayerChanged(func(dt *sfu.DownTrack, layer int32) {
		t.notifySubscriberMaxQuality(subscriberID, QualityForSpatialLayer(layer))
	})

	downTrack.OnRttUpdate(func(_ *sfu.DownTrack, rtt uint32) {
		go sub.UpdateRTT(rtt)
	})

	downTrack.OnCloseHandler(func() {
		t.subscribedTracksMu.Lock()
		delete(t.subscribedTracks, subscriberID)
		delete(t.pendingClose, subscriberID)
		t.subscribedTracksMu.Unlock()

		t.maybeNotifyNoSubscribers()

		t.params.Telemetry.TrackUnsubscribed(context.Background(), subscriberID, t.params.MediaTrack.ToProto())

		// ignore if the subscribing sub is not connected
		if sub.SubscriberPC().ConnectionState() == webrtc.PeerConnectionStateClosed {
			return
		}

		// if the source has been terminated, we'll need to terminate all the subscribed tracks
		// however, if the dest sub has disconnected, then we can skip
		if sender == nil {
			return
		}
		t.params.Logger.Debugw("removing peerconnection track",
			"subscriber", sub.Identity(),
			"subscriberID", subscriberID,
			"kind", t.params.MediaTrack.Kind(),
		)
		if err := sub.SubscriberPC().RemoveTrack(sender); err != nil {
			if err == webrtc.ErrConnectionClosed {
				// sub closing, can skip removing subscribedtracks
				return
			}
			if _, ok := err.(*rtcerr.InvalidStateError); !ok {
				// most of these are safe to ignore, since the track state might have already
				// been set to Inactive
				t.params.Logger.Debugw("could not remove remoteTrack from forwarder",
					"error", err,
					"subscriber", sub.Identity(),
					"subscriberID", subscriberID,
				)
			}
		}

		sub.RemoveSubscribedTrack(subTrack)
		sub.Negotiate()
	})

	t.subscribedTracksMu.Lock()
	t.subscribedTracks[subscriberID] = subTrack
	t.subscribedTracksMu.Unlock()
	subTrack.SetPublisherMuted(t.params.MediaTrack.IsMuted())

	// since sub will lock, run it in a goroutine to avoid deadlocks
	go func() {
		sub.AddSubscribedTrack(subTrack)
		sub.Negotiate()
	}()

	// initialize to default layer
	t.notifySubscriberMaxQuality(subscriberID, livekit.VideoQuality_HIGH)
	t.params.Telemetry.TrackSubscribed(context.Background(), subscriberID, t.params.MediaTrack.ToProto(),
		&livekit.ParticipantInfo{Sid: string(t.params.MediaTrack.PublisherID()), Identity: string(t.params.MediaTrack.PublisherIdentity())})
	return downTrack, nil
}

// RemoveSubscriber removes participant from subscription
// stop all forwarders to the client
func (t *MediaTrackSubscriptions) RemoveSubscriber(participantID livekit.ParticipantID, resume bool) {
	subTrack := t.getSubscribedTrack(participantID)

	t.subscribedTracksMu.Lock()
	delete(t.subscribedTracks, participantID)
	if subTrack != nil {
		t.pendingClose[participantID] = subTrack
	}
	t.subscribedTracksMu.Unlock()

	if subTrack != nil {
		subTrack.DownTrack().CloseWithFlush(!resume)
	}
}

func (t *MediaTrackSubscriptions) RemoveAllSubscribers() {
	t.params.Logger.Debugw("removing all subscribers")

	t.subscribedTracksMu.Lock()
	subscribedTracks := t.getAllSubscribedTracksLocked()
	t.subscribedTracks = make(map[livekit.ParticipantID]types.SubscribedTrack)

	for _, subTrack := range subscribedTracks {
		t.pendingClose[subTrack.SubscriberID()] = subTrack
	}
	t.subscribedTracksMu.Unlock()

	for _, subTrack := range subscribedTracks {
		subTrack.DownTrack().Close()
	}
}

func (t *MediaTrackSubscriptions) ResyncAllSubscribers() {
	t.params.Logger.Debugw("resyncing all subscribers")

	for _, subTrack := range t.getAllSubscribedTracks() {
		subTrack.DownTrack().Resync()
	}
}

func (t *MediaTrackSubscriptions) RevokeDisallowedSubscribers(allowedSubscriberIDs []livekit.ParticipantID) []livekit.ParticipantID {
	var revokedSubscriberIDs []livekit.ParticipantID

	// LK-TODO: large number of subscribers needs to be solved for this loop
	for _, subTrack := range t.getAllSubscribedTracks() {
		found := false
		for _, allowedID := range allowedSubscriberIDs {
			if subTrack.SubscriberID() == allowedID {
				found = true
				break
			}
		}

		if !found {
			go subTrack.DownTrack().Close()
			revokedSubscriberIDs = append(revokedSubscriberIDs, subTrack.SubscriberID())
		}
	}

	return revokedSubscriberIDs
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

// TODO: send for all down tracks from the source participant
// https://tools.ietf.org/html/rfc7941
func (t *MediaTrackSubscriptions) sendDownTrackBindingReports(sub types.LocalParticipant) {
	var sd []rtcp.SourceDescriptionChunk

	subTrack := t.getSubscribedTrack(sub.ID())
	if subTrack == nil {
		return
	}

	chunks := subTrack.DownTrack().CreateSourceDescriptionChunks()
	if chunks == nil {
		return
	}
	sd = append(sd, chunks...)

	pkts := []rtcp.Packet{
		&rtcp.SourceDescription{Chunks: sd},
	}

	go func() {
		defer RecoverSilent()
		batch := pkts
		i := 0
		for {
			if err := sub.SubscriberPC().WriteRTCP(batch); err != nil {
				t.params.Logger.Errorw("could not write RTCP", err)
				return
			}
			if i > 5 {
				return
			}
			i++
			time.Sleep(20 * time.Millisecond)
		}
	}()
}

func (t *MediaTrackSubscriptions) DebugInfo() []map[string]interface{} {
	subscribedTrackInfo := make([]map[string]interface{}, 0)
	for _, val := range t.getAllSubscribedTracks() {
		if st, ok := val.(*SubscribedTrack); ok {
			dt := st.DownTrack().DebugInfo()
			dt["PubMuted"] = st.pubMuted.Load()
			dt["SubMuted"] = st.subMuted.Load()
			subscribedTrackInfo = append(subscribedTrackInfo, dt)
		}
	}

	return subscribedTrackInfo
}

func (t *MediaTrackSubscriptions) OnSubscribedMaxQualityChange(f func(subscribedQualities []*livekit.SubscribedQuality, maxSubscribedQuality livekit.VideoQuality)) {
	t.onSubscribedMaxQualityChange = f
}

func (t *MediaTrackSubscriptions) notifySubscriberMaxQuality(subscriberID livekit.ParticipantID, quality livekit.VideoQuality) {
	if t.params.MediaTrack.Kind() != livekit.TrackType_VIDEO {
		return
	}

	t.maxQualityLock.Lock()
	if quality == livekit.VideoQuality_OFF {
		_, ok := t.maxSubscriberQuality[subscriberID]
		if !ok {
			t.maxQualityLock.Unlock()
			return
		}

		delete(t.maxSubscriberQuality, subscriberID)
	} else {
		maxQuality, ok := t.maxSubscriberQuality[subscriberID]
		if ok && maxQuality == quality {
			t.maxQualityLock.Unlock()
			return
		}

		t.maxSubscriberQuality[subscriberID] = quality
	}
	t.maxQualityLock.Unlock()

	t.UpdateQualityChange(false)
}

func (t *MediaTrackSubscriptions) NotifySubscriberNodeMaxQuality(nodeID livekit.NodeID, quality livekit.VideoQuality) {
	if t.params.MediaTrack.Kind() != livekit.TrackType_VIDEO {
		return
	}

	t.maxQualityLock.Lock()
	if quality == livekit.VideoQuality_OFF {
		_, ok := t.maxSubscriberNodeQuality[nodeID]
		if !ok {
			t.maxQualityLock.Unlock()
			return
		}

		delete(t.maxSubscriberNodeQuality, nodeID)
	} else {
		maxQuality, ok := t.maxSubscriberNodeQuality[nodeID]
		if ok && maxQuality == quality {
			t.maxQualityLock.Unlock()
			return
		}

		t.maxSubscriberNodeQuality[nodeID] = quality
	}
	t.maxQualityLock.Unlock()

	t.UpdateQualityChange(false)
}

func (t *MediaTrackSubscriptions) UpdateQualityChange(force bool) {
	if t.params.MediaTrack.Kind() != livekit.TrackType_VIDEO {
		return
	}

	t.maxQualityLock.Lock()
	maxSubscribedQuality := livekit.VideoQuality_OFF
	for _, subQuality := range t.maxSubscriberQuality {
		if maxSubscribedQuality == livekit.VideoQuality_OFF || subQuality > maxSubscribedQuality {
			maxSubscribedQuality = subQuality
		}
	}

	for _, subQuality := range t.maxSubscriberNodeQuality {
		if maxSubscribedQuality == livekit.VideoQuality_OFF || subQuality > maxSubscribedQuality {
			maxSubscribedQuality = subQuality
		}
	}

	if maxSubscribedQuality == t.maxSubscribedQuality && !force {
		t.maxQualityLock.Unlock()
		return
	}

	// if quality comes down(or become OFF), delay notify to publisher
	if (t.maxSubscribedQuality != livekit.VideoQuality_OFF) &&
		(t.maxSubscribedQuality > maxSubscribedQuality || maxSubscribedQuality == livekit.VideoQuality_OFF) &&
		t.params.VideoConfig.DynacastPauseDelay > 0 && !force {

		t.params.Logger.Debugw("throttle quality change", "from", t.maxSubscribedQuality, "to", maxSubscribedQuality)
		t.maxQualityLock.Unlock()
		t.maxSubscribedQualityDebounce(func() {
			t.UpdateQualityChange(true)
		})
		return
	}

	t.maxSubscribedQuality = maxSubscribedQuality

	var subscribedQualities []*livekit.SubscribedQuality
	if t.maxSubscribedQuality == livekit.VideoQuality_OFF {
		subscribedQualities = []*livekit.SubscribedQuality{
			{Quality: livekit.VideoQuality_LOW, Enabled: false},
			{Quality: livekit.VideoQuality_MEDIUM, Enabled: false},
			{Quality: livekit.VideoQuality_HIGH, Enabled: false},
		}
	} else {
		for q := livekit.VideoQuality_LOW; q <= livekit.VideoQuality_HIGH; q++ {
			subscribedQualities = append(subscribedQualities, &livekit.SubscribedQuality{
				Quality: q,
				Enabled: q <= t.maxSubscribedQuality,
			})
		}
	}
	t.maxQualityLock.Unlock()

	if t.onSubscribedMaxQualityChange != nil {
		t.onSubscribedMaxQualityChange(subscribedQualities, maxSubscribedQuality)
	}
}

func (t *MediaTrackSubscriptions) startMaxQualityTimer(force bool) {
	t.maxQualityLock.Lock()
	defer t.maxQualityLock.Unlock()

	if t.params.MediaTrack.Kind() != livekit.TrackType_VIDEO {
		return
	}

	t.maxQualityTimer = time.AfterFunc(initialQualityUpdateWait, func() {
		t.stopMaxQualityTimer()
		t.UpdateQualityChange(force)
	})
}

func (t *MediaTrackSubscriptions) stopMaxQualityTimer() {
	t.maxQualityLock.Lock()
	defer t.maxQualityLock.Unlock()

	if t.maxQualityTimer != nil {
		t.maxQualityTimer.Stop()
		t.maxQualityTimer = nil
	}
}

func (t *MediaTrackSubscriptions) maybeNotifyNoSubscribers() {
	if t.onNoSubscribers == nil {
		return
	}

	t.subscribedTracksMu.RLock()
	empty := len(t.subscribedTracks) == 0 && len(t.pendingClose) == 0
	t.subscribedTracksMu.RUnlock()

	if empty {
		t.onNoSubscribers()
	}
}

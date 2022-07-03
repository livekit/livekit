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
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/livekit-server/pkg/utils"
)

const (
	initialQualityUpdateWait = 10 * time.Second
)

var (
	errAlreadySubscribed = errors.New("already subscribed")
	errNoTransceiver     = errors.New("cannot subscribe without a transceiver in place")
	errNoSender          = errors.New("cannot subscribe without a sender in place")
	errNotFound          = errors.New("not found")
)

type SubscribeRequestType int

const (
	SubscribeRequestTypeRemove SubscribeRequestType = iota
	SubscribeRequestTypeAdd
)

type SubscribeRequest struct {
	requestType   SubscribeRequestType
	sub           types.LocalParticipant
	wr            *WrappedReceiver
	willBeResumed bool
}

// MediaTrackSubscriptions manages subscriptions of a media track
type MediaTrackSubscriptions struct {
	params MediaTrackSubscriptionsParams

	subscribedTracksMu sync.RWMutex
	subscribedTracks   map[livekit.ParticipantID]types.SubscribedTrack
	inProgress         map[livekit.ParticipantID]bool
	requestsQueue      map[livekit.ParticipantID][]SubscribeRequest

	onNoSubscribers func()

	// quality level enable/disable
	maxQualityLock               sync.RWMutex
	maxSubscriberQuality         map[livekit.ParticipantID]*types.SubscribedCodecQuality
	maxSubscriberNodeQuality     map[livekit.NodeID][]types.SubscribedCodecQuality
	maxSubscribedQuality         map[string]livekit.VideoQuality // codec mime -> quality
	maxSubscribedQualityDebounce func(func())
	onSubscribedMaxQualityChange func(subscribedQualities []*livekit.SubscribedCodec, maxSubscribedQualities []types.SubscribedCodecQuality)
	maxQualityTimer              *time.Timer

	qualityNotifyOpQueue *utils.OpsQueue

	onDownTrackCreated func(downTrack *sfu.DownTrack)
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
		inProgress:                   make(map[livekit.ParticipantID]bool),
		requestsQueue:                make(map[livekit.ParticipantID][]SubscribeRequest),
		maxSubscriberQuality:         make(map[livekit.ParticipantID]*types.SubscribedCodecQuality),
		maxSubscriberNodeQuality:     make(map[livekit.NodeID][]types.SubscribedCodecQuality),
		maxSubscribedQuality:         make(map[string]livekit.VideoQuality),
		maxSubscribedQualityDebounce: debounce.New(params.VideoConfig.DynacastPauseDelay),
		qualityNotifyOpQueue:         utils.NewOpsQueue(params.Logger, "quality-notify", 100),
	}

	return t
}

func (t *MediaTrackSubscriptions) Start() {
	t.qualityNotifyOpQueue.Start()
	t.startMaxQualityTimer(false)
}

func (t *MediaTrackSubscriptions) Restart() {
	t.startMaxQualityTimer(true)
}

func (t *MediaTrackSubscriptions) Stop() {
	t.stopMaxQualityTimer()
}

func (t *MediaTrackSubscriptions) Close() {
	t.qualityNotifyOpQueue.Stop()
}

func (t *MediaTrackSubscriptions) OnNoSubscribers(f func()) {
	t.onNoSubscribers = f
}

func (t *MediaTrackSubscriptions) OnDownTrackCreated(f func(downTrack *sfu.DownTrack)) {
	t.onDownTrackCreated = f
}

func (t *MediaTrackSubscriptions) SetMuted(muted bool) {
	// update quality based on subscription if unmuting.
	// This will queue up the current state, but subscriber
	// driven changes could update it.
	if !muted {
		t.UpdateQualityChange(true)
	}

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

func (t *MediaTrackSubscriptions) AddCodec(mime string) {
	t.subscribedTracksMu.Lock()
	t.maxSubscribedQuality[mime] = livekit.VideoQuality_HIGH
	t.subscribedTracksMu.Unlock()
}

func (t *MediaTrackSubscriptions) processRequestsQueue(subscriberID livekit.ParticipantID) {
	t.subscribedTracksMu.Lock()
	if t.inProgress[subscriberID] || len(t.requestsQueue[subscriberID]) == 0 {
		t.subscribedTracksMu.Unlock()
		return
	}

	request := t.requestsQueue[subscriberID][0]
	t.requestsQueue[subscriberID] = t.requestsQueue[subscriberID][1:]
	if len(t.requestsQueue[subscriberID]) == 0 {
		delete(t.requestsQueue, subscriberID)
	}

	t.inProgress[subscriberID] = true
	t.subscribedTracksMu.Unlock()

	switch request.requestType {
	case SubscribeRequestTypeAdd:
		err := t.addSubscriber(request.sub, request.wr)
		if err != nil {
			if err != errAlreadySubscribed {
				t.params.Logger.Errorw("error adding subscriber", err, "subscriberID", subscriberID)
			}

			// process pending request even if adding errors out
			go t.clearInProgressAndProcessRequestsQueue(subscriberID)
		}

	case SubscribeRequestTypeRemove:
		err := t.removeSubscriber(subscriberID, request.willBeResumed)
		if err != nil {
			go t.clearInProgressAndProcessRequestsQueue(subscriberID)
		}

	default:
		t.params.Logger.Warnw("unknown request type", nil)

		// let the queue move forward
		go t.clearInProgressAndProcessRequestsQueue(subscriberID)
	}
}

// AddSubscriber subscribes sub to current mediaTrack
func (t *MediaTrackSubscriptions) AddSubscriber(sub types.LocalParticipant, wr *WrappedReceiver) error {
	subscriberID := sub.ID()
	t.subscribedTracksMu.Lock()
	t.requestsQueue[subscriberID] = append(t.requestsQueue[subscriberID], SubscribeRequest{
		requestType: SubscribeRequestTypeAdd,
		sub:         sub,
		wr:          wr,
	})
	t.subscribedTracksMu.Unlock()

	t.processRequestsQueue(subscriberID)
	return nil
}

func (t *MediaTrackSubscriptions) addSubscriber(sub types.LocalParticipant, wr *WrappedReceiver) error {
	trackID := t.params.MediaTrack.ID()
	subscriberID := sub.ID()

	// don't subscribe to the same track multiple times
	t.subscribedTracksMu.Lock()
	if _, ok := t.subscribedTracks[subscriberID]; ok {
		t.subscribedTracksMu.Unlock()
		return errAlreadySubscribed
	}
	t.subscribedTracksMu.Unlock()

	var rtcpFeedback []webrtc.RTCPFeedback
	switch t.params.MediaTrack.Kind() {
	case livekit.TrackType_AUDIO:
		rtcpFeedback = t.params.SubscriberConfig.RTCPFeedback.Audio
	case livekit.TrackType_VIDEO:
		rtcpFeedback = t.params.SubscriberConfig.RTCPFeedback.Video
	}
	codecs := wr.Codecs()
	for _, c := range codecs {
		c.RTCPFeedback = rtcpFeedback
	}
	downTrack, err := sfu.NewDownTrack(
		codecs,
		wr,
		t.params.BufferFactory,
		subscriberID,
		t.params.ReceiverConfig.PacketBufferSize,
		LoggerWithTrack(sub.GetLogger(), trackID),
	)
	if err != nil {
		return err
	}

	if t.onDownTrackCreated != nil {
		t.onDownTrackCreated(downTrack)
	}

	subTrack := NewSubscribedTrack(SubscribedTrackParams{
		PublisherID:       t.params.MediaTrack.PublisherID(),
		PublisherIdentity: t.params.MediaTrack.PublisherIdentity(),
		Subscriber:        sub,
		MediaTrack:        t.params.MediaTrack,
		DownTrack:         downTrack,
		AdaptiveStream:    sub.GetAdaptiveStream(),
	})

	// Bind callback can happen from replaceTrack, so set it up early
	var reusingTransceiver atomic.Bool
	var forwarderState sfu.ForwarderState
	downTrack.OnBind(func() {
		wr.DetermineReceiver(downTrack.Codec())
		if reusingTransceiver.Load() {
			downTrack.SeedForwarderState(forwarderState)
		}
		if err = wr.AddDownTrack(downTrack); err != nil {
			sub.GetLogger().Errorw(
				"could not add down track", err,
				"publisher", subTrack.PublisherIdentity(),
				"publisherID", subTrack.PublisherID(),
				"trackID", trackID,
			)
		}

		go subTrack.Bound()

		// when down track is bound, start loop to send reports
		go t.sendDownTrackBindingReports(sub)

		// initialize to default layer
		t.notifySubscriberMaxQuality(subscriberID, downTrack.Codec(), livekit.VideoQuality_HIGH)
		subTrack.SetPublisherMuted(t.params.MediaTrack.IsMuted())
	})

	var transceiver *webrtc.RTPTransceiver
	var sender *webrtc.RTPSender

	// try cached RTP senders for a chance to replace track
	var existingTransceiver *webrtc.RTPTransceiver
	replacedTrack := false
	existingTransceiver, forwarderState = sub.GetCachedDownTrack(trackID)
	if existingTransceiver != nil {
		reusingTransceiver.Store(true)
		rtpSender := existingTransceiver.Sender()
		if rtpSender != nil {
			err := rtpSender.ReplaceTrack(downTrack)
			if err == nil {
				sender = rtpSender
				transceiver = existingTransceiver
				replacedTrack = true
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
		}
	}
	reusingTransceiver.Store(false)

	// if cannot replace, find an unused transceiver or add new one
	if transceiver == nil {
		if sub.ProtocolVersion().SupportsTransceiverReuse() {
			//
			// AddTrack will create a new transceiver or re-use an unused one
			// if the attributes match. This prevents SDP from bloating
			// because of dormant transceivers building up.
			//
			sender, err = sub.SubscriberPC().AddTrack(downTrack)
			if err != nil {
				return err
			}

			// as there is no way to get transceiver from sender, search
			for _, tr := range sub.SubscriberPC().GetTransceivers() {
				if tr.Sender() == sender {
					transceiver = tr
					break
				}
			}
		} else {
			transceiver, err = sub.SubscriberPC().AddTransceiverFromTrack(downTrack, webrtc.RTPTransceiverInit{
				Direction: webrtc.RTPTransceiverDirectionSendonly,
			})
			if err != nil {
				return err
			}

			sender = transceiver.Sender()
		}
	}
	if transceiver == nil {
		// cannot add, no transceiver
		return errNoTransceiver
	}
	if sender == nil {
		// cannot add, no sender
		return errNoSender
	}

	// wthether re-using or stopping remove transceiver from cache
	// NOTE: safety net, if somehow a cached transceiver is re-used by a different track
	sub.UncacheDownTrack(transceiver)

	sendParameters := sender.GetParameters()
	downTrack.SetRTPHeaderExtensions(sendParameters.HeaderExtensions)

	downTrack.SetTransceiver(transceiver)

	downTrack.OnStatsUpdate(func(_ *sfu.DownTrack, stat *livekit.AnalyticsStat) {
		t.params.Telemetry.TrackStats(livekit.StreamType_DOWNSTREAM, subscriberID, trackID, stat)
	})

	downTrack.OnMaxLayerChanged(func(dt *sfu.DownTrack, layer int32) {
		go t.notifySubscriberMaxQuality(subscriberID, dt.Codec(), utils.QualityForSpatialLayer(layer))
	})

	downTrack.OnRttUpdate(func(_ *sfu.DownTrack, rtt uint32) {
		go sub.UpdateRTT(rtt)
	})

	downTrack.OnCloseHandler(func(willBeResumed bool) {
		go t.downTrackClosed(sub, subTrack, willBeResumed, sender)
	})

	t.subscribedTracksMu.Lock()
	t.subscribedTracks[subscriberID] = subTrack
	t.subscribedTracksMu.Unlock()

	// since sub will lock, run it in a goroutine to avoid deadlocks
	go func() {
		sub.AddSubscribedTrack(subTrack)
		if !replacedTrack {
			sub.Negotiate(false)
		}

		t.clearInProgressAndProcessRequestsQueue(subscriberID)
	}()

	t.params.Telemetry.TrackSubscribed(
		context.Background(),
		subscriberID,
		t.params.MediaTrack.ToProto(),
		&livekit.ParticipantInfo{
			Sid:      string(t.params.MediaTrack.PublisherID()),
			Identity: string(t.params.MediaTrack.PublisherIdentity()),
		},
	)
	return nil
}

// RemoveSubscriber removes participant from subscription
// stop all forwarders to the client
func (t *MediaTrackSubscriptions) RemoveSubscriber(subscriberID livekit.ParticipantID, willBeResumed bool) {
	t.subscribedTracksMu.Lock()
	t.requestsQueue[subscriberID] = append(t.requestsQueue[subscriberID], SubscribeRequest{
		requestType:   SubscribeRequestTypeRemove,
		willBeResumed: willBeResumed,
	})
	t.subscribedTracksMu.Unlock()

	t.processRequestsQueue(subscriberID)
}

func (t *MediaTrackSubscriptions) removeSubscriber(subscriberID livekit.ParticipantID, willBeResumed bool) error {
	t.params.Logger.Debugw("removing subscriber", "subscriberID", subscriberID, "willBeResumed", willBeResumed)
	subTrack := t.getSubscribedTrack(subscriberID)
	if subTrack == nil {
		return errNotFound
	}

	t.closeSubscribedTrack(subTrack, willBeResumed)
	return nil
}

func (t *MediaTrackSubscriptions) RemoveAllSubscribers(willBeResumed bool) {
	t.params.Logger.Debugw("removing all subscribers")

	var subIDs []livekit.ParticipantID
	t.subscribedTracksMu.Lock()
	for _, subTrack := range t.getAllSubscribedTracksLocked() {
		subscriberID := subTrack.SubscriberID()
		t.requestsQueue[subscriberID] = append(t.requestsQueue[subscriberID], SubscribeRequest{
			requestType:   SubscribeRequestTypeRemove,
			willBeResumed: willBeResumed,
		})

		subIDs = append(subIDs, subscriberID)
	}
	t.subscribedTracksMu.Unlock()

	for _, subID := range subIDs {
		t.processRequestsQueue(subID)
	}

	t.maybeNotifyNoSubscribers()
}

func (t *MediaTrackSubscriptions) closeSubscribedTrack(subTrack types.SubscribedTrack, willBeResumed bool) {
	dt := subTrack.DownTrack()
	if dt == nil {
		return
	}

	dt.CloseWithFlush(!willBeResumed)

	if willBeResumed {
		tr := dt.GetTransceiver()
		if tr != nil {
			sub := subTrack.Subscriber()
			sub.CacheDownTrack(subTrack.ID(), tr, dt.GetForwarderState())
		}
	}
}

func (t *MediaTrackSubscriptions) ResyncAllSubscribers() {
	t.params.Logger.Debugw("resyncing all subscribers")

	for _, subTrack := range t.getAllSubscribedTracks() {
		subTrack.DownTrack().Resync()
	}
}

func (t *MediaTrackSubscriptions) RevokeDisallowedSubscribers(allowedSubscriberIdentities []livekit.ParticipantIdentity) []livekit.ParticipantIdentity {
	var revokedSubscriberIdentities []livekit.ParticipantIdentity

	// LK-TODO: large number of subscribers needs to be solved for this loop
	for _, subTrack := range t.getAllSubscribedTracks() {
		found := false
		for _, allowedIdentity := range allowedSubscriberIdentities {
			if subTrack.SubscriberIdentity() == allowedIdentity {
				found = true
				break
			}
		}

		if !found {
			t.params.Logger.Infow("revoking subscription",
				"subscriber", subTrack.SubscriberIdentity(),
				"subscriberID", subTrack.SubscriberID(),
			)
			t.RemoveSubscriber(subTrack.SubscriberID(), false)
			revokedSubscriberIdentities = append(revokedSubscriberIdentities, subTrack.SubscriberIdentity())
		}
	}

	return revokedSubscriberIdentities
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
				sub.GetLogger().Errorw("could not write RTCP", err)
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

func (t *MediaTrackSubscriptions) OnSubscribedMaxQualityChange(f func(subscribedQualities []*livekit.SubscribedCodec, maxSubscribedQualities []types.SubscribedCodecQuality)) {
	t.onSubscribedMaxQualityChange = f
}

func (t *MediaTrackSubscriptions) notifySubscriberMaxQuality(subscriberID livekit.ParticipantID, codec webrtc.RTPCodecCapability, quality livekit.VideoQuality) {
	if t.params.MediaTrack.Kind() != livekit.TrackType_VIDEO {
		return
	}
	t.params.Logger.Debugw("notifying subscriber max quality", "subscriberID", subscriberID, "codec", codec, "quality", quality)

	if codec.MimeType == "" {
		t.params.Logger.Errorw("codec mime type is empty", nil)
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
		if ok {
			if maxQuality.Quality == quality && maxQuality.CodecMime == codec.MimeType {
				t.maxQualityLock.Unlock()
				return
			}
			maxQuality.CodecMime = codec.MimeType
			maxQuality.Quality = quality
		} else {
			t.maxSubscriberQuality[subscriberID] = &types.SubscribedCodecQuality{
				Quality:   quality,
				CodecMime: codec.MimeType,
			}
		}
	}
	t.maxQualityLock.Unlock()

	t.UpdateQualityChange(false)
}

func (t *MediaTrackSubscriptions) NotifySubscriberNodeMaxQuality(nodeID livekit.NodeID, qualities []types.SubscribedCodecQuality) {
	if t.params.MediaTrack.Kind() != livekit.TrackType_VIDEO {
		return
	}

	if len(qualities) == 1 && qualities[0].CodecMime == "" {
		// for old version msg don't have codec mime, use first mime type
		t.maxQualityLock.RLock()
		for mime := range t.maxSubscribedQuality {
			qualities[0].CodecMime = mime
			break
		}
		t.maxQualityLock.RUnlock()
	}

	t.maxQualityLock.Lock()
	if len(qualities) == 0 {
		if _, ok := t.maxSubscriberNodeQuality[nodeID]; !ok {
			t.maxQualityLock.Unlock()
			return
		}
		delete(t.maxSubscriberNodeQuality, nodeID)
	} else {
		if maxQualities, ok := t.maxSubscriberNodeQuality[nodeID]; ok {
			var matchCounter int
			for _, quality := range qualities {
				for _, maxQuality := range maxQualities {
					if quality == maxQuality {
						matchCounter++
						break
					}
				}
			}

			if matchCounter == len(qualities) && matchCounter == len(maxQualities) {
				t.maxQualityLock.Unlock()
				return
			}
		}
		t.maxSubscriberNodeQuality[nodeID] = qualities
	}
	t.maxQualityLock.Unlock()

	t.UpdateQualityChange(false)
}

func (t *MediaTrackSubscriptions) UpdateQualityChange(force bool) {
	if t.params.MediaTrack.Kind() != livekit.TrackType_VIDEO {
		return
	}

	t.maxQualityLock.Lock()
	t.params.Logger.Debugw("updating quality change",
		"force", force,
		"maxSubscriberQuality", t.maxSubscriberQuality,
		"maxSubscriberNodeQuality", t.maxSubscriberNodeQuality,
		"maxSubscribedQuality", t.maxSubscribedQuality)

	maxSubscribedQuality := make(map[string]livekit.VideoQuality, len(t.maxSubscribedQuality))
	var changed bool
	// reset maxSubscribedQuality
	for mime := range t.maxSubscribedQuality {
		maxSubscribedQuality[mime] = livekit.VideoQuality_OFF
	}

	for _, subQuality := range t.maxSubscriberQuality {
		if q, ok := maxSubscribedQuality[subQuality.CodecMime]; ok {
			if q == livekit.VideoQuality_OFF || (subQuality.Quality != livekit.VideoQuality_OFF && subQuality.Quality > q) {
				maxSubscribedQuality[subQuality.CodecMime] = subQuality.Quality
			}
		} else {
			maxSubscribedQuality[subQuality.CodecMime] = subQuality.Quality
		}
	}
	for _, subQualities := range t.maxSubscriberNodeQuality {
		for _, subQuality := range subQualities {
			if q, ok := maxSubscribedQuality[subQuality.CodecMime]; ok {
				if q == livekit.VideoQuality_OFF || (subQuality.Quality != livekit.VideoQuality_OFF && subQuality.Quality > q) {
					maxSubscribedQuality[subQuality.CodecMime] = subQuality.Quality
				}
			} else {
				maxSubscribedQuality[subQuality.CodecMime] = subQuality.Quality
			}
		}
	}

	qualityDowngrades := make(map[string]livekit.VideoQuality, len(t.maxSubscribedQuality))
	noChangeCount := 0
	for mime, q := range maxSubscribedQuality {
		origin, ok := t.maxSubscribedQuality[mime]
		if !ok {
			origin = livekit.VideoQuality_OFF
		}
		if origin != q {
			if q == livekit.VideoQuality_OFF || (origin != livekit.VideoQuality_OFF && origin > q) {
				// quality downgrade (or become off), delay notify to publisher
				qualityDowngrades[mime] = origin
				if force {
					t.maxSubscribedQuality[mime] = q
				}
			} else {
				// quality upgrade, update immediately
				t.maxSubscribedQuality[mime] = q
			}
			changed = true
		} else {
			noChangeCount++
		}
	}
	t.params.Logger.Debugw("updated quality change",
		"changed", changed,
		"maxSubscribedQuality", maxSubscribedQuality,
		"t.maxSubscribedQuality", t.maxSubscribedQuality,
		"qualityDowngrades", qualityDowngrades)

	if !changed && !force {
		t.maxQualityLock.Unlock()
		return
	}

	// if quality downgrade (or become OFF), delay notify to publisher if needed
	if len(qualityDowngrades) > 0 && !force {
		t.maxSubscribedQualityDebounce(func() {
			t.UpdateQualityChange(true)
		})

		// no quality upgrades
		if len(qualityDowngrades)+noChangeCount == len(t.maxSubscribedQuality) {
			t.maxQualityLock.Unlock()
			return
		}
	}

	subscribedCodec := make([]*livekit.SubscribedCodec, 0, len(t.maxSubscribedQuality))
	maxSubscribedQualities := make([]types.SubscribedCodecQuality, 0, len(t.maxSubscribedQuality))
	for mime, maxQuality := range t.maxSubscribedQuality {
		maxSubscribedQualities = append(maxSubscribedQualities, types.SubscribedCodecQuality{
			CodecMime: mime,
			Quality:   maxQuality,
		})

		if maxQuality == livekit.VideoQuality_OFF {
			subscribedCodec = append(subscribedCodec, &livekit.SubscribedCodec{
				Codec: mime,
				Qualities: []*livekit.SubscribedQuality{
					{Quality: livekit.VideoQuality_LOW, Enabled: false},
					{Quality: livekit.VideoQuality_MEDIUM, Enabled: false},
					{Quality: livekit.VideoQuality_HIGH, Enabled: false},
				},
			})
		} else {
			var subscribedQualities []*livekit.SubscribedQuality
			for q := livekit.VideoQuality_LOW; q <= livekit.VideoQuality_HIGH; q++ {
				subscribedQualities = append(subscribedQualities, &livekit.SubscribedQuality{
					Quality: q,
					Enabled: q <= maxQuality,
				})
			}
			subscribedCodec = append(subscribedCodec, &livekit.SubscribedCodec{
				Codec:     mime,
				Qualities: subscribedQualities,
			})
		}
	}
	if t.onSubscribedMaxQualityChange != nil {
		t.params.Logger.Debugw("subscribedMaxQualityChange",
			"subscribedCodec", subscribedCodec,
			"maxSubscribedQualities", maxSubscribedQualities)
		t.qualityNotifyOpQueue.Enqueue(func() {
			t.onSubscribedMaxQualityChange(subscribedCodec, maxSubscribedQualities)
		})
	}
	t.maxQualityLock.Unlock()
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
	empty := len(t.subscribedTracks) == 0 && len(t.inProgress) == 0 && len(t.requestsQueue) == 0
	t.subscribedTracksMu.RUnlock()

	if empty {
		t.onNoSubscribers()
	}
}

func (t *MediaTrackSubscriptions) downTrackClosed(
	sub types.LocalParticipant,
	subTrack types.SubscribedTrack,
	willBeResumed bool,
	sender *webrtc.RTPSender,
) {
	subscriberID := sub.ID()
	t.subscribedTracksMu.Lock()
	delete(t.subscribedTracks, subscriberID)
	t.subscribedTracksMu.Unlock()

	if !willBeResumed {
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
		sub.GetLogger().Infow("removing peerconnection track",
			"publisher", subTrack.PublisherIdentity(),
			"publisherID", subTrack.PublisherID(),
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
				sub.GetLogger().Infow("could not remove remoteTrack from forwarder",
					"error", err,
					"publisher", subTrack.PublisherIdentity(),
					"publisherID", subTrack.PublisherID(),
				)
			}
		}
	}

	sub.RemoveSubscribedTrack(subTrack)
	if !willBeResumed {
		sub.Negotiate(false)
	}

	t.clearInProgressAndProcessRequestsQueue(subscriberID)
	t.maybeNotifyNoSubscribers()
}

func (t *MediaTrackSubscriptions) clearInProgressAndProcessRequestsQueue(subscriberID livekit.ParticipantID) {
	t.subscribedTracksMu.Lock()
	delete(t.inProgress, subscriberID)
	t.subscribedTracksMu.Unlock()

	t.processRequestsQueue(subscriberID)
}

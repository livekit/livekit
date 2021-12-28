package rtc

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/connectionquality"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/rtcerr"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/twcc"
	"github.com/livekit/livekit-server/pkg/telemetry"
)

var (
	FeedbackTypes = []webrtc.RTCPFeedback{
		{Type: webrtc.TypeRTCPFBGoogREMB},
		{Type: webrtc.TypeRTCPFBNACK},
		{Type: webrtc.TypeRTCPFBNACK, Parameter: "pli"}}
)

const (
	lostUpdateDelta                 = time.Second
	connectionQualityUpdateInterval = 5 * time.Second
	layerSelectionTolerance         = 0.9
)

// MediaTrack represents a WebRTC track that needs to be forwarded
// Implements MediaTrack and PublishedTrack interface
type MediaTrack struct {
	params      MediaTrackParams
	ssrc        webrtc.SSRC
	streamID    string
	codec       webrtc.RTPCodecParameters
	muted       utils.AtomicFlag
	numUpTracks uint32
	simulcasted utils.AtomicFlag
	buffer      *buffer.Buffer

	lock sync.RWMutex

	// map of target participantId -> types.SubscribedTrack
	subscribedTracks sync.Map // participantSid => types.SubscribedTrack
	twcc             *twcc.Responder
	audioLevel       *AudioLevel
	receiver         sfu.Receiver
	lastPLI          time.Time
	layerDimensions  sync.Map // quality => *livekit.VideoLayer

	// track audio fraction lost
	statsLock         sync.Mutex
	maxDownFracLost   uint8
	maxDownFracLostTs time.Time
	currentUpFracLost uint32
	maxUpFracLost     uint8
	maxUpFracLostTs   time.Time
	connectionStats   *connectionquality.ConnectionStats

	done chan struct{}

	// quality level enable/disable
	maxQualityLock               sync.RWMutex
	maxSubscriberQuality         map[string]livekit.VideoQuality
	maxSubscribedQuality         livekit.VideoQuality
	allSubscribersMuted          bool
	onSubscribedMaxQualityChange func(trackSid string, subscribedQualities []*livekit.SubscribedQuality) error

	onClose []func()
}

type MediaTrackParams struct {
	TrackInfo           *livekit.TrackInfo
	SignalCid           string
	SdpCid              string
	ParticipantID       string
	ParticipantIdentity string
	// channel to send RTCP packets to the source
	RTCPChan       chan []rtcp.Packet
	BufferFactory  *buffer.Factory
	ReceiverConfig ReceiverConfig
	AudioConfig    config.AudioConfig
	Telemetry      telemetry.TelemetryService
	Logger         logger.Logger
}

func NewMediaTrack(track *webrtc.TrackRemote, params MediaTrackParams) *MediaTrack {
	t := &MediaTrack{
		params:               params,
		ssrc:                 track.SSRC(),
		streamID:             track.StreamID(),
		codec:                track.Codec(),
		connectionStats:      connectionquality.NewConnectionStats(),
		done:                 make(chan struct{}),
		maxSubscriberQuality: make(map[string]livekit.VideoQuality),
	}

	if params.TrackInfo.Muted {
		t.SetMuted(true)
	}

	if params.TrackInfo != nil && t.Kind() == livekit.TrackType_VIDEO {
		t.UpdateVideoLayers(params.TrackInfo.Layers)
		// LK-TODO: maybe use this or simulcast flag in TrackInfo to set simulcasted here
	}
	// on close signal via closing channel to workers
	t.AddOnClose(t.closeChan)
	go t.updateStats()

	time.AfterFunc(30*time.Second, func() {
		t.updateQualityChange()
	})

	return t
}

func (t *MediaTrack) ID() string {
	return t.params.TrackInfo.Sid
}

func (t *MediaTrack) SignalCid() string {
	return t.params.SignalCid
}

func (t *MediaTrack) SdpCid() string {
	return t.params.SdpCid
}

func (t *MediaTrack) Kind() livekit.TrackType {
	return t.params.TrackInfo.Type
}

func (t *MediaTrack) Source() livekit.TrackSource {
	return t.params.TrackInfo.Source
}

func (t *MediaTrack) IsSimulcast() bool {
	return t.simulcasted.Get()
}

func (t *MediaTrack) Name() string {
	return t.params.TrackInfo.Name
}

func (t *MediaTrack) IsMuted() bool {
	return t.muted.Get()
}

func (t *MediaTrack) SetMuted(muted bool) {
	t.muted.TrySet(muted)

	t.lock.RLock()
	if t.receiver != nil {
		t.receiver.SetUpTrackPaused(muted)
	}
	t.lock.RUnlock()

	// mute all subscribed tracks
	t.subscribedTracks.Range(func(_, value interface{}) bool {
		if st, ok := value.(types.SubscribedTrack); ok {
			st.SetPublisherMuted(muted)
		}
		return true
	})

	// update quality based on subscription if unmuting
	if !muted {
		t.updateQualityChange()
	}
}

func (t *MediaTrack) AddOnClose(f func()) {
	if f == nil {
		return
	}
	t.onClose = append(t.onClose, f)
}

func (t *MediaTrack) IsSubscriber(subId string) bool {
	_, ok := t.subscribedTracks.Load(subId)
	return ok
}

func (t *MediaTrack) PublishLossPercentage() uint32 {
	return FixedPointToPercent(uint8(atomic.LoadUint32(&t.currentUpFracLost)))
}

// AddSubscriber subscribes sub to current mediaTrack
func (t *MediaTrack) AddSubscriber(sub types.Participant) error {
	t.lock.Lock()
	defer t.lock.Unlock()

	subscriberID := sub.ID()

	// don't subscribe to the same track multiple times
	if _, ok := t.subscribedTracks.Load(subscriberID); ok {
		return nil
	}

	if t.receiver == nil {
		// cannot add, no receiver
		return errors.New("cannot subscribe without a receiver in place")
	}

	codec := t.receiver.Codec()
	// using DownTrack from ion-sfu
	streamId := t.params.ParticipantID
	if sub.ProtocolVersion().SupportsPackedStreamId() {
		// when possible, pack both IDs in streamID to allow new streams to be generated
		// react-native-webrtc still uses stream based APIs and require this
		streamId = PackStreamID(t.params.ParticipantID, t.ID())
	}
	receiver := NewWrappedReceiver(t.receiver, t.ID(), streamId)
	downTrack, err := sfu.NewDownTrack(webrtc.RTPCodecCapability{
		MimeType:     codec.MimeType,
		ClockRate:    codec.ClockRate,
		Channels:     codec.Channels,
		SDPFmtpLine:  codec.SDPFmtpLine,
		RTCPFeedback: FeedbackTypes,
	}, receiver, t.params.BufferFactory, subscriberID, t.params.ReceiverConfig.PacketBufferSize)
	if err != nil {
		return err
	}
	subTrack := NewSubscribedTrack(SubscribedTrackParams{
		PublisherID:       t.params.ParticipantID,
		PublisherIdentity: t.params.ParticipantIdentity,
		SubscriberID:      subscriberID,
		MediaTrack:        t,
		DownTrack:         downTrack,
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
			return err
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
			return errors.New("cannot subscribe without a transceiver in place")
		}
	} else {
		transceiver, err = sub.SubscriberPC().AddTransceiverFromTrack(downTrack, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionSendonly,
		})
		if err != nil {
			return err
		}

		sender = transceiver.Sender()
		if sender == nil {
			// cannot add, no sender
			return errors.New("cannot subscribe without a sender in place")
		}
	}

	sendParameters := sender.GetParameters()
	downTrack.SetRTPHeaderExtensions(sendParameters.HeaderExtensions)

	downTrack.SetTransceiver(transceiver)
	// when outtrack is bound, start loop to send reports
	downTrack.OnBind(func() {
		go subTrack.Bound()
		go t.sendDownTrackBindingReports(sub)
	})
	downTrack.OnPacketSent(func(_ *sfu.DownTrack, size int) {
		t.params.Telemetry.OnDownstreamPacket(subscriberID, size)
	})
	downTrack.OnPaddingSent(func(_ *sfu.DownTrack, size int) {
		t.params.Telemetry.OnDownstreamPacket(subscriberID, size)
	})
	downTrack.OnRTCP(func(pkts []rtcp.Packet) {
		t.params.Telemetry.HandleRTCP(livekit.StreamType_DOWNSTREAM, subscriberID, pkts)
	})

	downTrack.OnCloseHandler(func() {
		go func() {
			t.subscribedTracks.Delete(subscriberID)
			t.params.Telemetry.TrackUnsubscribed(context.Background(), subscriberID, t.ToProto())

			// ignore if the subscribing sub is not connected
			if sub.SubscriberPC().ConnectionState() == webrtc.PeerConnectionStateClosed {
				return
			}

			// if the source has been terminated, we'll need to terminate all of the subscribedtracks
			// however, if the dest sub has disconnected, then we can skip
			if sender == nil {
				return
			}
			t.params.Logger.Debugw("removing peerconnection track",
				"track", t.ID(),
				"subscriber", sub.Identity(),
				"subscriberID", subscriberID,
				"kind", t.Kind(),
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

			t.NotifySubscriberMute(subscriberID)
			sub.RemoveSubscribedTrack(subTrack)
			sub.Negotiate()
		}()
	})
	if t.Kind() == livekit.TrackType_AUDIO {
		downTrack.AddReceiverReportListener(t.handleMaxLossFeedback)
	}

	t.subscribedTracks.Store(subscriberID, subTrack)
	subTrack.SetPublisherMuted(t.IsMuted())

	t.receiver.AddDownTrack(downTrack)
	// since sub will lock, run it in a goroutine to avoid deadlocks
	go func() {
		t.NotifySubscriberMaxQuality(subscriberID, livekit.VideoQuality_HIGH) // start with HIGH, let subscription change it later
		sub.AddSubscribedTrack(subTrack)
		sub.Negotiate()
	}()

	t.params.Telemetry.TrackSubscribed(context.Background(), subscriberID, t.ToProto())
	return nil
}

func (t *MediaTrack) NumUpTracks() (uint32, uint32) {
	numRegistered := atomic.LoadUint32(&t.numUpTracks)

	t.maxQualityLock.RLock()
	// LK-TODO: take into account t.allSubscribersMuted when turning off layer 0 also
	maxSubscribed := uint32(SpatialLayerForQuality(t.maxSubscribedQuality) + 1)
	t.maxQualityLock.RUnlock()
	if maxSubscribed < numRegistered {
		numRegistered = maxSubscribed
	}

	t.lock.RLock()
	numPublishing := uint32(t.receiver.NumAvailableSpatialLayers())
	t.lock.RUnlock()

	return numPublishing, numRegistered
}

// AddReceiver adds a new RTP receiver to the track
func (t *MediaTrack) AddReceiver(receiver *webrtc.RTPReceiver, track *webrtc.TrackRemote, twcc *twcc.Responder) {
	t.lock.Lock()
	defer t.lock.Unlock()

	buff, rtcpReader := t.params.BufferFactory.GetBufferPair(uint32(track.SSRC()))
	if buff == nil || rtcpReader == nil {
		logger.Errorw("could not retrieve buffer pair", nil,
			"track", t.ID())
		return
	}
	buff.OnFeedback(t.handlePublisherFeedback)

	if t.Kind() == livekit.TrackType_AUDIO {
		t.audioLevel = NewAudioLevel(t.params.AudioConfig.ActiveLevel, t.params.AudioConfig.MinPercentile)
		buff.OnAudioLevel(func(level uint8, duration uint32) {
			t.audioLevel.Observe(level, duration)
		})
	} else if t.Kind() == livekit.TrackType_VIDEO {
		if twcc != nil {
			buff.OnTransportWideCC(func(sn uint16, timeNS int64, marker bool) {
				twcc.Push(sn, timeNS, marker)
			})
		}
	}

	rtcpReader.OnPacket(func(bytes []byte) {
		pkts, err := rtcp.Unmarshal(bytes)
		if err != nil {
			t.params.Logger.Errorw("could not unmarshal RTCP", err)
			return
		}

		for _, pkt := range pkts {
			switch pkt := pkt.(type) {
			case *rtcp.SourceDescription:
			// do nothing for now
			case *rtcp.SenderReport:
				buff.SetSenderReportData(pkt.RTPTime, pkt.NTPTime)
			}
		}
	})

	if t.receiver == nil {
		t.receiver = sfu.NewWebRTCReceiver(receiver, track, t.params.ParticipantID,
			sfu.WithPliThrottle(0),
			sfu.WithLoadBalanceThreshold(20),
			sfu.WithStreamTrackers())
		t.receiver.SetRTCPCh(t.params.RTCPChan)
		t.receiver.OnCloseHandler(func() {
			t.lock.Lock()
			t.receiver = nil
			onclose := t.onClose
			t.lock.Unlock()
			t.RemoveAllSubscribers()
			t.params.Telemetry.TrackUnpublished(context.Background(), t.params.ParticipantID, t.ToProto(), uint32(track.SSRC()))
			for _, f := range onclose {
				f()
			}
		})
		t.params.Telemetry.TrackPublished(context.Background(), t.params.ParticipantID, t.ToProto())
		if t.Kind() == livekit.TrackType_AUDIO {
			t.buffer = buff
		}
	}

	t.receiver.AddUpTrack(track, buff)
	t.params.Telemetry.AddUpTrack(t.params.ParticipantID, buff)

	atomic.AddUint32(&t.numUpTracks, 1)
	// LK-TODO: can remove this completely when VideoLayers protocol becomes the default as it has info from client or if we decide to use TrackInfo.Simulcast
	if atomic.LoadUint32(&t.numUpTracks) > 1 || track.RID() != "" {
		// cannot only rely on numUpTracks since we fire metadata events immediately after the first layer
		t.simulcasted.TrySet(true)
	}

	buff.Bind(receiver.GetParameters(), track.Codec().RTPCodecCapability, buffer.Options{
		MaxBitRate: t.params.ReceiverConfig.maxBitrate,
	})
}

// RemoveSubscriber removes participant from subscription
// stop all forwarders to the client
func (t *MediaTrack) RemoveSubscriber(participantId string) {
	subTrack := t.getSubscribedTrack(participantId)
	if subTrack != nil {
		go subTrack.DownTrack().Close()
	}
}

func (t *MediaTrack) RemoveAllSubscribers() {
	t.params.Logger.Debugw("removing all subscribers", "track", t.ID())
	t.lock.Lock()
	defer t.lock.Unlock()
	t.subscribedTracks.Range(func(_, val interface{}) bool {
		if subTrack, ok := val.(types.SubscribedTrack); ok {
			go subTrack.DownTrack().Close()
		}
		return true
	})
	t.subscribedTracks = sync.Map{}
}

func (t *MediaTrack) RevokeDisallowedSubscribers(allowedSubscriberIDs []string) []string {
	t.lock.Lock()
	defer t.lock.Unlock()

	var revokedSubscriberIDs []string
	// LK-TODO: large number of subscribers needs to be solved for this loop
	t.subscribedTracks.Range(func(key interface{}, val interface{}) bool {
		if subID, ok := key.(string); ok {
			found := false
			for _, allowedID := range allowedSubscriberIDs {
				if subID == allowedID {
					found = true
					break
				}
			}

			if !found {
				if subTrack, ok := val.(types.SubscribedTrack); ok {
					go subTrack.DownTrack().Close()
					revokedSubscriberIDs = append(revokedSubscriberIDs, subID)
				}
			}
		}
		return true
	})

	return revokedSubscriberIDs
}

func (t *MediaTrack) ToProto() *livekit.TrackInfo {
	info := t.params.TrackInfo
	info.Muted = t.IsMuted()
	info.Simulcast = t.simulcasted.Get()
	layers := make([]*livekit.VideoLayer, 0)
	t.layerDimensions.Range(func(_, val interface{}) bool {
		if layer, ok := val.(*livekit.VideoLayer); ok {
			layers = append(layers, layer)
		}
		return true
	})
	info.Layers = layers

	return info
}

func (t *MediaTrack) UpdateVideoLayers(layers []*livekit.VideoLayer) {
	for _, layer := range layers {
		t.layerDimensions.Store(layer.Quality, layer)
	}
	t.subscribedTracks.Range(func(_, val interface{}) bool {
		if st, ok := val.(types.SubscribedTrack); ok {
			st.UpdateVideoLayer()
		}
		return true
	})
	// TODO: this might need to trigger a participant update for clients to pick up dimension change
}

// GetQualityForDimension finds the closest quality to use for desired dimensions
// affords a 20% tolerance on dimension
func (t *MediaTrack) GetQualityForDimension(width, height uint32) livekit.VideoQuality {
	quality := livekit.VideoQuality_HIGH
	if t.Kind() == livekit.TrackType_AUDIO || t.params.TrackInfo.Height == 0 {
		return quality
	}
	origSize := t.params.TrackInfo.Height
	requestedSize := height
	if t.params.TrackInfo.Width < t.params.TrackInfo.Height {
		// for portrait videos
		origSize = t.params.TrackInfo.Width
		requestedSize = width
	}

	// default sizes representing qualities low - high
	layerSizes := []uint32{180, 360, origSize}
	var providedSizes []uint32
	t.layerDimensions.Range(func(_, val interface{}) bool {
		if layer, ok := val.(*livekit.VideoLayer); ok {
			providedSizes = append(providedSizes, layer.Height)
		}
		return true
	})
	if len(providedSizes) > 0 {
		layerSizes = providedSizes
		// comparing height always
		requestedSize = height
		sort.Slice(layerSizes, func(i, j int) bool {
			return layerSizes[i] < layerSizes[j]
		})
	}

	// finds the lowest layer that could satisfy client demands
	requestedSize = uint32(float32(requestedSize) * layerSelectionTolerance)
	for i, s := range layerSizes {
		quality = livekit.VideoQuality(i)
		if s >= requestedSize {
			break
		}
	}

	return quality
}

func (t *MediaTrack) getSubscribedTrack(subscriberID string) types.SubscribedTrack {
	if val, ok := t.subscribedTracks.Load(subscriberID); ok {
		if st, ok := val.(types.SubscribedTrack); ok {
			return st
		}
	}
	return nil
}

// TODO: send for all downtracks from the source participant
// https://tools.ietf.org/html/rfc7941
func (t *MediaTrack) sendDownTrackBindingReports(sub types.Participant) {
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

func (t *MediaTrack) handlePublisherFeedback(packets []rtcp.Packet) {
	var maxLost uint8
	var hasReport bool
	var delay uint32
	var jitter uint32
	var totalLost uint32
	var maxSeqNum uint32

	for _, p := range packets {
		switch pkt := p.(type) {
		// sfu.Buffer generates ReceiverReports for the publisher
		case *rtcp.ReceiverReport:
			for _, rr := range pkt.Reports {
				if rr.FractionLost > maxLost {
					maxLost = rr.FractionLost
				}

				if rr.Delay > delay {
					delay = rr.Delay
				}
				if rr.Jitter > jitter {
					jitter = rr.Jitter
				}
				if rr.LastSequenceNumber > maxSeqNum {
					maxSeqNum = rr.LastSequenceNumber
				}

				totalLost = rr.TotalLost

				hasReport = true
			}
		}
	}

	if hasReport {
		t.statsLock.Lock()
		if maxLost > t.maxUpFracLost {
			t.maxUpFracLost = maxLost
		}

		now := time.Now()
		if now.Sub(t.maxUpFracLostTs) > lostUpdateDelta {
			atomic.StoreUint32(&t.currentUpFracLost, uint32(t.maxUpFracLost))
			t.maxUpFracLost = 0
			t.maxUpFracLostTs = now
		}
		// update feedback stats
		current := t.connectionStats.Curr
		if jitter > current.Jitter {
			current.Jitter = jitter
		}
		if delay > current.Delay {
			current.Delay = delay
		}
		if maxSeqNum > current.LastSeqNum {
			current.LastSeqNum = maxSeqNum
		}
		current.PacketsLost = totalLost
		t.statsLock.Unlock()
	}

	// also look for sender reports
	// feedback for the source RTCP
	t.params.RTCPChan <- packets
}

// handles max loss for audio packets
func (t *MediaTrack) handleMaxLossFeedback(_ *sfu.DownTrack,
	report *rtcp.ReceiverReport) {
	var (
		shouldUpdate bool
		maxLost      uint8
	)
	t.statsLock.Lock()
	for _, rr := range report.Reports {
		if t.maxDownFracLost < rr.FractionLost {
			t.maxDownFracLost = rr.FractionLost
		}
	}

	now := time.Now()
	if now.Sub(t.maxDownFracLostTs) > lostUpdateDelta {
		shouldUpdate = true
		maxLost = t.maxDownFracLost
		t.maxDownFracLost = 0
		t.maxDownFracLostTs = now
	}
	t.statsLock.Unlock()

	if shouldUpdate && t.buffer != nil {
		// ok to access buffer since receivers are added before subscribers
		t.buffer.SetLastFractionLostReport(maxLost)
	}
}

func (t *MediaTrack) DebugInfo() map[string]interface{} {
	info := map[string]interface{}{
		"ID":       t.ID(),
		"SSRC":     t.ssrc,
		"Kind":     t.Kind().String(),
		"PubMuted": t.muted.Get(),
	}

	subscribedTrackInfo := make([]map[string]interface{}, 0)
	t.subscribedTracks.Range(func(_, val interface{}) bool {
		if track, ok := val.(*SubscribedTrack); ok {
			dt := track.DownTrack().DebugInfo()
			dt["PubMuted"] = track.pubMuted.Get()
			dt["SubMuted"] = track.subMuted.Get()
			subscribedTrackInfo = append(subscribedTrackInfo, dt)
		}
		return true
	})
	info["DownTracks"] = subscribedTrackInfo

	if t.receiver != nil {
		receiverInfo := t.receiver.DebugInfo()
		for k, v := range receiverInfo {
			info[k] = v
		}
	}

	return info
}

func (t *MediaTrack) Receiver() sfu.TrackReceiver {
	return t.receiver
}

func (t *MediaTrack) GetConnectionScore() float64 {
	t.statsLock.Lock()
	defer t.statsLock.Unlock()
	return t.connectionStats.Score
}

func (t *MediaTrack) closeChan() {
	close(t.done)
}

func (t *MediaTrack) updateStats() {
	for {
		select {
		case <-t.done:
			return
		case <-time.After(connectionQualityUpdateInterval):
			t.statsLock.Lock()
			if t.Kind() == livekit.TrackType_AUDIO {
				t.connectionStats.CalculateAudioScore()
			} else {
				t.calculateVideoScore()
			}
			t.statsLock.Unlock()
		}
	}
}

func (t *MediaTrack) calculateVideoScore() {
	var reducedQuality bool
	publishing, registered := t.NumUpTracks()
	if registered > 0 && publishing < registered {
		reducedQuality = true
	}

	loss := t.PublishLossPercentage()
	if registered == 0 {
		loss = 0
	}
	t.connectionStats.Score = connectionquality.Loss2Score(loss, reducedQuality)
}

func (t *MediaTrack) OnSubscribedMaxQualityChange(f func(trackSid string, subscribedQualities []*livekit.SubscribedQuality) error) {
	t.onSubscribedMaxQualityChange = f
}

func (t *MediaTrack) NotifySubscriberMute(subscriberID string) {
	if !t.IsSimulcast() {
		return
	}

	t.maxQualityLock.Lock()
	_, ok := t.maxSubscriberQuality[subscriberID]
	if !ok {
		t.maxQualityLock.Unlock()
		return
	}

	delete(t.maxSubscriberQuality, subscriberID)
	t.maxQualityLock.Unlock()

	t.updateQualityChange()
}

func (t *MediaTrack) NotifySubscriberMaxQuality(subscriberID string, quality livekit.VideoQuality) {
	if !t.IsSimulcast() {
		return
	}

	t.maxQualityLock.Lock()
	maxQuality, ok := t.maxSubscriberQuality[subscriberID]
	if ok && maxQuality == quality {
		t.maxQualityLock.Unlock()
		return
	}

	t.maxSubscriberQuality[subscriberID] = quality
	t.maxQualityLock.Unlock()

	t.updateQualityChange()
}

func (t *MediaTrack) updateQualityChange() {
	if t.IsMuted() || !t.IsSimulcast() {
		return
	}

	var subscribedQualities []*livekit.SubscribedQuality

	t.maxQualityLock.Lock()
	allSubscribersMuted := false
	maxSubscribedQuality := livekit.VideoQuality_LOW
	if len(t.maxSubscriberQuality) == 0 {
		allSubscribersMuted = true
	} else {
		for _, subQuality := range t.maxSubscriberQuality {
			if subQuality > maxSubscribedQuality {
				maxSubscribedQuality = subQuality
			}
		}
	}

	notifyMaxExpected := false
	maxExpectedSpatialLayer := int32(-1)
	if allSubscribersMuted {
		if !t.allSubscribersMuted {
			notifyMaxExpected = true
			t.allSubscribersMuted = true

			// LK-TODO: do not set this when turning off LOW also below
			t.maxSubscribedQuality = livekit.VideoQuality_LOW
			maxExpectedSpatialLayer = SpatialLayerForQuality(t.maxSubscribedQuality)

			subscribedQualities = []*livekit.SubscribedQuality{
				// LK-TODO-START
				// Restarting layers and subscribers getting video involves several things in the path
				// which adds up to a few seconds of latency on re-subscription. Do not turn off the
				// base layer till there is a good design to reduce re-subscription delays
				// LK-TODO-END
				{Quality: livekit.VideoQuality_LOW, Enabled: true},
				{Quality: livekit.VideoQuality_MEDIUM, Enabled: false},
				{Quality: livekit.VideoQuality_HIGH, Enabled: false},
			}
		}
	} else {
		t.allSubscribersMuted = false
		if maxSubscribedQuality != t.maxSubscribedQuality {
			notifyMaxExpected = true
			t.maxSubscribedQuality = maxSubscribedQuality
			maxExpectedSpatialLayer = SpatialLayerForQuality(t.maxSubscribedQuality)

			subscribedQualities = append(subscribedQualities, &livekit.SubscribedQuality{Quality: livekit.VideoQuality_LOW, Enabled: true})

			if t.maxSubscribedQuality == livekit.VideoQuality_LOW {
				subscribedQualities = append(subscribedQualities, &livekit.SubscribedQuality{Quality: livekit.VideoQuality_MEDIUM, Enabled: false})
			} else {
				subscribedQualities = append(subscribedQualities, &livekit.SubscribedQuality{Quality: livekit.VideoQuality_MEDIUM, Enabled: true})
			}

			if t.maxSubscribedQuality != livekit.VideoQuality_HIGH {
				subscribedQualities = append(subscribedQualities, &livekit.SubscribedQuality{Quality: livekit.VideoQuality_HIGH, Enabled: false})
			} else {
				subscribedQualities = append(subscribedQualities, &livekit.SubscribedQuality{Quality: livekit.VideoQuality_HIGH, Enabled: true})
			}
		}
	}
	t.maxQualityLock.Unlock()

	if len(subscribedQualities) != 0 && t.onSubscribedMaxQualityChange != nil {
		_ = t.onSubscribedMaxQualityChange(t.ID(), subscribedQualities)
	}

	if notifyMaxExpected {
		t.lock.RLock()
		if t.receiver != nil {
			t.receiver.SetMaxExpectedSpatialLayer(maxExpectedSpatialLayer)
		}
		t.lock.RUnlock()
	}
}

//---------------------------

func SpatialLayerForQuality(quality livekit.VideoQuality) int32 {
	switch quality {
	case livekit.VideoQuality_LOW:
		return 0
	case livekit.VideoQuality_MEDIUM:
		return 1
	default:
		return 2
	}
}

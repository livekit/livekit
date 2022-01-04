package rtc

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu/connectionquality"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/twcc"
	"github.com/livekit/livekit-server/pkg/telemetry"
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

	twcc            *twcc.Responder
	audioLevel      *AudioLevel
	receiver        sfu.Receiver
	lastPLI         time.Time
	layerDimensions sync.Map // quality => *livekit.VideoLayer

	// track audio fraction lost
	statsLock         sync.Mutex
	maxDownFracLost   uint8
	maxDownFracLostTs time.Time
	currentUpFracLost uint32
	maxUpFracLost     uint8
	maxUpFracLostTs   time.Time
	connectionStats   *connectionquality.ConnectionStats

	done chan struct{}

	onClose []func()

	*MediaTrackSubscriptions
}

type MediaTrackParams struct {
	TrackInfo           *livekit.TrackInfo
	SignalCid           string
	SdpCid              string
	ParticipantID       livekit.ParticipantID
	ParticipantIdentity livekit.ParticipantIdentity
	// channel to send RTCP packets to the source
	RTCPChan       chan []rtcp.Packet
	BufferFactory  *buffer.Factory
	ReceiverConfig ReceiverConfig
	AudioConfig    config.AudioConfig
	Telemetry      telemetry.TelemetryService
	Logger         logger.Logger

	SubscriberConfig DirectionConfig
}

func NewMediaTrack(track *webrtc.TrackRemote, params MediaTrackParams) *MediaTrack {
	t := &MediaTrack{
		params:          params,
		ssrc:            track.SSRC(),
		streamID:        track.StreamID(),
		codec:           track.Codec(),
		connectionStats: connectionquality.NewConnectionStats(),
		done:            make(chan struct{}),
	}

	t.MediaTrackSubscriptions = NewMediaTrackSubscriptions(MediaTrackSubscriptionsParams{
		MediaTrack:       t,
		BufferFactory:    params.BufferFactory,
		ReceiverConfig:   params.ReceiverConfig,
		SubscriberConfig: params.SubscriberConfig,
		Telemetry:        params.Telemetry,
		Logger:           params.Logger,
	})

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

	return t
}

func (t *MediaTrack) ID() livekit.TrackID {
	return livekit.TrackID(t.params.TrackInfo.Sid)
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

func (t *MediaTrack) ParticipantID() livekit.ParticipantID {
	return t.params.ParticipantID
}

func (t *MediaTrack) ParticipantIdentity() livekit.ParticipantIdentity {
	return t.params.ParticipantIdentity
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

	t.MediaTrackSubscriptions.SetMuted(muted)
}

func (t *MediaTrack) AddOnClose(f func()) {
	if f == nil {
		return
	}
	t.onClose = append(t.onClose, f)
}

func (t *MediaTrack) PublishLossPercentage() uint32 {
	return FixedPointToPercent(uint8(atomic.LoadUint32(&t.currentUpFracLost)))
}

// AddSubscriber subscribes sub to current mediaTrack
func (t *MediaTrack) AddSubscriber(sub types.Participant) error {
	t.lock.Lock()
	defer t.lock.Unlock()

	if t.receiver == nil {
		// cannot add, no receiver
		return errors.New("cannot subscribe without a receiver in place")
	}

	// using DownTrack from ion-sfu
	streamId := string(t.params.ParticipantID)
	if sub.ProtocolVersion().SupportsPackedStreamId() {
		// when possible, pack both IDs in streamID to allow new streams to be generated
		// react-native-webrtc still uses stream based APIs and require this
		streamId = PackStreamID(t.params.ParticipantID, t.ID())
	}

	downTrack, err := t.MediaTrackSubscriptions.AddSubscriber(sub, t.receiver.Codec(), NewWrappedReceiver(t.receiver, t.ID(), streamId))
	if err != nil {
		return err
	}

	if downTrack != nil {
		if t.Kind() == livekit.TrackType_AUDIO {
			downTrack.AddReceiverReportListener(t.handleMaxLossFeedback)
		}

		t.receiver.AddDownTrack(downTrack)
	}
	return nil
}

func (t *MediaTrack) NumUpTracks() (uint32, uint32) {
	numExpected := atomic.LoadUint32(&t.numUpTracks)

	numSubscribed := t.numSubscribed()
	if numSubscribed < numExpected {
		numExpected = numSubscribed
	}

	t.lock.RLock()
	numPublishing := uint32(0)
	if t.receiver != nil {
		numPublishing = uint32(t.receiver.NumAvailableSpatialLayers())
	}
	t.lock.RUnlock()

	return numPublishing, numExpected
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
			t.stopMaxQualityTimer()

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

		t.startMaxQualityTimer()
	}

	t.receiver.AddUpTrack(track, buff)
	t.params.Telemetry.AddUpTrack(t.params.ParticipantID, t.ID(), buff)

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

	t.MediaTrackSubscriptions.UpdateVideoLayers()

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

func (t *MediaTrack) handlePublisherFeedback(packets []rtcp.Packet) {
	var maxLost uint8
	var hasReport bool
	var delay uint32
	var jitter uint32
	var totalLost uint32
	var maxSeqNum uint32

	//forward to telemetry
	t.params.Telemetry.HandleRTCP(livekit.StreamType_UPSTREAM, t.params.ParticipantID, t.ID(), packets)

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
func (t *MediaTrack) handleMaxLossFeedback(_ *sfu.DownTrack, report *rtcp.ReceiverReport) {
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

	info["DownTracks"] = t.MediaTrackSubscriptions.DebugInfo()

	t.lock.RLock()
	if t.receiver != nil {
		receiverInfo := t.receiver.DebugInfo()
		for k, v := range receiverInfo {
			info[k] = v
		}
	}
	t.lock.RUnlock()

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
	publishing, expected := t.NumUpTracks()
	if publishing < expected {
		reducedQuality = true
	}

	loss := t.PublishLossPercentage()
	if expected == 0 {
		loss = 0
	}
	t.connectionStats.Score = connectionquality.Loss2Score(loss, reducedQuality)
}

func (t *MediaTrack) OnSubscribedMaxQualityChange(f func(trackID livekit.TrackID, subscribedQualities []*livekit.SubscribedQuality) error) {
	t.MediaTrackSubscriptions.OnSubscribedMaxQualityChange(func(subscribedQualities []*livekit.SubscribedQuality, maxSubscribedQuality livekit.VideoQuality) {
		if f != nil && !t.IsMuted() {
			_ = f(t.ID(), subscribedQualities)
		}

		t.lock.RLock()
		if t.receiver != nil {
			t.receiver.SetMaxExpectedSpatialLayer(SpatialLayerForQuality(maxSubscribedQuality))
		}
		t.lock.RUnlock()
	})
}

//---------------------------

func SpatialLayerForQuality(quality livekit.VideoQuality) int32 {
	switch quality {
	case livekit.VideoQuality_LOW:
		return 0
	case livekit.VideoQuality_MEDIUM:
		return 1
	case livekit.VideoQuality_HIGH:
		return 2
	case livekit.VideoQuality_OFF:
		return -1
	default:
		return -1
	}
}

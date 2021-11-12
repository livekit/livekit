package rtc

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/protocol/logger"
	livekit "github.com/livekit/protocol/proto"
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
	feedbackTypes = []webrtc.RTCPFeedback{
		{Type: webrtc.TypeRTCPFBGoogREMB},
		{Type: webrtc.TypeRTCPFBNACK},
		{Type: webrtc.TypeRTCPFBNACK, Parameter: "pli"}}
)

const (
	lostUpdateDelta = time.Second
)

// MediaTrack represents a WebRTC track that needs to be forwarded
// Implements the PublishedTrack interface
type MediaTrack struct {
	params      MediaTrackParams
	ssrc        webrtc.SSRC
	streamID    string
	codec       webrtc.RTPCodecParameters
	muted       utils.AtomicFlag
	numUpTracks uint32
	simulcasted utils.AtomicFlag
	buffer      *buffer.Buffer

	// channel to send RTCP packets to the source
	lock sync.RWMutex
	// map of target participantId -> *SubscribedTrack
	subscribedTracks map[string]*SubscribedTrack
	twcc             *twcc.Responder
	audioLevel       *AudioLevel
	receiver         sfu.Receiver
	lastPLI          time.Time

	// track audio fraction lost
	fracLostLock      sync.Mutex
	maxDownFracLost   uint8
	maxDownFracLostTs time.Time
	currentUpFracLost uint32
	maxUpFracLost     uint8
	maxUpFracLostTs   time.Time

	onClose func()
}

type MediaTrackParams struct {
	TrackInfo           *livekit.TrackInfo
	SignalCid           string
	SdpCid              string
	ParticipantID       string
	ParticipantIdentity string
	RTCPChan            chan []rtcp.Packet
	BufferFactory       *buffer.Factory
	ReceiverConfig      ReceiverConfig
	AudioConfig         config.AudioConfig
	Telemetry           *telemetry.TelemetryService
	Logger              logger.Logger
}

func NewMediaTrack(track *webrtc.TrackRemote, params MediaTrackParams) *MediaTrack {
	t := &MediaTrack{
		params:           params,
		ssrc:             track.SSRC(),
		streamID:         track.StreamID(),
		codec:            track.Codec(),
		subscribedTracks: make(map[string]*SubscribedTrack),
	}

	if params.TrackInfo.Muted {
		t.SetMuted(true)
	}
	return t
}

func (t *MediaTrack) Start() {
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
	// mute all subscribed tracks
	for _, st := range t.subscribedTracks {
		st.SetPublisherMuted(muted)
	}
	t.lock.RUnlock()
}

func (t *MediaTrack) OnClose(f func()) {
	t.onClose = f
}

func (t *MediaTrack) IsSubscriber(subId string) bool {
	t.lock.RLock()
	defer t.lock.RUnlock()
	return t.subscribedTracks[subId] != nil
}

func (t *MediaTrack) PublishLossPercentage() uint32 {
	return FixedPointToPercent(uint8(atomic.LoadUint32(&t.currentUpFracLost)))
}

// AddSubscriber subscribes sub to current mediaTrack
func (t *MediaTrack) AddSubscriber(sub types.Participant) error {
	if !sub.CanSubscribe() {
		return ErrPermissionDenied
	}

	t.lock.Lock()
	defer t.lock.Unlock()
	existingSt := t.subscribedTracks[sub.ID()]

	// don't subscribe to the same track multiple times
	if existingSt != nil {
		return nil
	}

	if t.receiver == nil {
		// cannot add, no receiver
		return errors.New("cannot subscribe without a receiver in place")
	}

	codec := t.receiver.Codec()
	if err := sub.SubscriberMediaEngine().RegisterCodec(codec, t.receiver.Kind()); err != nil {
		return err
	}

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
		RTCPFeedback: feedbackTypes,
	}, receiver, t.params.BufferFactory, sub.ID(), t.params.ReceiverConfig.packetBufferSize)
	if err != nil {
		return err
	}
	subTrack := NewSubscribedTrack(t.params.ParticipantIdentity, downTrack)

	var transceiver *webrtc.RTPTransceiver
	var sender *webrtc.RTPSender
	if sub.ProtocolVersion().SupportsTransceiverReuse() {
		//
		// AddTrack will create a new transceiver or re-use an unused one
		// if the attributes match. This prevents SDP from bloating
		// because of dormant transceivers buidling up.
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
		go t.sendDownTrackBindingReports(sub)
	})

	downTrack.OnCloseHandler(func() {
		go func() {
			t.lock.Lock()
			delete(t.subscribedTracks, sub.ID())
			t.lock.Unlock()

			t.params.Telemetry.TrackUnsubscribed(sub.ID(), sub.Identity(), t.ToProto())

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
				"pIDs", []string{t.params.ParticipantID, sub.ID()},
				"participant", sub.Identity(),
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
						"participant", sub.Identity(), "pID", sub.ID())
				}
			}

			sub.RemoveSubscribedTrack(subTrack)
			sub.Negotiate()
		}()
	})
	if t.Kind() == livekit.TrackType_AUDIO {
		downTrack.AddReceiverReportListener(t.handleMaxLossFeedback)
	}

	t.subscribedTracks[sub.ID()] = subTrack
	subTrack.SetPublisherMuted(t.IsMuted())

	t.receiver.AddDownTrack(downTrack, t.shouldStartWithBestQuality())
	// since sub will lock, run it in a goroutine to avoid deadlocks
	go func() {
		sub.AddSubscribedTrack(subTrack)
		sub.Negotiate()
	}()

	t.params.Telemetry.TrackSubscribed(sub.ID(), sub.Identity(), t.ToProto())
	return nil
}

func (t *MediaTrack) NumUpTracks() (uint32, uint32) {
	numRegistered := atomic.LoadUint32(&t.numUpTracks)
	numPublishing := uint32(0)
	if t.simulcasted.Get() {
		t.lock.RLock()
		if t.receiver != nil {
			for i := int32(0); i < 3; i++ {
				if t.receiver.HasSpatialLayer(i) {
					numPublishing += 1
				}
			}
		}
		t.lock.RUnlock()
	} else {
		numPublishing = 1
	}

	return numPublishing, numRegistered
}

// AddReceiver adds a new RTP receiver to the track
func (t *MediaTrack) AddReceiver(receiver *webrtc.RTPReceiver, track *webrtc.TrackRemote, twcc *twcc.Responder) {
	t.lock.Lock()
	defer t.lock.Unlock()

	buff, rtcpReader := t.params.BufferFactory.GetBufferPair(uint32(track.SSRC()))
	buff.OnFeedback(t.handlePublisherFeedback)

	if t.Kind() == livekit.TrackType_AUDIO {
		t.audioLevel = NewAudioLevel(t.params.AudioConfig.ActiveLevel, t.params.AudioConfig.MinPercentile)
		buff.OnAudioLevel(func(level uint8) {
			t.audioLevel.Observe(level)
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
			t.params.Telemetry.TrackUnpublished(t.params.ParticipantID, t.params.ParticipantIdentity, t.ToProto(), uint32(track.SSRC()))
			if onclose != nil {
				onclose()
			}
		})
		t.params.Telemetry.TrackPublished(t.params.ParticipantID, t.params.ParticipantIdentity, t.ToProto(), buff)

		if t.Kind() == livekit.TrackType_AUDIO {
			t.buffer = buff
		}
	}
	t.receiver.AddUpTrack(track, buff, t.shouldStartWithBestQuality())
	atomic.AddUint32(&t.numUpTracks, 1)
	if atomic.LoadUint32(&t.numUpTracks) > 1 {
		t.simulcasted.TrySet(true)
	}

	buff.Bind(receiver.GetParameters(), buffer.Options{
		MaxBitRate: t.params.ReceiverConfig.maxBitrate,
	})
}

// RemoveSubscriber removes participant from subscription
// stop all forwarders to the client
func (t *MediaTrack) RemoveSubscriber(participantId string) {
	t.lock.RLock()
	defer t.lock.RUnlock()

	if subTrack := t.subscribedTracks[participantId]; subTrack != nil {
		go subTrack.DownTrack().Close()
	}
}

func (t *MediaTrack) RemoveAllSubscribers() {
	t.params.Logger.Debugw("removing all subscribers", "track", t.ID())
	t.lock.Lock()
	defer t.lock.Unlock()
	for _, subTrack := range t.subscribedTracks {
		go subTrack.DownTrack().Close()
	}
	t.subscribedTracks = make(map[string]*SubscribedTrack)
}

func (t *MediaTrack) ToProto() *livekit.TrackInfo {
	info := t.params.TrackInfo
	info.Muted = t.IsMuted()
	info.Simulcast = t.simulcasted.Get()
	return info
}

// GetQualityForDimension finds the closest quality to use for desired dimensions
// affords a 10% tolerance on dimension
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

	// representing qualities low - high
	layerSizes := []uint32{180, 360, origSize}

	// finds the lowest layer that could satisfy client demands
	requestedSize = uint32(float32(requestedSize) * 0.9)
	for i, s := range layerSizes {
		quality = livekit.VideoQuality(i)
		if s >= requestedSize {
			break
		}
	}

	return quality
}

// this function assumes caller holds lock
func (t *MediaTrack) shouldStartWithBestQuality() bool {
	return len(t.subscribedTracks) < 10
}

// TODO: send for all downtracks from the source participant
// https://tools.ietf.org/html/rfc7941
func (t *MediaTrack) sendDownTrackBindingReports(sub types.Participant) {
	var sd []rtcp.SourceDescriptionChunk

	t.lock.RLock()
	subTrack := t.subscribedTracks[sub.ID()]
	t.lock.RUnlock()

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
	var hasSenderReport bool
	for _, p := range packets {
		switch pkt := p.(type) {
		case *rtcp.SenderReport:
			for _, rr := range pkt.Reports {
				if rr.FractionLost > maxLost {
					maxLost = rr.FractionLost
				}
				hasSenderReport = true
			}
		}
	}

	if hasSenderReport {
		t.fracLostLock.Lock()
		if maxLost > t.maxUpFracLost {
			t.maxUpFracLost = maxLost
		}

		now := time.Now()
		if now.Sub(t.maxUpFracLostTs) > lostUpdateDelta {
			atomic.StoreUint32(&t.currentUpFracLost, uint32(t.maxUpFracLost))
			t.maxUpFracLost = 0
			t.maxUpFracLostTs = now
		}
		t.fracLostLock.Unlock()
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
	t.fracLostLock.Lock()
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
	t.fracLostLock.Unlock()

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
	t.lock.RLock()
	for _, track := range t.subscribedTracks {
		dt := track.dt.DebugInfo()
		dt["PubMuted"] = track.pubMuted.Get()
		dt["SubMuted"] = track.subMuted.Get()
		subscribedTrackInfo = append(subscribedTrackInfo, dt)
	}
	t.lock.RUnlock()
	info["DownTracks"] = subscribedTrackInfo

	if t.receiver != nil {
		receiverInfo := t.receiver.DebugInfo()
		for k, v := range receiverInfo {
			info[k] = v
		}
	}

	return info
}

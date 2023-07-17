package sfu

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/transport/v2/packetio"
	"github.com/pion/webrtc/v3"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/connectionquality"
	dd "github.com/livekit/livekit-server/pkg/sfu/dependencydescriptor"
	"github.com/livekit/livekit-server/pkg/sfu/pacer"
)

// TrackSender defines an interface send media to remote peer
type TrackSender interface {
	UpTrackLayersChange()
	UpTrackBitrateAvailabilityChange()
	UpTrackMaxPublishedLayerChange(maxPublishedLayer int32)
	UpTrackMaxTemporalLayerSeenChange(maxTemporalLayerSeen int32)
	UpTrackBitrateReport(availableLayers []int32, bitrates Bitrates)
	WriteRTP(p *buffer.ExtPacket, layer int32) error
	Close()
	IsClosed() bool
	// ID is the globally unique identifier for this Track.
	ID() string
	SubscriberID() livekit.ParticipantID
	TrackInfoAvailable()
	HandleRTCPSenderReportData(payloadType webrtc.PayloadType, layer int32, srData *buffer.RTCPSenderReportData) error
}

// -------------------------------------------------------------------

const (
	RTPPaddingMaxPayloadSize      = 255
	RTPPaddingEstimatedHeaderSize = 20
	RTPBlankFramesMuteSeconds     = float32(1.0)
	RTPBlankFramesCloseSeconds    = float32(0.2)

	FlagStopRTXOnPLI = true

	keyFrameIntervalMin = 200
	keyFrameIntervalMax = 1000
	flushTimeout        = 1 * time.Second

	maxPadding = 2000

	waitBeforeSendPaddingOnMute = 100 * time.Millisecond
	maxPaddingOnMuteDuration    = 5 * time.Second
)

// -------------------------------------------------------------------

var (
	ErrUnknownKind                       = errors.New("unknown kind of codec")
	ErrOutOfOrderSequenceNumberCacheMiss = errors.New("out-of-order sequence number not found in cache")
	ErrPaddingOnlyPacket                 = errors.New("padding only packet that need not be forwarded")
	ErrDuplicatePacket                   = errors.New("duplicate packet")
	ErrPaddingNotOnFrameBoundary         = errors.New("padding cannot send on non-frame boundary")
	ErrDownTrackAlreadyBound             = errors.New("already bound")
)

var (
	VP8KeyFrame8x8 = []byte{
		0x10, 0x02, 0x00, 0x9d, 0x01, 0x2a, 0x08, 0x00,
		0x08, 0x00, 0x00, 0x47, 0x08, 0x85, 0x85, 0x88,
		0x85, 0x84, 0x88, 0x02, 0x02, 0x00, 0x0c, 0x0d,
		0x60, 0x00, 0xfe, 0xff, 0xab, 0x50, 0x80,
	}

	H264KeyFrame2x2SPS = []byte{
		0x67, 0x42, 0xc0, 0x1f, 0x0f, 0xd9, 0x1f, 0x88,
		0x88, 0x84, 0x00, 0x00, 0x03, 0x00, 0x04, 0x00,
		0x00, 0x03, 0x00, 0xc8, 0x3c, 0x60, 0xc9, 0x20,
	}
	H264KeyFrame2x2PPS = []byte{
		0x68, 0x87, 0xcb, 0x83, 0xcb, 0x20,
	}
	H264KeyFrame2x2IDR = []byte{
		0x65, 0x88, 0x84, 0x0a, 0xf2, 0x62, 0x80, 0x00,
		0xa7, 0xbe,
	}
	H264KeyFrame2x2 = [][]byte{H264KeyFrame2x2SPS, H264KeyFrame2x2PPS, H264KeyFrame2x2IDR}

	OpusSilenceFrame = []byte{
		0xf8, 0xff, 0xfe, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
)

// -------------------------------------------------------------------

type DownTrackState struct {
	RTPStats                       *buffer.RTPStats
	DeltaStatsSnapshotId           uint32
	DeltaStatsOverriddenSnapshotId uint32
	ForwarderState                 ForwarderState
}

func (d DownTrackState) String() string {
	return fmt.Sprintf("DownTrackState{rtpStats: %s, delta: %d, deltaOverridden: %d, forwarder: %s}",
		d.RTPStats.ToString(), d.DeltaStatsSnapshotId, d.DeltaStatsOverriddenSnapshotId, d.ForwarderState.String())
}

// -------------------------------------------------------------------

type NackInfo struct {
	Timestamp      uint32
	SequenceNumber uint16
	Attempts       uint8
}

type DownTrackStreamAllocatorListener interface {
	// RTCP received
	OnREMB(dt *DownTrack, remb *rtcp.ReceiverEstimatedMaximumBitrate)
	OnTransportCCFeedback(dt *DownTrack, cc *rtcp.TransportLayerCC)

	// video layer availability changed
	OnAvailableLayersChanged(dt *DownTrack)

	// video layer bitrate availability changed
	OnBitrateAvailabilityChanged(dt *DownTrack)

	// max published spatial layer changed
	OnMaxPublishedSpatialChanged(dt *DownTrack)

	// max published temporal layer changed
	OnMaxPublishedTemporalChanged(dt *DownTrack)

	// subscription changed - mute/unmute
	OnSubscriptionChanged(dt *DownTrack)

	// subscribed max video layer changed
	OnSubscribedLayerChanged(dt *DownTrack, layers buffer.VideoLayer)

	// stream resumed
	OnResume(dt *DownTrack)

	// packet(s) sent
	OnPacketsSent(dt *DownTrack, size int)

	// NACKs received
	OnNACK(dt *DownTrack, nackInfos []NackInfo)

	// RTCP Receiver Report received
	OnRTCPReceiverReport(dt *DownTrack, rr rtcp.ReceptionReport)
}

type ReceiverReportListener func(dt *DownTrack, report *rtcp.ReceiverReport)

// DownTrack implements TrackLocal, is the track used to write packets
// to SFU Subscriber, the track handle the packets for simple, simulcast
// and SVC Publisher.
// A DownTrack has the following lifecycle
// - new
// - bound / unbound
// - closed
// once closed, a DownTrack cannot be re-used.
type DownTrack struct {
	logger        logger.Logger
	id            livekit.TrackID
	subscriberID  livekit.ParticipantID
	kind          webrtc.RTPCodecType
	mime          string
	ssrc          uint32
	streamID      string
	maxTrack      int
	payloadType   uint8
	sequencer     *sequencer
	bufferFactory *buffer.Factory

	forwarder *Forwarder

	upstreamCodecs            []webrtc.RTPCodecParameters
	codec                     webrtc.RTPCodecCapability
	absSendTimeExtID          int
	transportWideExtID        int
	dependencyDescriptorExtID int
	receiver                  TrackReceiver
	transceiver               *webrtc.RTPTransceiver
	writeStream               webrtc.TrackLocalWriter
	rtcpReader                *buffer.RTCPReader

	listenerLock            sync.RWMutex
	receiverReportListeners []ReceiverReportListener

	bindLock  sync.Mutex
	bound     atomic.Bool
	onBinding func(error)

	isClosed             atomic.Bool
	connected            atomic.Bool
	bindAndConnectedOnce atomic.Bool

	rtpStats *buffer.RTPStats

	totalRepeatedNACKs atomic.Uint32

	keyFrameRequestGeneration atomic.Uint32

	blankFramesGeneration atomic.Uint32

	connectionStats                *connectionquality.ConnectionStats
	deltaStatsSnapshotId           uint32
	deltaStatsOverriddenSnapshotId uint32

	isNACKThrottled atomic.Bool

	activePaddingOnMuteUpTrack atomic.Bool

	streamAllocatorLock             sync.RWMutex
	streamAllocatorListener         DownTrackStreamAllocatorListener
	streamAllocatorReportGeneration int
	streamAllocatorBytesCounter     atomic.Uint32
	bytesSent                       atomic.Uint32
	bytesRetransmitted              atomic.Uint32

	pacer pacer.Pacer

	maxLayerNotifierCh chan struct{}

	cbMu                        sync.RWMutex
	onStatsUpdate               func(dt *DownTrack, stat *livekit.AnalyticsStat)
	onMaxSubscribedLayerChanged func(dt *DownTrack, layer int32)
	onRttUpdate                 func(dt *DownTrack, rtt uint32)
	onCloseHandler              func(willBeResumed bool)
}

// NewDownTrack returns a DownTrack.
func NewDownTrack(
	codecs []webrtc.RTPCodecParameters,
	r TrackReceiver,
	bf *buffer.Factory,
	subID livekit.ParticipantID,
	mt int,
	pacer pacer.Pacer,
	logger logger.Logger,
) (*DownTrack, error) {
	var kind webrtc.RTPCodecType
	switch {
	case strings.HasPrefix(codecs[0].MimeType, "audio/"):
		kind = webrtc.RTPCodecTypeAudio
	case strings.HasPrefix(codecs[0].MimeType, "video/"):
		kind = webrtc.RTPCodecTypeVideo
	default:
		kind = webrtc.RTPCodecType(0)
	}

	d := &DownTrack{
		logger:             logger,
		id:                 r.TrackID(),
		subscriberID:       subID,
		maxTrack:           mt,
		streamID:           r.StreamID(),
		bufferFactory:      bf,
		receiver:           r,
		upstreamCodecs:     codecs,
		kind:               kind,
		codec:              codecs[0].RTPCodecCapability,
		pacer:              pacer,
		maxLayerNotifierCh: make(chan struct{}, 20),
	}
	d.forwarder = NewForwarder(
		d.kind,
		d.logger,
		d.receiver.GetReferenceLayerRTPTimestamp,
		d.getExpectedRTPTimestamp,
	)
	d.forwarder.OnParkedLayerExpired(func() {
		if sal := d.getStreamAllocatorListener(); sal != nil {
			sal.OnSubscriptionChanged(d)
		}
	})

	d.rtpStats = buffer.NewRTPStats(buffer.RTPStatsParams{
		ClockRate:              d.codec.ClockRate,
		IsReceiverReportDriven: true,
		Logger:                 d.logger,
	})
	d.deltaStatsSnapshotId = d.rtpStats.NewSnapshotId()
	d.deltaStatsOverriddenSnapshotId = d.rtpStats.NewSnapshotId()

	d.connectionStats = connectionquality.NewConnectionStats(connectionquality.ConnectionStatsParams{
		MimeType:                  codecs[0].MimeType, // LK-TODO have to notify on codec change
		IsFECEnabled:              strings.EqualFold(codecs[0].MimeType, webrtc.MimeTypeOpus) && strings.Contains(strings.ToLower(codecs[0].SDPFmtpLine), "fec"),
		GetDeltaStats:             d.getDeltaStats,
		GetDeltaStatsOverridden:   d.getDeltaStatsOverridden,
		GetLastReceiverReportTime: func() time.Time { return d.rtpStats.LastReceiverReport() },
		Logger:                    d.logger.WithValues("direction", "down"),
	})
	d.connectionStats.OnStatsUpdate(func(_cs *connectionquality.ConnectionStats, stat *livekit.AnalyticsStat) {
		if onStatsUpdate := d.getOnStatsUpdate(); onStatsUpdate != nil {
			onStatsUpdate(d, stat)
		}
	})

	if d.kind == webrtc.RTPCodecTypeVideo {
		go d.maxLayerNotifierWorker()
	}

	return d, nil
}

// Bind is called by the PeerConnection after negotiation is complete
// This asserts that the code requested is supported by the remote peer.
// If so it sets up all the state (SSRC and PayloadType) to have a call
func (d *DownTrack) Bind(t webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	d.bindLock.Lock()
	if d.bound.Load() {
		d.bindLock.Unlock()
		return webrtc.RTPCodecParameters{}, ErrDownTrackAlreadyBound
	}
	var codec webrtc.RTPCodecParameters
	for _, c := range d.upstreamCodecs {
		matchCodec, err := codecParametersFuzzySearch(c, t.CodecParameters())
		if err == nil {
			codec = matchCodec
			break
		}
	}

	if codec.MimeType == "" {
		err := webrtc.ErrUnsupportedCodec
		onBinding := d.onBinding
		d.bindLock.Unlock()
		d.logger.Infow("bind error for unsupported codec", "codecs", d.upstreamCodecs, "remoteParameters", t.CodecParameters())
		if onBinding != nil {
			onBinding(err)
		}
		return webrtc.RTPCodecParameters{}, err
	}

	// if a downtrack is closed before bind, it already unsubscribed from client, don't do subsequent operation and return here.
	if d.IsClosed() {
		d.logger.Debugw("DownTrack closed before bind")
		d.bindLock.Unlock()
		return codec, nil
	}

	d.logger.Debugw("DownTrack.Bind", "codecs", d.upstreamCodecs, "matchCodec", codec, "ssrc", t.SSRC())
	d.ssrc = uint32(t.SSRC())
	d.payloadType = uint8(codec.PayloadType)
	d.writeStream = t.WriteStream()
	d.mime = strings.ToLower(codec.MimeType)
	if rr := d.bufferFactory.GetOrNew(packetio.RTCPBufferPacket, uint32(t.SSRC())).(*buffer.RTCPReader); rr != nil {
		rr.OnPacket(func(pkt []byte) {
			d.handleRTCP(pkt)
		})
		d.rtcpReader = rr
	}

	if d.kind == webrtc.RTPCodecTypeAudio {
		d.sequencer = newSequencer(d.maxTrack, 0, d.logger)
	} else {
		d.sequencer = newSequencer(d.maxTrack, maxPadding, d.logger)
	}

	d.codec = codec.RTPCodecCapability
	if d.onBinding != nil {
		d.onBinding(nil)
	}
	d.bound.Store(true)
	d.bindLock.Unlock()

	d.forwarder.DetermineCodec(d.codec, d.receiver.HeaderExtensions())

	d.logger.Debugw("downtrack bound")
	d.onBindAndConnected()

	return codec, nil
}

// Unbind implements the teardown logic when the track is no longer needed. This happens
// because a track has been stopped.
func (d *DownTrack) Unbind(_ webrtc.TrackLocalContext) error {
	d.bound.Store(false)
	return nil
}

func (d *DownTrack) TrackInfoAvailable() {
	ti := d.receiver.TrackInfo()
	if ti == nil {
		return
	}
	d.connectionStats.Start(ti)
}

func (d *DownTrack) SetStreamAllocatorListener(listener DownTrackStreamAllocatorListener) {
	d.streamAllocatorLock.Lock()
	d.streamAllocatorListener = listener
	d.streamAllocatorLock.Unlock()

	// kick of a gratuitous allocation
	if listener != nil {
		listener.OnSubscriptionChanged(d)
	}
}

func (d *DownTrack) getStreamAllocatorListener() DownTrackStreamAllocatorListener {
	d.streamAllocatorLock.RLock()
	defer d.streamAllocatorLock.RUnlock()

	return d.streamAllocatorListener
}

func (d *DownTrack) SetStreamAllocatorReportInterval(interval time.Duration) {
	d.ClearStreamAllocatorReportInterval()

	if interval == 0 {
		return
	}

	d.streamAllocatorLock.Lock()
	d.streamAllocatorBytesCounter.Store(0)

	d.streamAllocatorReportGeneration++
	gen := d.streamAllocatorReportGeneration
	d.streamAllocatorLock.Unlock()

	go func(generation int) {
		timer := time.NewTimer(interval)
		for {
			<-timer.C

			d.streamAllocatorLock.Lock()
			if generation != d.streamAllocatorReportGeneration {
				d.streamAllocatorLock.Unlock()
				return
			}

			sal := d.streamAllocatorListener
			bytes := d.streamAllocatorBytesCounter.Swap(0)
			d.streamAllocatorLock.Unlock()

			if sal != nil {
				sal.OnPacketsSent(d, int(bytes))
			}

			timer.Reset(interval)
		}
	}(gen)
}

func (d *DownTrack) ClearStreamAllocatorReportInterval() {
	d.streamAllocatorLock.Lock()
	d.streamAllocatorReportGeneration++
	d.streamAllocatorLock.Unlock()
}

// ID is the unique identifier for this Track. This should be unique for the
// stream, but doesn't have to globally unique. A common example would be 'audio' or 'video'
// and StreamID would be 'desktop' or 'webcam'
func (d *DownTrack) ID() string { return string(d.id) }

// Codec returns current track codec capability
func (d *DownTrack) Codec() webrtc.RTPCodecCapability { return d.codec }

// StreamID is the group this track belongs too. This must be unique
func (d *DownTrack) StreamID() string { return d.streamID }

func (d *DownTrack) SubscriberID() livekit.ParticipantID { return d.subscriberID }

// Sets RTP header extensions for this track
func (d *DownTrack) SetRTPHeaderExtensions(rtpHeaderExtensions []webrtc.RTPHeaderExtensionParameter) {
	for _, ext := range rtpHeaderExtensions {
		switch ext.URI {
		case sdp.ABSSendTimeURI:
			d.absSendTimeExtID = ext.ID
		case sdp.TransportCCURI:
			d.transportWideExtID = ext.ID
		case dd.ExtensionUrl:
			d.dependencyDescriptorExtID = ext.ID
		}
	}
}

// Kind controls if this TrackLocal is audio or video
func (d *DownTrack) Kind() webrtc.RTPCodecType {
	return d.kind
}

// RID is required by `webrtc.TrackLocal` interface
func (d *DownTrack) RID() string {
	return ""
}

func (d *DownTrack) SSRC() uint32 {
	return d.ssrc
}

func (d *DownTrack) Stop() error {
	if d.transceiver != nil {
		return d.transceiver.Stop()
	}
	return errors.New("downtrack transceiver does not exist")
}

func (d *DownTrack) SetTransceiver(transceiver *webrtc.RTPTransceiver) {
	d.transceiver = transceiver
}

func (d *DownTrack) GetTransceiver() *webrtc.RTPTransceiver {
	return d.transceiver
}

func (d *DownTrack) maybeStartKeyFrameRequester() {
	//
	// Always move to next generation to abandon any running key frame requester
	// This ensures that it is stopped if forwarding is disabled due to mute
	// or paused due to bandwidth constraints. A new key frame requester is
	// started if a layer lock is required.
	//
	d.stopKeyFrameRequester()

	// SVC-TODO : don't need pli/lrr when layer comes down
	locked, layer := d.forwarder.CheckSync()
	if !locked {
		go d.keyFrameRequester(d.keyFrameRequestGeneration.Load(), layer)
	}
}

func (d *DownTrack) stopKeyFrameRequester() {
	d.keyFrameRequestGeneration.Inc()
}

func (d *DownTrack) keyFrameRequester(generation uint32, layer int32) {
	if d.IsClosed() || layer == buffer.InvalidLayerSpatial {
		return
	}

	interval := 2 * d.rtpStats.GetRtt()
	if interval < keyFrameIntervalMin {
		interval = keyFrameIntervalMin
	}
	if interval > keyFrameIntervalMax {
		interval = keyFrameIntervalMax
	}
	ticker := time.NewTicker(time.Duration(interval) * time.Millisecond)
	defer ticker.Stop()

	for {
		locked, _ := d.forwarder.CheckSync()
		if locked {
			return
		}

		if d.connected.Load() {
			d.logger.Debugw("sending PLI for layer lock", "generation", generation, "layer", layer)
			d.receiver.SendPLI(layer, false)
			d.rtpStats.UpdateLayerLockPliAndTime(1)
		}

		<-ticker.C

		if generation != d.keyFrameRequestGeneration.Load() || !d.bound.Load() {
			return
		}
	}
}

func (d *DownTrack) postMaxLayerNotifierEvent() {
	if d.IsClosed() {
		return
	}

	select {
	case d.maxLayerNotifierCh <- struct{}{}:
	default:
		d.logger.Warnw("max layer notifier event queue full", nil)
	}
}

func (d *DownTrack) maxLayerNotifierWorker() {
	more := true
	for more {
		_, more = <-d.maxLayerNotifierCh

		maxLayerSpatial := buffer.InvalidLayerSpatial
		if more {
			maxLayerSpatial = d.forwarder.GetMaxSubscribedSpatial()
		}
		if onMaxSubscribedLayerChanged := d.getOnMaxLayerChanged(); onMaxSubscribedLayerChanged != nil {
			d.logger.Infow("max subscribed layer changed", "maxLayerSpatial", maxLayerSpatial)
			onMaxSubscribedLayerChanged(d, maxLayerSpatial)
		}
	}
}

// WriteRTP writes an RTP Packet to the DownTrack
func (d *DownTrack) WriteRTP(extPkt *buffer.ExtPacket, layer int32) error {
	if !d.bound.Load() || !d.connected.Load() {
		return nil
	}

	tp, err := d.forwarder.GetTranslationParams(extPkt, layer)
	if tp.shouldDrop {
		if err != nil {
			d.logger.Errorw("write rtp packet failed", err)
		}
		return err
	}

	var payload []byte
	pool := PacketFactory.Get().(*[]byte)
	if len(tp.codecBytes) != 0 {
		incomingVP8, _ := extPkt.Payload.(buffer.VP8)
		payload = d.translateVP8PacketTo(extPkt.Packet, &incomingVP8, tp.codecBytes, pool)
	}
	if payload == nil {
		payload = (*pool)[:len(extPkt.Packet.Payload)]
		copy(payload, extPkt.Packet.Payload)
	}

	if d.sequencer != nil {
		d.sequencer.push(
			extPkt.Packet.SequenceNumber,
			tp.rtp.sequenceNumber,
			tp.rtp.timestamp,
			int8(layer),
			tp.codecBytes,
			tp.ddBytes,
		)
	}

	hdr, err := d.getTranslatedRTPHeader(extPkt, tp)
	if err != nil {
		d.logger.Errorw("write rtp packet failed", err)
		if pool != nil {
			PacketFactory.Put(pool)
		}
		return err
	}

	d.pacer.Enqueue(pacer.Packet{
		Header:             hdr,
		Extensions:         []pacer.ExtensionData{{ID: uint8(d.dependencyDescriptorExtID), Payload: tp.ddBytes}},
		Payload:            payload,
		AbsSendTimeExtID:   uint8(d.absSendTimeExtID),
		TransportWideExtID: uint8(d.transportWideExtID),
		WriteStream:        d.writeStream,
		Metadata: sendPacketMetadata{
			layer:      layer,
			arrival:    extPkt.Arrival,
			isKeyFrame: extPkt.KeyFrame,
			tp:         tp,
			pool:       pool,
		},
		OnSent: d.packetSent,
	})
	return nil
}

// WritePaddingRTP tries to write as many padding only RTP packets as necessary
// to satisfy given size to the DownTrack
func (d *DownTrack) WritePaddingRTP(bytesToSend int, paddingOnMute bool, forceMarker bool) int {
	if !d.rtpStats.IsActive() && !paddingOnMute {
		return 0
	}

	// LK-TODO-START
	// Ideally should look at header extensions negotiated for
	// track and decide if padding can be sent. But, browsers behave
	// in unexpected ways when using audio for bandwidth estimation and
	// padding is mainly used to probe for excess available bandwidth.
	// So, to be safe, limit to video tracks
	// LK-TODO-END
	if d.kind == webrtc.RTPCodecTypeAudio {
		return 0
	}

	// LK-TODO-START
	// Potentially write padding even if muted. Given that padding
	// can be sent only on frame boundaries, writing on disabled tracks
	// will give more options.
	// LK-TODO-END
	if d.forwarder.IsMuted() && !paddingOnMute {
		return 0
	}

	// RTP padding maximum is 255 bytes. Break it up.
	// Use 20 byte as estimate of RTP header size (12 byte header + 8 byte extension)
	num := (bytesToSend + RTPPaddingMaxPayloadSize + RTPPaddingEstimatedHeaderSize - 1) / (RTPPaddingMaxPayloadSize + RTPPaddingEstimatedHeaderSize)
	if num == 0 {
		return 0
	}

	snts, err := d.forwarder.GetSnTsForPadding(num, forceMarker)
	if err != nil {
		return 0
	}

	// LK-TODO Look at load balancing a la sfu.Receiver to spread across available CPUs
	bytesSent := 0
	for i := 0; i < len(snts); i++ {
		// LK-TODO-START
		// Hold sending padding packets till first RTCP-RR is received for this RTP stream.
		// That is definitive proof that the remote side knows about this RTP stream.
		// The packet count check at the beginning of this function gates sending padding
		// on as yet unstarted streams which is a reasonable check.
		// LK-TODO-END

		hdr := rtp.Header{
			Version:        2,
			Padding:        true,
			Marker:         false,
			PayloadType:    d.payloadType,
			SequenceNumber: snts[i].sequenceNumber,
			Timestamp:      snts[i].timestamp,
			SSRC:           d.ssrc,
			CSRC:           []uint32{},
		}

		payload := make([]byte, RTPPaddingMaxPayloadSize)
		// last byte of padding has padding size including that byte
		payload[RTPPaddingMaxPayloadSize-1] = byte(RTPPaddingMaxPayloadSize)

		d.pacer.Enqueue(pacer.Packet{
			Header:             &hdr,
			Payload:            payload,
			AbsSendTimeExtID:   uint8(d.absSendTimeExtID),
			TransportWideExtID: uint8(d.transportWideExtID),
			WriteStream:        d.writeStream,
			Metadata: sendPacketMetadata{
				isPadding:       true,
				disableCounter:  true,
				disableRTPStats: paddingOnMute,
			},
			OnSent: d.packetSent,
		})

		//
		// Register with sequencer with invalid layer so that NACKs for these can be filtered out.
		// Retransmission is probably a sign of network congestion/badness.
		// So, retransmitting padding packets is only going to make matters worse.
		//
		if d.sequencer != nil {
			d.sequencer.pushPadding(hdr.SequenceNumber)
		}

		bytesSent += hdr.MarshalSize() + len(payload)
	}

	// STREAM_ALLOCATOR-TODO: change this to pull this counter from stream allocator so that counter can be update in pacer callback
	return bytesSent
}

// Mute enables or disables media forwarding - subscriber triggered
func (d *DownTrack) Mute(muted bool) {
	changed := d.forwarder.Mute(muted)
	d.handleMute(muted, changed)
}

// PubMute enables or disables media forwarding - publisher side
func (d *DownTrack) PubMute(pubMuted bool) {
	changed := d.forwarder.PubMute(pubMuted)
	d.handleMute(pubMuted, changed)
}

func (d *DownTrack) handleMute(muted bool, changed bool) {
	if !changed {
		return
	}

	d.connectionStats.UpdateMute(d.forwarder.IsAnyMuted())

	//
	// Subscriber mute changes trigger a max layer notification.
	// That could result in encoding layers getting turned on/off on publisher side
	// (depending on aggregate layer requirements of all subscribers of the track).
	//
	// Publisher mute changes should not trigger notification.
	// If publisher turns off all layers because of subscribers indicating
	// no layers required due to publisher mute (bit of circular dependency),
	// there will be a delay in layers turning back on when unmute happens.
	// Unmute path will require
	//   1. unmute signalling out-of-band from publisher received by down track(s)
	//   2. down track(s) notifying max layer
	//   3. out-of-band notification about max layer sent back to the publisher
	//   4. publisher starts layer(s)
	// Ideally, on publisher mute, whatever layers were active remain active and
	// can be restarted by publisher immediately on unmute.
	//
	// Note that while publisher mute is active, subscriber changes can also happen
	// and that could turn on/off layers on publisher side.
	//
	d.postMaxLayerNotifierEvent()

	if sal := d.getStreamAllocatorListener(); sal != nil {
		sal.OnSubscriptionChanged(d)
	}

	// when muting, send a few silence frames to ensure residual noise does not
	// put the comfort noise generator on decoder side in a bad state where it
	// generates noise that is not so comfortable.
	//
	// One possibility is not to inject blank frames when publisher is muted
	// and let forwarding continue. When publisher is muted, unless the media
	// stream is stopped, publisher will send silence frames which should have
	// comfort noise information. But, in case the publisher stops at an
	// inopportune frame (due to media stream stop or injecting audio from a file),
	// the decoder could be in a noisy state. So, inject blank frames on publisher
	// mute too.
	d.blankFramesGeneration.Inc()
	if d.kind == webrtc.RTPCodecTypeAudio && muted {
		d.writeBlankFrameRTP(RTPBlankFramesMuteSeconds, d.blankFramesGeneration.Load())
	}
}

func (d *DownTrack) IsClosed() bool {
	return d.isClosed.Load()
}

func (d *DownTrack) Close() {
	d.CloseWithFlush(true)
}

// CloseWithFlush - flush used to indicate whether send blank frame to flush
// decoder of client.
//  1. When transceiver is reused by other participant's video track,
//     set flush=true to avoid previous video shows before new stream is displayed.
//  2. in case of session migration, participant migrate from other node, video track should
//     be resumed with same participant, set flush=false since we don't need to flush decoder.
func (d *DownTrack) CloseWithFlush(flush bool) {
	if d.isClosed.Swap(true) {
		// already closed
		return
	}

	d.bindLock.Lock()
	d.logger.Debugw("close down track", "flushBlankFrame", flush)
	if d.bound.Load() {
		if d.forwarder != nil {
			d.forwarder.Mute(true)
		}
		// write blank frames after disabling so that other frames do not interfere.
		// Idea here is to send blank key frames to flush the decoder buffer at the remote end.
		// Otherwise, with transceiver re-use last frame from previous stream is held in the
		// display buffer and there could be a brief moment where the previous stream is displayed.
		if flush {
			doneFlushing := d.writeBlankFrameRTP(RTPBlankFramesCloseSeconds, d.blankFramesGeneration.Inc())

			// wait a limited time to flush
			timer := time.NewTimer(flushTimeout)
			defer timer.Stop()

			select {
			case <-doneFlushing:
			case <-timer.C:
				d.blankFramesGeneration.Inc() // in case flush is still running
			}
		}

		d.bound.Store(false)
		d.logger.Debugw("closing sender", "kind", d.kind)
	}
	d.receiver.DeleteDownTrack(d.subscriberID)

	if d.rtcpReader != nil && flush {
		d.logger.Debugw("downtrack close rtcp reader")
		d.rtcpReader.Close()
		d.rtcpReader.OnPacket(nil)
	}

	d.bindLock.Unlock()
	d.connectionStats.Close()
	d.rtpStats.Stop()
	d.logger.Infow("rtp stats", "direction", "downstream", "mime", d.mime, "ssrc", d.ssrc, "stats", d.rtpStats.ToString())

	close(d.maxLayerNotifierCh)

	if onCloseHandler := d.getOnCloseHandler(); onCloseHandler != nil {
		onCloseHandler(!flush)
	}

	d.stopKeyFrameRequester()
	d.ClearStreamAllocatorReportInterval()
}

func (d *DownTrack) SetMaxSpatialLayer(spatialLayer int32) {
	changed, maxLayer := d.forwarder.SetMaxSpatialLayer(spatialLayer)
	if !changed {
		return
	}

	d.postMaxLayerNotifierEvent()

	if sal := d.getStreamAllocatorListener(); sal != nil {
		sal.OnSubscribedLayerChanged(d, maxLayer)
	}
}

func (d *DownTrack) SetMaxTemporalLayer(temporalLayer int32) {
	changed, maxLayer := d.forwarder.SetMaxTemporalLayer(temporalLayer)
	if !changed {
		return
	}

	if sal := d.getStreamAllocatorListener(); sal != nil {
		sal.OnSubscribedLayerChanged(d, maxLayer)
	}
}

func (d *DownTrack) MaxLayer() buffer.VideoLayer {
	return d.forwarder.MaxLayer()
}

func (d *DownTrack) GetState() DownTrackState {
	dts := DownTrackState{
		RTPStats:                       d.rtpStats,
		DeltaStatsSnapshotId:           d.deltaStatsSnapshotId,
		DeltaStatsOverriddenSnapshotId: d.deltaStatsOverriddenSnapshotId,
		ForwarderState:                 d.forwarder.GetState(),
	}
	return dts
}

func (d *DownTrack) SeedState(state DownTrackState) {
	d.rtpStats.Seed(state.RTPStats)
	d.deltaStatsSnapshotId = state.DeltaStatsSnapshotId
	d.deltaStatsOverriddenSnapshotId = state.DeltaStatsOverriddenSnapshotId
	d.forwarder.SeedState(state.ForwarderState)
}

func (d *DownTrack) UpTrackLayersChange() {
	if sal := d.getStreamAllocatorListener(); sal != nil {
		sal.OnAvailableLayersChanged(d)
	}
}

func (d *DownTrack) UpTrackBitrateAvailabilityChange() {
	if sal := d.getStreamAllocatorListener(); sal != nil {
		sal.OnBitrateAvailabilityChanged(d)
	}
}

func (d *DownTrack) UpTrackMaxPublishedLayerChange(maxPublishedLayer int32) {
	if d.forwarder.SetMaxPublishedLayer(maxPublishedLayer) {
		if sal := d.getStreamAllocatorListener(); sal != nil {
			sal.OnMaxPublishedSpatialChanged(d)
		}
	}
}

func (d *DownTrack) UpTrackMaxTemporalLayerSeenChange(maxTemporalLayerSeen int32) {
	if d.forwarder.SetMaxTemporalLayerSeen(maxTemporalLayerSeen) {
		if sal := d.getStreamAllocatorListener(); sal != nil {
			sal.OnMaxPublishedTemporalChanged(d)
		}
	}
}

func (d *DownTrack) maybeAddTransition(_bitrate int64, distance float64, pauseReason VideoPauseReason) {
	if d.kind == webrtc.RTPCodecTypeAudio {
		return
	}

	if pauseReason == VideoPauseReasonBandwidth {
		d.connectionStats.UpdatePause(true)
	} else {
		d.connectionStats.UpdatePause(false)
		d.connectionStats.AddLayerTransition(distance)
	}
}

func (d *DownTrack) UpTrackBitrateReport(availableLayers []int32, bitrates Bitrates) {
	d.maybeAddTransition(
		d.forwarder.GetOptimalBandwidthNeeded(bitrates),
		d.forwarder.DistanceToDesired(availableLayers, bitrates),
		d.forwarder.PauseReason(),
	)
}

// OnCloseHandler method to be called on remote tracked removed
func (d *DownTrack) OnCloseHandler(fn func(willBeResumed bool)) {
	d.cbMu.Lock()
	defer d.cbMu.Unlock()

	d.onCloseHandler = fn
}

func (d *DownTrack) getOnCloseHandler() func(willBeResumed bool) {
	d.cbMu.RLock()
	defer d.cbMu.RUnlock()

	return d.onCloseHandler
}

func (d *DownTrack) OnBinding(fn func(error)) {
	d.bindLock.Lock()
	defer d.bindLock.Unlock()

	d.onBinding = fn
}

func (d *DownTrack) AddReceiverReportListener(listener ReceiverReportListener) {
	d.listenerLock.Lock()
	defer d.listenerLock.Unlock()

	d.receiverReportListeners = append(d.receiverReportListeners, listener)
}

func (d *DownTrack) OnStatsUpdate(fn func(dt *DownTrack, stat *livekit.AnalyticsStat)) {
	d.cbMu.Lock()
	defer d.cbMu.Unlock()

	d.onStatsUpdate = fn
}

func (d *DownTrack) getOnStatsUpdate() func(dt *DownTrack, stat *livekit.AnalyticsStat) {
	d.cbMu.RLock()
	defer d.cbMu.RUnlock()

	return d.onStatsUpdate
}

func (d *DownTrack) OnRttUpdate(fn func(dt *DownTrack, rtt uint32)) {
	d.cbMu.Lock()
	defer d.cbMu.Unlock()

	d.onRttUpdate = fn
}

func (d *DownTrack) getOnRttUpdate() func(dt *DownTrack, rtt uint32) {
	d.cbMu.RLock()
	defer d.cbMu.RUnlock()

	return d.onRttUpdate
}

func (d *DownTrack) OnMaxLayerChanged(fn func(dt *DownTrack, layer int32)) {
	d.cbMu.Lock()
	defer d.cbMu.Unlock()

	d.onMaxSubscribedLayerChanged = fn
}

func (d *DownTrack) getOnMaxLayerChanged() func(dt *DownTrack, layer int32) {
	d.cbMu.RLock()
	defer d.cbMu.RUnlock()

	return d.onMaxSubscribedLayerChanged
}

func (d *DownTrack) IsDeficient() bool {
	return d.forwarder.IsDeficient()
}

func (d *DownTrack) BandwidthRequested() int64 {
	_, brs := d.receiver.GetLayeredBitrate()
	return d.forwarder.BandwidthRequested(brs)
}

func (d *DownTrack) DistanceToDesired() float64 {
	al, brs := d.receiver.GetLayeredBitrate()
	return d.forwarder.DistanceToDesired(al, brs)
}

func (d *DownTrack) AllocateOptimal(allowOvershoot bool) VideoAllocation {
	al, brs := d.receiver.GetLayeredBitrate()
	allocation := d.forwarder.AllocateOptimal(al, brs, allowOvershoot)
	d.maybeStartKeyFrameRequester()
	d.maybeAddTransition(allocation.BandwidthNeeded, allocation.DistanceToDesired, allocation.PauseReason)
	return allocation
}

func (d *DownTrack) ProvisionalAllocatePrepare() {
	al, brs := d.receiver.GetLayeredBitrate()
	d.forwarder.ProvisionalAllocatePrepare(al, brs)
}

func (d *DownTrack) ProvisionalAllocate(availableChannelCapacity int64, layers buffer.VideoLayer, allowPause bool, allowOvershoot bool) int64 {
	return d.forwarder.ProvisionalAllocate(availableChannelCapacity, layers, allowPause, allowOvershoot)
}

func (d *DownTrack) ProvisionalAllocateGetCooperativeTransition(allowOvershoot bool) VideoTransition {
	transition := d.forwarder.ProvisionalAllocateGetCooperativeTransition(allowOvershoot)
	d.logger.Debugw("stream: cooperative transition", "transition", transition)
	return transition
}

func (d *DownTrack) ProvisionalAllocateGetBestWeightedTransition() VideoTransition {
	transition := d.forwarder.ProvisionalAllocateGetBestWeightedTransition()
	d.logger.Debugw("stream: best weighted transition", "transition", transition)
	return transition
}

func (d *DownTrack) ProvisionalAllocateCommit() VideoAllocation {
	allocation := d.forwarder.ProvisionalAllocateCommit()
	d.maybeStartKeyFrameRequester()
	d.maybeAddTransition(allocation.BandwidthNeeded, allocation.DistanceToDesired, allocation.PauseReason)
	return allocation
}

func (d *DownTrack) AllocateNextHigher(availableChannelCapacity int64, allowOvershoot bool) (VideoAllocation, bool) {
	al, brs := d.receiver.GetLayeredBitrate()
	allocation, available := d.forwarder.AllocateNextHigher(availableChannelCapacity, al, brs, allowOvershoot)
	d.maybeStartKeyFrameRequester()
	d.maybeAddTransition(allocation.BandwidthNeeded, allocation.DistanceToDesired, allocation.PauseReason)
	return allocation, available
}

func (d *DownTrack) GetNextHigherTransition(allowOvershoot bool) (VideoTransition, bool) {
	_, brs := d.receiver.GetLayeredBitrate()
	transition, available := d.forwarder.GetNextHigherTransition(brs, allowOvershoot)
	d.logger.Debugw("stream: get next higher layer", "transition", transition, "available", available, "bitrates", brs)
	return transition, available
}

func (d *DownTrack) Pause() VideoAllocation {
	al, brs := d.receiver.GetLayeredBitrate()
	allocation := d.forwarder.Pause(al, brs)
	d.maybeStartKeyFrameRequester()
	d.maybeAddTransition(allocation.BandwidthNeeded, allocation.DistanceToDesired, allocation.PauseReason)
	return allocation
}

func (d *DownTrack) Resync() {
	d.forwarder.Resync()
}

func (d *DownTrack) CreateSourceDescriptionChunks() []rtcp.SourceDescriptionChunk {
	if !d.bound.Load() {
		return nil
	}
	return []rtcp.SourceDescriptionChunk{
		{
			Source: d.ssrc,
			Items: []rtcp.SourceDescriptionItem{{
				Type: rtcp.SDESCNAME,
				Text: d.streamID,
			}},
		}, {
			Source: d.ssrc,
			Items: []rtcp.SourceDescriptionItem{{
				Type: rtcp.SDESType(15),
				Text: d.transceiver.Mid(),
			}},
		},
	}
}

func (d *DownTrack) CreateSenderReport() *rtcp.SenderReport {
	if !d.bound.Load() {
		return nil
	}

	return d.rtpStats.GetRtcpSenderReport(d.ssrc, d.receiver.GetCalculatedClockRate(d.forwarder.CurrentLayer().Spatial))
}

func (d *DownTrack) writeBlankFrameRTP(duration float32, generation uint32) chan struct{} {
	done := make(chan struct{})
	go func() {
		// don't send if nothing has been sent
		if !d.rtpStats.IsActive() {
			close(done)
			return
		}

		var getBlankFrame func(bool) ([]byte, error)
		switch d.mime {
		case "audio/opus":
			getBlankFrame = d.getOpusBlankFrame
		case "audio/red":
			getBlankFrame = d.getOpusRedBlankFrame
		case "video/vp8":
			getBlankFrame = d.getVP8BlankFrame
		case "video/h264":
			getBlankFrame = d.getH264BlankFrame
		default:
			close(done)
			return
		}

		frameRate := uint32(30)
		if d.mime == "audio/opus" || d.mime == "audio/red" {
			frameRate = 50
		}

		// send a number of blank frames just in case there is loss.
		// Intentionally ignoring check for mute or bandwidth constrained mute
		// as this is used to clear client side buffer.
		numFrames := int(float32(frameRate) * duration)
		frameDuration := time.Duration(1000/frameRate) * time.Millisecond

		ticker := time.NewTicker(frameDuration)
		defer ticker.Stop()

		for {
			if generation != d.blankFramesGeneration.Load() || numFrames <= 0 {
				close(done)
				return
			}

			snts, frameEndNeeded, err := d.forwarder.GetSnTsForBlankFrames(frameRate, 1)
			if err != nil {
				d.logger.Warnw("could not get SN/TS for blank frame", err)
				close(done)
				return
			}

			for i := 0; i < len(snts); i++ {
				hdr := rtp.Header{
					Version:        2,
					Padding:        false,
					Marker:         true,
					PayloadType:    d.payloadType,
					SequenceNumber: snts[i].sequenceNumber,
					Timestamp:      snts[i].timestamp,
					SSRC:           d.ssrc,
					CSRC:           []uint32{},
				}

				payload, err := getBlankFrame(frameEndNeeded)
				if err != nil {
					d.logger.Warnw("could not get blank frame", err)
					close(done)
					return
				}

				d.pacer.Enqueue(pacer.Packet{
					Header:             &hdr,
					Payload:            payload,
					AbsSendTimeExtID:   uint8(d.absSendTimeExtID),
					TransportWideExtID: uint8(d.transportWideExtID),
					WriteStream:        d.writeStream,
					Metadata: sendPacketMetadata{
						isBlankFrame: true,
					},
					OnSent: d.packetSent,
				})

				// only the first frame will need frameEndNeeded to close out the
				// previous picture, rest are small key frames (for the video case)
				frameEndNeeded = false
			}

			numFrames--
			<-ticker.C
		}
	}()

	return done
}

func (d *DownTrack) getOpusBlankFrame(_frameEndNeeded bool) ([]byte, error) {
	// silence frame
	// Used shortly after muting to ensure residual noise does not keep
	// generating noise at the decoder after the stream is stopped
	// i. e. comfort noise generation actually not producing something comfortable.
	payload := make([]byte, len(OpusSilenceFrame))
	copy(payload[0:], OpusSilenceFrame)
	return payload, nil
}

func (d *DownTrack) getOpusRedBlankFrame(_frameEndNeeded bool) ([]byte, error) {
	// primary only silence frame for opus/red, there is no need to contain redundant silent frames
	payload := make([]byte, len(OpusSilenceFrame)+1)

	// primary header
	//  0 1 2 3 4 5 6 7
	// +-+-+-+-+-+-+-+-+
	// |0|   Block PT  |
	// +-+-+-+-+-+-+-+-+
	payload[0] = opusPT
	copy(payload[1:], OpusSilenceFrame)
	return payload, nil
}

func (d *DownTrack) getVP8BlankFrame(frameEndNeeded bool) ([]byte, error) {
	blankVP8, err := d.forwarder.GetPadding(frameEndNeeded)
	if err != nil {
		return nil, err
	}

	// 8x8 key frame
	// Used even when closing out a previous frame. Looks like receivers
	// do not care about content (it will probably end up being an undecodable
	// frame, but that should be okay as there are key frames following)
	payload := make([]byte, len(blankVP8)+len(VP8KeyFrame8x8))
	copy(payload[:len(blankVP8)], blankVP8)
	copy(payload[len(blankVP8):], VP8KeyFrame8x8)
	return payload, nil
}

func (d *DownTrack) getH264BlankFrame(_frameEndNeeded bool) ([]byte, error) {
	// TODO - Jie Zeng
	// now use STAP-A to compose sps, pps, idr together, most decoder support packetization-mode 1.
	// if client only support packetization-mode 0, use single nalu unit packet
	buf := make([]byte, 1462)
	offset := 0
	buf[0] = 0x18 // STAP-A
	offset++
	for _, payload := range H264KeyFrame2x2 {
		binary.BigEndian.PutUint16(buf[offset:], uint16(len(payload)))
		offset += 2
		copy(buf[offset:offset+len(payload)], payload)
		offset += len(payload)
	}
	payload := buf[:offset]
	return payload, nil
}

func (d *DownTrack) handleRTCP(bytes []byte) {
	pkts, err := rtcp.Unmarshal(bytes)
	if err != nil {
		d.logger.Errorw("unmarshal rtcp receiver packets err", err)
		return
	}

	pliOnce := true
	sendPliOnce := func() {
		if pliOnce {
			_, layer := d.forwarder.CheckSync()
			if layer != buffer.InvalidLayerSpatial && !d.forwarder.IsAnyMuted() {
				d.logger.Debugw("sending PLI RTCP", "layer", layer)
				d.receiver.SendPLI(layer, false)
				d.isNACKThrottled.Store(true)
				d.rtpStats.UpdatePliTime()
				pliOnce = false
			}
		}
	}

	rttToReport := uint32(0)

	var numNACKs uint32
	var numPLIs uint32
	var numFIRs uint32
	for _, pkt := range pkts {
		switch p := pkt.(type) {
		case *rtcp.PictureLossIndication:
			numPLIs++
			sendPliOnce()

		case *rtcp.FullIntraRequest:
			numFIRs++
			sendPliOnce()

		case *rtcp.ReceiverEstimatedMaximumBitrate:
			if sal := d.getStreamAllocatorListener(); sal != nil {
				sal.OnREMB(d, p)
			}

		case *rtcp.ReceiverReport:
			// create new receiver report w/ only valid reception reports
			rr := &rtcp.ReceiverReport{
				SSRC:              p.SSRC,
				ProfileExtensions: p.ProfileExtensions,
			}
			for _, r := range p.Reports {
				if r.SSRC != d.ssrc {
					continue
				}
				rr.Reports = append(rr.Reports, r)

				rtt, isRttChanged := d.rtpStats.UpdateFromReceiverReport(r)
				if isRttChanged {
					rttToReport = rtt
				}

				if sal := d.getStreamAllocatorListener(); sal != nil {
					sal.OnRTCPReceiverReport(d, r)
				}
			}
			if len(rr.Reports) > 0 {
				d.listenerLock.RLock()
				for _, l := range d.receiverReportListeners {
					l(d, rr)
				}
				d.listenerLock.RUnlock()
			}

		case *rtcp.TransportLayerNack:
			var nacks []uint16
			for _, pair := range p.Nacks {
				packetList := pair.PacketList()
				numNACKs += uint32(len(packetList))
				nacks = append(nacks, packetList...)
			}
			go d.retransmitPackets(nacks)

		case *rtcp.TransportLayerCC:
			if p.MediaSSRC == d.ssrc {
				if sal := d.getStreamAllocatorListener(); sal != nil {
					sal.OnTransportCCFeedback(d, p)
				}
			}
		}
	}

	d.rtpStats.UpdateNack(numNACKs)
	d.rtpStats.UpdatePli(numPLIs)
	d.rtpStats.UpdateFir(numFIRs)

	if rttToReport != 0 {
		if d.sequencer != nil {
			d.sequencer.setRTT(rttToReport)
		}

		if onRttUpdate := d.getOnRttUpdate(); onRttUpdate != nil {
			onRttUpdate(d, rttToReport)
		}
	}
}

func (d *DownTrack) SetConnected() {
	if !d.connected.Swap(true) {
		d.onBindAndConnected()
	}
}

// SetActivePaddingOnMuteUpTrack will enable padding on the track when its uptrack is muted.
// Pion will not fire OnTrack event until it receives packet for the track,
// so we send padding packets to help pion client (go-sdk) to fire the event.
func (d *DownTrack) SetActivePaddingOnMuteUpTrack() {
	d.activePaddingOnMuteUpTrack.Store(true)
}

func (d *DownTrack) retransmitPackets(nacks []uint16) {
	if d.sequencer == nil {
		return
	}

	if FlagStopRTXOnPLI && d.isNACKThrottled.Load() {
		return
	}

	filtered, disallowedLayers := d.forwarder.FilterRTX(nacks)
	if len(filtered) == 0 {
		return
	}

	src := PacketFactory.Get().(*[]byte)
	defer PacketFactory.Put(src)

	nackAcks := uint32(0)
	nackMisses := uint32(0)
	numRepeatedNACKs := uint32(0)
	nackInfos := make([]NackInfo, 0, len(filtered))
	for _, meta := range d.sequencer.getPacketsMeta(filtered) {
		if disallowedLayers[meta.layer] {
			continue
		}

		nackAcks++
		nackInfos = append(nackInfos, NackInfo{
			SequenceNumber: meta.targetSeqNo,
			Timestamp:      meta.timestamp,
			Attempts:       meta.nacked,
		})

		pktBuff := *src
		n, err := d.receiver.ReadRTP(pktBuff, uint8(meta.layer), meta.sourceSeqNo)
		if err != nil {
			if err == io.EOF {
				break
			}
			nackMisses++
			continue
		}

		if meta.nacked > 1 {
			numRepeatedNACKs++
		}

		var pkt rtp.Packet
		if err = pkt.Unmarshal(pktBuff[:n]); err != nil {
			continue
		}
		pkt.Header.SequenceNumber = meta.targetSeqNo
		pkt.Header.Timestamp = meta.timestamp
		pkt.Header.SSRC = d.ssrc
		pkt.Header.PayloadType = d.payloadType

		var payload []byte
		pool := PacketFactory.Get().(*[]byte)
		if d.mime == "video/vp8" && len(pkt.Payload) > 0 {
			var incomingVP8 buffer.VP8
			if err = incomingVP8.Unmarshal(pkt.Payload); err != nil {
				d.logger.Errorw("unmarshalling VP8 packet err", err)
				PacketFactory.Put(pool)
				continue
			}

			if len(meta.codecBytes) != 0 {
				payload = d.translateVP8PacketTo(&pkt, &incomingVP8, meta.codecBytes, pool)
			}
		}
		if payload == nil {
			payload = (*pool)[:len(pkt.Payload)]
			copy(payload, pkt.Payload)
		}

		d.pacer.Enqueue(pacer.Packet{
			Header:             &pkt.Header,
			Extensions:         []pacer.ExtensionData{{ID: uint8(d.dependencyDescriptorExtID), Payload: meta.ddBytes}},
			Payload:            payload,
			AbsSendTimeExtID:   uint8(d.absSendTimeExtID),
			TransportWideExtID: uint8(d.transportWideExtID),
			WriteStream:        d.writeStream,
			Metadata: sendPacketMetadata{
				isRTX: true,
				pool:  pool,
			},
			OnSent: d.packetSent,
		})
	}

	d.totalRepeatedNACKs.Add(numRepeatedNACKs)

	d.rtpStats.UpdateNackProcessed(nackAcks, nackMisses, numRepeatedNACKs)
	// STREAM-ALLOCATOR-EXPERIMENTAL-TODO-START
	// Need to check on the following
	//   - get all NACKs from sequencer even if SFU is not acknowledging,
	//     i. e. SFU does not acknowledge even same sequence number is NACKed too closely,
	//     but if sequencer return those also (even if not actually retransmitting),
	//     will that provide a signal?
	//   - get padding NACKs also? Maybe only look at them when their NACK count is 2?
	//     because padding runs in a separate path, it could get out of order with
	//     primary packets. So, it could be NACKed once. But, a repeat NACK means they
	//     were probably lost. But, as we do not retransmit padding packets, more than
	//     the second try does not provide any useful signal.
	// STREAM-ALLOCATOR-EXPERIMENTAL-TODO-END
	if sal := d.getStreamAllocatorListener(); sal != nil && len(nackInfos) != 0 {
		sal.OnNACK(d, nackInfos)
	}
}

func (d *DownTrack) getTranslatedRTPHeader(extPkt *buffer.ExtPacket, tp *TranslationParams) (*rtp.Header, error) {
	tpRTP := tp.rtp
	hdr := extPkt.Packet.Header
	hdr.PayloadType = d.payloadType
	hdr.Timestamp = tpRTP.timestamp
	hdr.SequenceNumber = tpRTP.sequenceNumber
	hdr.SSRC = d.ssrc
	if tp.marker {
		hdr.Marker = tp.marker
	}

	return &hdr, nil
}

func (d *DownTrack) translateVP8PacketTo(pkt *rtp.Packet, incomingVP8 *buffer.VP8, translatedVP8 []byte, outbuf *[]byte) []byte {
	buf := (*outbuf)[:len(pkt.Payload)+len(translatedVP8)-incomingVP8.HeaderSize]
	srcPayload := pkt.Payload[incomingVP8.HeaderSize:]
	dstPayload := buf[len(translatedVP8):]
	copy(dstPayload, srcPayload)

	copy(buf[:len(translatedVP8)], translatedVP8)
	return buf
}

func (d *DownTrack) DebugInfo() map[string]interface{} {
	rtpMungerParams := d.forwarder.GetRTPMungerParams()
	stats := map[string]interface{}{
		"HighestIncomingSN": rtpMungerParams.highestIncomingSN,
		"LastSN":            rtpMungerParams.lastSN,
		"SNOffset":          rtpMungerParams.snOffset,
		"LastTS":            rtpMungerParams.lastTS,
		"TSOffset":          rtpMungerParams.tsOffset,
		"LastMarker":        rtpMungerParams.lastMarker,
		"LastPli":           d.rtpStats.LastPli(),
	}

	senderReport := d.CreateSenderReport()
	if senderReport != nil {
		stats["NTPTime"] = senderReport.NTPTime
		stats["RTPTime"] = senderReport.RTPTime
		stats["PacketCount"] = senderReport.PacketCount
	}

	return map[string]interface{}{
		"SubscriberID":        d.subscriberID,
		"TrackID":             d.id,
		"StreamID":            d.streamID,
		"SSRC":                d.ssrc,
		"MimeType":            d.codec.MimeType,
		"Bound":               d.bound.Load(),
		"Muted":               d.forwarder.IsMuted(),
		"PubMuted":            d.forwarder.IsPubMuted(),
		"CurrentSpatialLayer": d.forwarder.CurrentLayer().Spatial,
		"Stats":               stats,
	}
}

func (d *DownTrack) getExpectedRTPTimestamp(at time.Time) (uint64, error) {
	return d.rtpStats.GetExpectedRTPTimestamp(at)
}

func (d *DownTrack) GetConnectionScoreAndQuality() (float32, livekit.ConnectionQuality) {
	return d.connectionStats.GetScoreAndQuality()
}

func (d *DownTrack) GetTrackStats() *livekit.RTPStats {
	return d.rtpStats.ToProto()
}

func (d *DownTrack) deltaStats(ds *buffer.RTPDeltaInfo) map[uint32]*buffer.StreamStatsWithLayers {
	if ds == nil {
		return nil
	}

	streamStats := make(map[uint32]*buffer.StreamStatsWithLayers, 1)
	streamStats[d.ssrc] = &buffer.StreamStatsWithLayers{
		RTPStats: ds,
		Layers: map[int32]*buffer.RTPDeltaInfo{
			0: ds,
		},
	}

	return streamStats
}

func (d *DownTrack) getDeltaStats() map[uint32]*buffer.StreamStatsWithLayers {
	return d.deltaStats(d.rtpStats.DeltaInfo(d.deltaStatsSnapshotId))
}

func (d *DownTrack) getDeltaStatsOverridden() map[uint32]*buffer.StreamStatsWithLayers {
	return d.deltaStats(d.rtpStats.DeltaInfoOverridden(d.deltaStatsOverriddenSnapshotId))
}

func (d *DownTrack) GetNackStats() (totalPackets uint32, totalRepeatedNACKs uint32) {
	totalPackets = d.rtpStats.GetTotalPacketsPrimary()
	totalRepeatedNACKs = d.totalRepeatedNACKs.Load()
	return
}

func (d *DownTrack) GetAndResetBytesSent() (uint32, uint32) {
	return d.bytesSent.Swap(0), d.bytesRetransmitted.Swap(0)
}

func (d *DownTrack) onBindAndConnected() {
	if d.connected.Load() && d.bound.Load() && !d.bindAndConnectedOnce.Swap(true) {
		if d.kind == webrtc.RTPCodecTypeVideo {
			_, layer := d.forwarder.CheckSync()
			if layer != buffer.InvalidLayerSpatial {
				d.receiver.SendPLI(layer, true)
			}
		}

		if d.activePaddingOnMuteUpTrack.Load() {
			go d.sendPaddingOnMute()
		}
	}
}

func (d *DownTrack) sendPaddingOnMute() {
	// let uptrack have chance to send packet before we send padding
	time.Sleep(waitBeforeSendPaddingOnMute)

	d.logger.Debugw("sending padding on mute")
	if d.kind == webrtc.RTPCodecTypeVideo {
		d.sendPaddingOnMuteForVideo()
	} else if d.mime == "audio/opus" {
		d.sendSilentFrameOnMuteForOpus()
	}
}

func (d *DownTrack) sendPaddingOnMuteForVideo() {
	paddingOnMuteInterval := 100 * time.Millisecond
	numPackets := maxPaddingOnMuteDuration / paddingOnMuteInterval
	for i := 0; i < int(numPackets); i++ {
		if d.rtpStats.IsActive() || d.IsClosed() {
			return
		}
		d.WritePaddingRTP(20, true, true)
		time.Sleep(paddingOnMuteInterval)
	}
}

func (d *DownTrack) sendSilentFrameOnMuteForOpus() {
	frameRate := uint32(50)
	frameDuration := time.Duration(1000/frameRate) * time.Millisecond
	numFrames := frameRate * uint32(maxPaddingOnMuteDuration/time.Second)
	for {
		if d.rtpStats.IsActive() || d.IsClosed() || numFrames <= 0 {
			return
		}
		snts, _, err := d.forwarder.GetSnTsForBlankFrames(frameRate, 1)
		if err != nil {
			d.logger.Warnw("could not get SN/TS for blank frame", err)
			return
		}
		for i := 0; i < len(snts); i++ {
			hdr := rtp.Header{
				Version:        2,
				Padding:        false,
				Marker:         true,
				PayloadType:    d.payloadType,
				SequenceNumber: snts[i].sequenceNumber,
				Timestamp:      snts[i].timestamp,
				SSRC:           d.ssrc,
				CSRC:           []uint32{},
			}

			payload, err := d.getOpusBlankFrame(false)
			if err != nil {
				d.logger.Warnw("could not get blank frame", err)
				return
			}

			d.pacer.Enqueue(pacer.Packet{
				Header:             &hdr,
				Payload:            payload,
				AbsSendTimeExtID:   uint8(d.absSendTimeExtID),
				TransportWideExtID: uint8(d.transportWideExtID),
				WriteStream:        d.writeStream,
				Metadata: sendPacketMetadata{
					isBlankFrame:    true,
					disableRTPStats: true,
				},
				OnSent: d.packetSent,
			})
		}

		numFrames--
		time.Sleep(frameDuration)
	}
}

func (d *DownTrack) HandleRTCPSenderReportData(_payloadType webrtc.PayloadType, _layer int32, _srData *buffer.RTCPSenderReportData) error {
	return nil
}

type sendPacketMetadata struct {
	layer           int32
	arrival         time.Time
	isKeyFrame      bool
	isRTX           bool
	isPadding       bool
	isBlankFrame    bool
	disableCounter  bool
	disableRTPStats bool
	tp              *TranslationParams
	pool            *[]byte
}

func (d *DownTrack) packetSent(md interface{}, hdr *rtp.Header, payloadSize int, sendTime time.Time, sendError error) {
	spmd, ok := md.(sendPacketMetadata)
	if !ok {
		d.logger.Errorw("invalid send packet metadata", nil)
		return
	}

	if spmd.pool != nil {
		PacketFactory.Put(spmd.pool)
	}

	if sendError != nil {
		return
	}

	headerSize := hdr.MarshalSize()
	if !spmd.disableCounter {
		// STREAM-ALLOCATOR-TODO: remove this stream allocator bytes counter once stream allocator changes fully to pull bytes counter
		size := uint32(headerSize + payloadSize)
		d.streamAllocatorBytesCounter.Add(size)
		if spmd.isRTX {
			d.bytesRetransmitted.Add(size)
		} else {
			d.bytesSent.Add(size)
		}
	}

	if !spmd.disableRTPStats {
		packetTime := spmd.arrival
		if packetTime.IsZero() {
			packetTime = sendTime
		}
		if spmd.isPadding {
			d.rtpStats.Update(hdr, 0, payloadSize, packetTime)
		} else {
			d.rtpStats.Update(hdr, payloadSize, 0, packetTime)
		}
	}

	if spmd.isKeyFrame {
		d.isNACKThrottled.Store(false)
		d.rtpStats.UpdateKeyFrame(1)
		d.logger.Debugw(
			"forwarding key frame",
			"layer", spmd.layer,
			"rtpsn", hdr.SequenceNumber,
			"rtpts", hdr.Timestamp,
		)
	}

	if spmd.tp != nil {
		if spmd.tp.isSwitching {
			d.postMaxLayerNotifierEvent()
		}

		if spmd.tp.isResuming {
			if sal := d.getStreamAllocatorListener(); sal != nil {
				sal.OnResume(d)
			}
		}
	}
}

// -------------------------------------------------------------------------------

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
	return fmt.Sprintf("DownTrackState{rtpStats: %s, delta: %d, forwarder: %s}",
		d.RTPStats.ToString(), d.DeltaStatsSnapshotId, d.ForwarderState.String())
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

	upstreamCodecs         []webrtc.RTPCodecParameters
	codec                  webrtc.RTPCodecCapability
	rtpHeaderExtensions    []webrtc.RTPHeaderExtensionParameter
	absSendTimeID          int
	dependencyDescriptorID int
	receiver               TrackReceiver
	transceiver            *webrtc.RTPTransceiver
	writeStream            webrtc.TrackLocalWriter
	rtcpReader             *buffer.RTCPReader
	onCloseHandler         func(willBeResumed bool)
	onBinding              func()

	listenerLock            sync.RWMutex
	receiverReportListeners []ReceiverReportListener

	bindLock sync.Mutex
	bound    atomic.Bool

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

	// for throttling error logs
	writeIOErrors atomic.Uint32

	isNACKThrottled atomic.Bool

	activePaddingOnMuteUpTrack atomic.Bool

	streamAllocatorLock             sync.RWMutex
	streamAllocatorListener         DownTrackStreamAllocatorListener
	streamAllocatorReportGeneration int
	streamAllocatorBytesCounter     atomic.Uint32
	bytesSent                       atomic.Uint32
	bytesRetransmitted              atomic.Uint32

	// update stats
	onStatsUpdate func(dt *DownTrack, stat *livekit.AnalyticsStat)

	// when max subscribed layer changes
	onMaxSubscribedLayerChanged func(dt *DownTrack, layer int32)

	// update rtt
	onRttUpdate func(dt *DownTrack, rtt uint32)
}

// NewDownTrack returns a DownTrack.
func NewDownTrack(
	codecs []webrtc.RTPCodecParameters,
	r TrackReceiver,
	bf *buffer.Factory,
	subID livekit.ParticipantID,
	mt int,
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
		logger:         logger,
		id:             r.TrackID(),
		subscriberID:   subID,
		maxTrack:       mt,
		streamID:       r.StreamID(),
		bufferFactory:  bf,
		receiver:       r,
		upstreamCodecs: codecs,
		kind:           kind,
		codec:          codecs[0].RTPCodecCapability,
	}
	d.forwarder = NewForwarder(d.kind, d.logger, d.receiver.GetReferenceLayerRTPTimestamp)
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
		IsDependentJitter:         true,
		GetDeltaStats:             d.getDeltaStats,
		GetDeltaStatsOverridden:   d.getDeltaStatsOverridden,
		GetLastReceiverReportTime: func() time.Time { return d.rtpStats.LastReceiverReport() },
		Logger:                    d.logger.WithValues("direction", "down"),
	})
	d.connectionStats.OnStatsUpdate(func(_cs *connectionquality.ConnectionStats, stat *livekit.AnalyticsStat) {
		if d.onStatsUpdate != nil {
			d.onStatsUpdate(d, stat)
		}
	})

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
		d.bindLock.Unlock()
		return webrtc.RTPCodecParameters{}, webrtc.ErrUnsupportedCodec
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
		d.onBinding()
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
	d.connectionStats.Start(ti, time.Now())
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
	d.rtpHeaderExtensions = rtpHeaderExtensions
	for _, ext := range rtpHeaderExtensions {
		switch ext.URI {
		case sdp.ABSSendTimeURI:
			d.absSendTimeID = ext.ID
		case dd.ExtensionUrl:
			d.dependencyDescriptorID = ext.ID
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

// WriteRTP writes an RTP Packet to the DownTrack
func (d *DownTrack) WriteRTP(extPkt *buffer.ExtPacket, layer int32) error {
	var pool *[]byte
	defer func() {
		if pool != nil {
			PacketFactory.Put(pool)
			pool = nil
		}
	}()

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

	payload := extPkt.Packet.Payload
	if len(tp.codecBytes) != 0 {
		incomingVP8, _ := extPkt.Payload.(buffer.VP8)
		pool = PacketFactory.Get().(*[]byte)
		payload = d.translateVP8PacketTo(extPkt.Packet, &incomingVP8, tp.codecBytes, pool)
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
		return err
	}

	_, err = d.writeStream.WriteRTP(hdr, payload)
	if err != nil {
		if errors.Is(err, io.ErrClosedPipe) {
			writeIOErrors := d.writeIOErrors.Inc()
			if (writeIOErrors % 100) == 1 {
				d.logger.Errorw("write rtp packet failed", err, "count", writeIOErrors)
			}
		} else {
			d.logger.Errorw("write rtp packet failed", err)
		}
		return err
	}

	// STREAM-ALLOCATOR-TODO: remove this stream allocator bytes counter once stream allocator changes fully to pull bytes counter
	d.streamAllocatorBytesCounter.Add(uint32(hdr.MarshalSize() + len(payload)))
	d.bytesSent.Add(uint32(hdr.MarshalSize() + len(payload)))

	if tp.isSwitchingToMaxSpatial && d.onMaxSubscribedLayerChanged != nil && d.kind == webrtc.RTPCodecTypeVideo {
		d.onMaxSubscribedLayerChanged(d, layer)
	}

	if extPkt.KeyFrame {
		d.isNACKThrottled.Store(false)
		d.rtpStats.UpdateKeyFrame(1)
		d.logger.Debugw("forwarding key frame", "layer", layer, "rtpsn", hdr.SequenceNumber, "rtpts", hdr.Timestamp)
	}

	if tp.isSwitchingToRequestSpatial {
		locked, _ := d.forwarder.CheckSync()
		if locked {
			d.stopKeyFrameRequester()
		}
	}

	if tp.isResuming {
		if sal := d.getStreamAllocatorListener(); sal != nil {
			sal.OnResume(d)
		}
	}

	d.rtpStats.Update(hdr, len(payload), 0, time.Now().UnixNano())
	return nil
}

// WritePaddingRTP tries to write as many padding only RTP packets as necessary
// to satisfy given size to the DownTrack
func (d *DownTrack) WritePaddingRTP(bytesToSend int, paddingOnMute bool) int {
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

	snts, err := d.forwarder.GetSnTsForPadding(num)
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

		err = d.writeRTPHeaderExtensions(&hdr)
		if err != nil {
			return bytesSent
		}

		payload := make([]byte, RTPPaddingMaxPayloadSize)
		// last byte of padding has padding size including that byte
		payload[RTPPaddingMaxPayloadSize-1] = byte(RTPPaddingMaxPayloadSize)

		_, err = d.writeStream.WriteRTP(&hdr, payload)
		if err != nil {
			return bytesSent
		}

		if !paddingOnMute {
			d.rtpStats.Update(&hdr, 0, len(payload), time.Now().UnixNano())
		}

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

	return bytesSent
}

// Mute enables or disables media forwarding - subscriber triggered
func (d *DownTrack) Mute(muted bool) {
	changed, maxLayer := d.forwarder.Mute(muted)
	d.handleMute(muted, false, changed, maxLayer)
}

// PubMute enables or disables media forwarding - publisher side
func (d *DownTrack) PubMute(pubMuted bool) {
	changed, maxLayer := d.forwarder.PubMute(pubMuted)
	d.handleMute(pubMuted, true, changed, maxLayer)
}

func (d *DownTrack) handleMute(muted bool, isPub bool, changed bool, maxLayer buffer.VideoLayer) {
	if !changed {
		return
	}

	d.connectionStats.UpdateMute(d.forwarder.IsAnyMuted(), time.Now())

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
	if !isPub && d.onMaxSubscribedLayerChanged != nil && d.kind == webrtc.RTPCodecTypeVideo {
		notifyLayer := buffer.InvalidLayerSpatial
		if !muted {
			//
			// When unmuting, don't wait for layer lock as
			// client might need to be notified to start layers
			// before locking can happen in the forwarder.
			//
			notifyLayer = maxLayer.Spatial
		}
		d.onMaxSubscribedLayerChanged(d, notifyLayer)
	}

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

// Close track, flush used to indicate whether send blank frame to flush
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
		d.receiver.DeleteDownTrack(d.subscriberID)

		if d.rtcpReader != nil && flush {
			d.logger.Debugw("downtrack close rtcp reader")
			d.rtcpReader.Close()
			d.rtcpReader.OnPacket(nil)
		}
	}

	d.bindLock.Unlock()
	d.connectionStats.Close()
	d.rtpStats.Stop()
	d.logger.Infow("rtp stats", "direction", "downstream", "mime", d.mime, "ssrc", d.ssrc, "stats", d.rtpStats.ToString())

	if d.onMaxSubscribedLayerChanged != nil && d.kind == webrtc.RTPCodecTypeVideo {
		d.onMaxSubscribedLayerChanged(d, buffer.InvalidLayerSpatial)
	}

	if d.onCloseHandler != nil {
		d.onCloseHandler(!flush)
	}

	d.stopKeyFrameRequester()
	d.ClearStreamAllocatorReportInterval()
}

func (d *DownTrack) SetMaxSpatialLayer(spatialLayer int32) {
	changed, maxLayer, currentLayer := d.forwarder.SetMaxSpatialLayer(spatialLayer)
	if !changed {
		return
	}

	if d.onMaxSubscribedLayerChanged != nil && d.kind == webrtc.RTPCodecTypeVideo && maxLayer.SpatialGreaterThanOrEqual(currentLayer) {
		//
		// Notify when new max is
		//   1. Equal to current -> already locked to the new max
		//   2. Greater than current -> two scenarios
		//      a. is higher than previous max -> client may need to start higher layer before forwarder can lock
		//      b. is lower than previous max -> client can stop higher layer(s)
		//
		d.onMaxSubscribedLayerChanged(d, maxLayer.Spatial)
	}

	if sal := d.getStreamAllocatorListener(); sal != nil {
		sal.OnSubscribedLayerChanged(d, maxLayer)
	}
}

func (d *DownTrack) SetMaxTemporalLayer(temporalLayer int32) {
	changed, maxLayer, _ := d.forwarder.SetMaxTemporalLayer(temporalLayer)
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

func (d *DownTrack) maybeAddTransition(_bitrate int64, distance float64) {
	if d.kind == webrtc.RTPCodecTypeAudio {
		return
	}

	d.connectionStats.AddLayerTransition(distance, time.Now())
}

func (d *DownTrack) UpTrackBitrateReport(availableLayers []int32, bitrates Bitrates) {
	d.maybeAddTransition(
		d.forwarder.GetOptimalBandwidthNeeded(bitrates),
		d.forwarder.DistanceToDesired(availableLayers, bitrates),
	)
}

// OnCloseHandler method to be called on remote tracked removed
func (d *DownTrack) OnCloseHandler(fn func(willBeResumed bool)) {
	d.onCloseHandler = fn
}

func (d *DownTrack) OnBinding(fn func()) {
	d.onBinding = fn
}

func (d *DownTrack) AddReceiverReportListener(listener ReceiverReportListener) {
	d.listenerLock.Lock()
	defer d.listenerLock.Unlock()

	d.receiverReportListeners = append(d.receiverReportListeners, listener)
}

func (d *DownTrack) OnStatsUpdate(fn func(dt *DownTrack, stat *livekit.AnalyticsStat)) {
	d.onStatsUpdate = fn
}

func (d *DownTrack) OnRttUpdate(fn func(dt *DownTrack, rtt uint32)) {
	d.onRttUpdate = fn
}

func (d *DownTrack) OnMaxLayerChanged(fn func(dt *DownTrack, layer int32)) {
	d.onMaxSubscribedLayerChanged = fn
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
	d.maybeAddTransition(allocation.BandwidthNeeded, allocation.DistanceToDesired)
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
	d.maybeAddTransition(allocation.BandwidthNeeded, allocation.DistanceToDesired)
	return allocation
}

func (d *DownTrack) AllocateNextHigher(availableChannelCapacity int64, allowOvershoot bool) (VideoAllocation, bool) {
	al, brs := d.receiver.GetLayeredBitrate()
	allocation, available := d.forwarder.AllocateNextHigher(availableChannelCapacity, al, brs, allowOvershoot)
	d.maybeStartKeyFrameRequester()
	d.maybeAddTransition(allocation.BandwidthNeeded, allocation.DistanceToDesired)
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
	d.maybeAddTransition(allocation.BandwidthNeeded, allocation.DistanceToDesired)
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

	return d.rtpStats.GetRtcpSenderReport(d.ssrc, d.receiver.GetRTCPSenderReportDataExt(d.forwarder.GetReferenceLayerSpatial()))
}

func (d *DownTrack) writeBlankFrameRTP(duration float32, generation uint32) chan struct{} {
	done := make(chan struct{})
	go func() {
		// don't send if nothing has been sent
		if !d.rtpStats.IsActive() {
			close(done)
			return
		}

		var writeBlankFrame func(*rtp.Header, bool) (int, error)
		switch d.mime {
		case "audio/opus":
			writeBlankFrame = d.writeOpusBlankFrame
		case "audio/red":
			writeBlankFrame = d.writeOpusRedBlankFrame
		case "video/vp8":
			writeBlankFrame = d.writeVP8BlankFrame
		case "video/h264":
			writeBlankFrame = d.writeH264BlankFrame
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

				err = d.writeRTPHeaderExtensions(&hdr)
				if err != nil {
					d.logger.Warnw("could not write header extension for blank frame", err)
					close(done)
					return
				}

				pktSize, err := writeBlankFrame(&hdr, frameEndNeeded)
				if err != nil {
					d.logger.Warnw("could not write blank frame", err)
					close(done)
					return
				}

				d.streamAllocatorBytesCounter.Add(uint32(pktSize))
				d.bytesSent.Add(uint32(pktSize))

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

func (d *DownTrack) writeOpusBlankFrame(hdr *rtp.Header, frameEndNeeded bool) (int, error) {
	// silence frame
	// Used shortly after muting to ensure residual noise does not keep
	// generating noise at the decoder after the stream is stopped
	// i. e. comfort noise generation actually not producing something comfortable.
	payload := make([]byte, len(OpusSilenceFrame))
	copy(payload[0:], OpusSilenceFrame)

	_, err := d.writeStream.WriteRTP(hdr, payload)
	if err == nil {
		d.rtpStats.Update(hdr, len(payload), 0, time.Now().UnixNano())
	}
	return hdr.MarshalSize() + len(payload), err
}

func (d *DownTrack) writeOpusRedBlankFrame(hdr *rtp.Header, frameEndNeeded bool) (int, error) {
	// primary only silence frame for opus/red, there is no need to contain redundant silent frames
	payload := make([]byte, len(OpusSilenceFrame)+1)

	// primary header
	//  0 1 2 3 4 5 6 7
	// +-+-+-+-+-+-+-+-+
	// |0|   Block PT  |
	// +-+-+-+-+-+-+-+-+
	payload[0] = opusPT
	copy(payload[1:], OpusSilenceFrame)

	_, err := d.writeStream.WriteRTP(hdr, payload)
	if err == nil {
		d.rtpStats.Update(hdr, len(payload), 0, time.Now().UnixNano())
	}
	return hdr.MarshalSize() + len(payload), err
}

func (d *DownTrack) writeVP8BlankFrame(hdr *rtp.Header, frameEndNeeded bool) (int, error) {
	blankVP8, err := d.forwarder.GetPadding(frameEndNeeded)
	if err != nil {
		return 0, err
	}

	// 8x8 key frame
	// Used even when closing out a previous frame. Looks like receivers
	// do not care about content (it will probably end up being an undecodable
	// frame, but that should be okay as there are key frames following)
	payload := make([]byte, len(blankVP8)+len(VP8KeyFrame8x8))
	copy(payload[:len(blankVP8)], blankVP8)
	copy(payload[len(blankVP8):], VP8KeyFrame8x8)

	_, err = d.writeStream.WriteRTP(hdr, payload)
	if err == nil {
		d.rtpStats.Update(hdr, len(payload), 0, time.Now().UnixNano())
	}
	return hdr.MarshalSize() + len(payload), err
}

func (d *DownTrack) writeH264BlankFrame(hdr *rtp.Header, frameEndNeeded bool) (int, error) {
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
	_, err := d.writeStream.WriteRTP(hdr, payload)
	if err == nil {
		d.rtpStats.Update(hdr, len(payload), 0, time.Now().UnixNano())
	}
	return hdr.MarshalSize() + offset, err
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

		if d.onRttUpdate != nil {
			d.onRttUpdate(d, rttToReport)
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

	var pool *[]byte
	defer func() {
		if pool != nil {
			PacketFactory.Put(pool)
			pool = nil
		}
	}()

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

		if pool != nil {
			PacketFactory.Put(pool)
			pool = nil
		}

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

		payload := pkt.Payload
		if d.mime == "video/vp8" && len(pkt.Payload) > 0 {
			var incomingVP8 buffer.VP8
			if err = incomingVP8.Unmarshal(pkt.Payload); err != nil {
				d.logger.Errorw("unmarshalling VP8 packet err", err)
				continue
			}

			if len(meta.codecBytes) != 0 {
				pool = PacketFactory.Get().(*[]byte)
				payload = d.translateVP8PacketTo(&pkt, &incomingVP8, meta.codecBytes, pool)
			}
		}

		var extraExtensions []extensionData
		if d.dependencyDescriptorID != 0 && len(meta.ddBytes) != 0 {
			extraExtensions = append(extraExtensions, extensionData{
				id:      uint8(d.dependencyDescriptorID),
				payload: meta.ddBytes,
			})
		}
		err = d.writeRTPHeaderExtensions(&pkt.Header, extraExtensions...)
		if err != nil {
			d.logger.Errorw("writing rtp header extensions err", err)
			continue
		}

		if _, err = d.writeStream.WriteRTP(&pkt.Header, payload); err != nil {
			d.logger.Errorw("writing rtx packet err", err)
		} else {
			d.streamAllocatorBytesCounter.Add(uint32(pkt.Header.MarshalSize() + len(payload)))
			d.bytesRetransmitted.Add(uint32(pkt.Header.MarshalSize() + len(payload)))

			d.rtpStats.Update(&pkt.Header, len(payload), 0, time.Now().UnixNano())
		}
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

type extensionData struct {
	id      uint8
	payload []byte
}

// writes RTP header extensions of track
func (d *DownTrack) writeRTPHeaderExtensions(hdr *rtp.Header, extraExtensions ...extensionData) error {
	// clear out extensions that may have been in the forwarded header
	hdr.Extension = false
	hdr.ExtensionProfile = 0
	hdr.Extensions = []rtp.Extension{}

	for _, ext := range extraExtensions {
		hdr.SetExtension(ext.id, ext.payload)
	}

	if d.absSendTimeID != 0 {
		sendTime := rtp.NewAbsSendTimeExtension(time.Now())
		b, err := sendTime.Marshal()
		if err != nil {
			return err
		}

		err = hdr.SetExtension(uint8(d.absSendTimeID), b)
		if err != nil {
			return err
		}
	}

	return nil
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

	var extension []extensionData
	if d.dependencyDescriptorID != 0 && len(tp.ddBytes) != 0 {
		extension = append(extension, extensionData{
			id:      uint8(d.dependencyDescriptorID),
			payload: tp.ddBytes,
		})
	}
	err := d.writeRTPHeaderExtensions(&hdr, extension...)
	if err != nil {
		return nil, err
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
	d.logger.Debugw("sending padding on mute")
	// let uptrack have chance to send packet before we send padding
	time.Sleep(waitBeforeSendPaddingOnMute)

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
		d.WritePaddingRTP(20, true)
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

			err = d.writeRTPHeaderExtensions(&hdr)
			if err != nil {
				d.logger.Warnw("could not write header extension for blank frame", err)
				return
			}

			payload := make([]byte, len(OpusSilenceFrame))
			copy(payload[0:], OpusSilenceFrame)

			_, err := d.writeStream.WriteRTP(&hdr, payload)
			if err != nil {
				d.logger.Warnw("could not write blank frame", err)
				return
			}
		}

		numFrames--
		time.Sleep(frameDuration)
	}
}

func (d *DownTrack) HandleRTCPSenderReportData(_payloadType webrtc.PayloadType, _layer int32, _srData *buffer.RTCPSenderReportData) error {
	return nil
}

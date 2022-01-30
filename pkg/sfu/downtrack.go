package sfu

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/transport/packetio"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/connectionquality"
)

const (
	connectionQualityUpdateInterval = 5 * time.Second
)

// TrackSender defines an interface send media to remote peer
type TrackSender interface {
	UpTrackLayersChange(availableLayers []uint16)
	WriteRTP(p *buffer.ExtPacket, layer int32) error
	Close()
	// ID is the globally unique identifier for this Track.
	ID() string
	Codec() webrtc.RTPCodecCapability
	PeerID() livekit.ParticipantID
}

const (
	RTPPaddingMaxPayloadSize      = 255
	RTPPaddingEstimatedHeaderSize = 20
	RTPBlankFramesMax             = 6
)

var (
	ErrUnknownKind                       = errors.New("unknown kind of codec")
	ErrOutOfOrderSequenceNumberCacheMiss = errors.New("out-of-order sequence number not found in cache")
	ErrPaddingOnlyPacket                 = errors.New("padding only packet that need not be forwarded")
	ErrDuplicatePacket                   = errors.New("duplicate packet")
	ErrPaddingNotOnFrameBoundary         = errors.New("padding cannot send on non-frame boundary")
	ErrNotVP8                            = errors.New("not VP8")
	ErrOutOfOrderVP8PictureIdCacheMiss   = errors.New("out-of-order VP8 picture id not found in cache")
	ErrFilteredVP8TemporalLayer          = errors.New("filtered VP8 temporal layer")
)

var (
	VP8KeyFrame8x8 = []byte{0x10, 0x02, 0x00, 0x9d, 0x01, 0x2a, 0x08, 0x00, 0x08, 0x00, 0x00, 0x47, 0x08, 0x85, 0x85, 0x88, 0x85, 0x84, 0x88, 0x02, 0x02, 0x00, 0x0c, 0x0d, 0x60, 0x00, 0xfe, 0xff, 0xab, 0x50, 0x80}

	H264KeyFrame2x2SPS = []byte{0x67, 0x42, 0xc0, 0x1f, 0x0f, 0xd9, 0x1f, 0x88, 0x88, 0x84, 0x00, 0x00, 0x03, 0x00, 0x04, 0x00, 0x00, 0x03, 0x00, 0xc8, 0x3c, 0x60, 0xc9, 0x20}
	H264KeyFrame2x2PPS = []byte{0x68, 0x87, 0xcb, 0x83, 0xcb, 0x20}
	H264KeyFrame2x2IDR = []byte{0x65, 0x88, 0x84, 0x0a, 0xf2, 0x62, 0x80, 0x00, 0xa7, 0xbe}

	H264KeyFrame2x2 = [][]byte{H264KeyFrame2x2SPS, H264KeyFrame2x2PPS, H264KeyFrame2x2IDR}
)

type ReceiverReportListener func(dt *DownTrack, report *rtcp.ReceiverReport)

type PacketStats struct {
	octets  uint64
	packets uint32
}

// DownTrack  implements TrackLocal, is the track used to write packets
// to SFU Subscriber, the track handle the packets for simple, simulcast
// and SVC Publisher.
type DownTrack struct {
	logger        logger.Logger
	id            livekit.TrackID
	peerID        livekit.ParticipantID
	bound         atomicBool
	kind          webrtc.RTPCodecType
	mime          string
	ssrc          uint32
	streamID      string
	maxTrack      int
	payloadType   uint8
	sequencer     *sequencer
	bufferFactory *buffer.Factory

	forwarder *Forwarder

	codec                   webrtc.RTPCodecCapability
	rtpHeaderExtensions     []webrtc.RTPHeaderExtensionParameter
	receiver                TrackReceiver
	transceiver             *webrtc.RTPTransceiver
	writeStream             webrtc.TrackLocalWriter
	onCloseHandler          func()
	onBind                  func()
	receiverReportListeners []ReceiverReportListener
	listenerLock            sync.RWMutex
	closeOnce               sync.Once

	// Report helpers
	primaryStats atomic.Value // contains *PacketStats
	rtxStats     atomic.Value // contains *PacketStats
	paddingStats atomic.Value // contains *PacketStats

	connectionStats *connectionquality.ConnectionStats

	// Debug info
	lastPli     atomicInt64
	lastRTP     atomicInt64
	pktsDropped atomicUint32

	// RTCP callbacks
	onREMB                func(dt *DownTrack, remb *rtcp.ReceiverEstimatedMaximumBitrate)
	onTransportCCFeedback func(dt *DownTrack, cc *rtcp.TransportLayerCC)

	// simulcast layer availability change callback
	onAvailableLayersChanged func(dt *DownTrack)

	// subscription change callback
	onSubscriptionChanged func(dt *DownTrack)

	// max layer change callback
	onSubscribedLayersChanged func(dt *DownTrack, layers VideoLayers)

	// packet sent callback
	onPacketSent []func(dt *DownTrack, size int)

	// padding packet sent callback
	onPaddingSent []func(dt *DownTrack, size int)

	// update stats
	onStatsUpdate func(dt *DownTrack, stat *livekit.AnalyticsStat)

	// when max subscribed layer changes
	onMaxLayerChanged func(dt *DownTrack, layer int32)
}

// NewDownTrack returns a DownTrack.
func NewDownTrack(
	c webrtc.RTPCodecCapability,
	r TrackReceiver,
	bf *buffer.Factory,
	peerID livekit.ParticipantID,
	mt int,
	logger logger.Logger,
) (*DownTrack, error) {
	var kind webrtc.RTPCodecType
	switch {
	case strings.HasPrefix(c.MimeType, "audio/"):
		kind = webrtc.RTPCodecTypeAudio
	case strings.HasPrefix(c.MimeType, "video/"):
		kind = webrtc.RTPCodecTypeVideo
	default:
		kind = webrtc.RTPCodecType(0)
	}

	d := &DownTrack{
		logger:        logger,
		id:            r.TrackID(),
		peerID:        peerID,
		maxTrack:      mt,
		streamID:      r.StreamID(),
		bufferFactory: bf,
		receiver:      r,
		codec:         c,
		kind:          kind,
		forwarder:     NewForwarder(c, kind, logger),
	}

	d.connectionStats = connectionquality.NewConnectionStats(connectionquality.ConnectionStatsParams{
		UpdateInterval: connectionQualityUpdateInterval,
		CodecType:      kind,
		GetTotalBytes: func() uint64 {
			octets, _ := d.getSRStats()
			return octets
		},
		GetIsReducedQuality: func() bool {
			return d.GetForwardingStatus() != ForwardingStatusOptimal
		},
		Logger: d.logger,
	})
	d.connectionStats.OnStatsUpdate(func(_cs *connectionquality.ConnectionStats, stat *livekit.AnalyticsStat) {
		if d.onStatsUpdate != nil {
			d.onStatsUpdate(d, stat)
		}
	})
	d.connectionStats.Start()

	d.primaryStats.Store(new(PacketStats))
	d.rtxStats.Store(new(PacketStats))
	d.paddingStats.Store(new(PacketStats))

	return d, nil
}

// Bind is called by the PeerConnection after negotiation is complete
// This asserts that the code requested is supported by the remote peer.
// If so it sets up all the state (SSRC and PayloadType) to have a call
func (d *DownTrack) Bind(t webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	parameters := webrtc.RTPCodecParameters{RTPCodecCapability: d.codec}
	if codec, err := codecParametersFuzzySearch(parameters, t.CodecParameters()); err == nil {
		d.ssrc = uint32(t.SSRC())
		d.payloadType = uint8(codec.PayloadType)
		d.writeStream = t.WriteStream()
		d.mime = strings.ToLower(codec.MimeType)
		if rr := d.bufferFactory.GetOrNew(packetio.RTCPBufferPacket, uint32(t.SSRC())).(*buffer.RTCPReader); rr != nil {
			rr.OnPacket(func(pkt []byte) {
				d.handleRTCP(pkt)
			})
		}
		if strings.HasPrefix(d.codec.MimeType, "video/") {
			d.sequencer = newSequencer(d.maxTrack)
		}
		if d.onBind != nil {
			d.onBind()
		}
		d.bound.set(true)
		return codec, nil
	}
	return webrtc.RTPCodecParameters{}, webrtc.ErrUnsupportedCodec
}

// Unbind implements the teardown logic when the track is no longer needed. This happens
// because a track has been stopped.
func (d *DownTrack) Unbind(_ webrtc.TrackLocalContext) error {
	d.bound.set(false)
	d.receiver.DeleteDownTrack(d.peerID)
	return nil
}

// ID is the unique identifier for this Track. This should be unique for the
// stream, but doesn't have to globally unique. A common example would be 'audio' or 'video'
// and StreamID would be 'desktop' or 'webcam'
func (d *DownTrack) ID() string { return string(d.id) }

// Codec returns current track codec capability
func (d *DownTrack) Codec() webrtc.RTPCodecCapability { return d.codec }

// StreamID is the group this track belongs too. This must be unique
func (d *DownTrack) StreamID() string { return d.streamID }

func (d *DownTrack) PeerID() livekit.ParticipantID { return d.peerID }

// Sets RTP header extensions for this track
func (d *DownTrack) SetRTPHeaderExtensions(rtpHeaderExtensions []webrtc.RTPHeaderExtensionParameter) {
	d.rtpHeaderExtensions = rtpHeaderExtensions
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
	return fmt.Errorf("d.transceiver not exists")
}

func (d *DownTrack) SetTransceiver(transceiver *webrtc.RTPTransceiver) {
	d.transceiver = transceiver
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

	d.lastRTP.set(time.Now().UnixNano())

	if !d.bound.get() {
		return nil
	}

	tp, err := d.forwarder.GetTranslationParams(extPkt, layer)
	if tp.shouldSendPLI {
		d.lastPli.set(time.Now().UnixNano())
		d.receiver.SendPLI(layer)
	}
	if tp.shouldDrop {
		if tp.isDroppingRelevant {
			d.pktsDropped.add(1)
		}
		return err
	}

	payload := extPkt.Packet.Payload
	if tp.vp8 != nil {
		incomingVP8, _ := extPkt.Payload.(buffer.VP8)

		outbuf := &payload
		if incomingVP8.HeaderSize != tp.vp8.header.HeaderSize {
			pool = PacketFactory.Get().(*[]byte)
			outbuf = pool
		}
		payload, err = d.translateVP8PacketTo(&extPkt.Packet, &incomingVP8, tp.vp8.header, outbuf)
		if err != nil {
			d.pktsDropped.add(1)
			return err
		}
	}

	if d.sequencer != nil {
		meta := d.sequencer.push(extPkt.Packet.SequenceNumber, tp.rtp.sequenceNumber, tp.rtp.timestamp, uint8(layer), extPkt.Head)
		if meta != nil && tp.vp8 != nil {
			meta.packVP8(tp.vp8.header)
		}
	}

	hdr, err := d.getTranslatedRTPHeader(extPkt, tp.rtp)
	if err != nil {
		d.pktsDropped.add(1)
		return err
	}

	_, err = d.writeStream.WriteRTP(hdr, payload)
	if err == nil {
		pktSize := hdr.MarshalSize() + len(payload)
		for _, f := range d.onPacketSent {
			f(d, pktSize)
		}

		if tp.isSwitchingToMaxLayer && d.onMaxLayerChanged != nil && d.kind == webrtc.RTPCodecTypeVideo {
			d.onMaxLayerChanged(d, layer)
		}

		d.UpdatePrimaryStats(uint32(pktSize))
	} else {
		d.logger.Errorw("writing rtp packet err", err)
		d.pktsDropped.add(1)
	}

	return err
}

// WritePaddingRTP tries to write as many padding only RTP packets as necessary
// to satisfy given size to the DownTrack
func (d *DownTrack) WritePaddingRTP(bytesToSend int) int {
	primaryStats := d.primaryStats.Load().(*PacketStats)
	if primaryStats.packets == 0 {
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
	if d.forwarder.IsMuted() {
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

		size := hdr.MarshalSize() + len(payload)
		d.UpdatePaddingStats(uint32(size))
		for _, f := range d.onPaddingSent {
			f(d, size)
		}

		// LK-TODO-START
		// NACK buffer for these probe packets.
		// Probably okay to absorb the NACKs for these and ignore them.
		// Retransmission is probably a sign of network congestion/badness.
		// So, retransmitting padding packets is only going to make matters worse.
		// LK-TODO-END

		bytesSent += size
	}

	return bytesSent
}

// Mute enables or disables media forwarding
func (d *DownTrack) Mute(muted bool) {
	changed, maxLayers := d.forwarder.Mute(muted)
	if !changed {
		return
	}

	if d.onSubscriptionChanged != nil {
		d.onSubscriptionChanged(d)
	}

	if d.onMaxLayerChanged != nil && d.kind == webrtc.RTPCodecTypeVideo {
		if muted {
			d.onMaxLayerChanged(d, InvalidLayerSpatial)
		} else {
			//
			// When unmuting, don't wait for layer lock as
			// client might need to be notified to start layers
			// before locking can happen in the forwarder.
			//
			d.onMaxLayerChanged(d, maxLayers.spatial)
		}
	}
}

func (d *DownTrack) Close() {
	d.CloseWithFlush(true)
}

// Close track, flush used to indicate whether send blank frame to flush
// decoder of client.
// 1. When transceiver is reused by other participant's video track,
//    set flush=true to avoid previous video shows before previous stream is displayed.
// 2. in case of session migration, participant migrate from other node, video track should
//    be resumed with same participant, set flush=false since we don't need flush decoder.
func (d *DownTrack) CloseWithFlush(flush bool) {
	d.forwarder.Mute(true)

	// write blank frames after disabling so that other frames do not interfere.
	// Idea here is to send blank 1x1 key frames to flush the decoder buffer at the remote end.
	// Otherwise, with transceiver re-use last frame from previous stream is held in the
	// display buffer and there could be a brief moment where the previous stream is displayed.
	if flush {
		_ = d.writeBlankFrameRTP()
	}

	d.closeOnce.Do(func() {
		d.logger.Infow("closing sender", "peerID", d.peerID, "trackID", d.id, "kind", d.kind)
		d.receiver.DeleteDownTrack(d.peerID)

		d.connectionStats.Close()

		if d.onMaxLayerChanged != nil && d.kind == webrtc.RTPCodecTypeVideo {
			d.onMaxLayerChanged(d, InvalidLayerSpatial)
		}

		if d.onCloseHandler != nil {
			d.onCloseHandler()
		}
	})
}

func (d *DownTrack) SetMaxSpatialLayer(spatialLayer int32) {
	changed, maxLayers, prevMaxLayers, currentLayers := d.forwarder.SetMaxSpatialLayer(spatialLayer)
	if !changed {
		return
	}

	if d.onSubscribedLayersChanged != nil {
		d.onSubscribedLayersChanged(d, maxLayers)
	}

	if d.onMaxLayerChanged != nil && d.kind == webrtc.RTPCodecTypeVideo {
		if maxLayers.SpatialGreaterThan(prevMaxLayers) || maxLayers.SpatialEqual(currentLayers) {
			//
			// When max layer is increasing, don't wait for layer lock as
			// client might need to be notified to start layers
			// before locking can happen in the forwarder.
			//
			d.onMaxLayerChanged(d, maxLayers.spatial)
		}
	}
}

func (d *DownTrack) SetMaxTemporalLayer(temporalLayer int32) {
	changed, maxLayers, _, _ := d.forwarder.SetMaxTemporalLayer(temporalLayer)
	if !changed {
		return
	}

	if d.onSubscribedLayersChanged != nil {
		d.onSubscribedLayersChanged(d, maxLayers)
	}
}

func (d *DownTrack) MaxLayers() VideoLayers {
	return d.forwarder.MaxLayers()
}

func (d *DownTrack) GetForwardingStatus() ForwardingStatus {
	return d.forwarder.GetForwardingStatus()
}

func (d *DownTrack) UpTrackLayersChange(availableLayers []uint16) {
	d.forwarder.UpTrackLayersChange(availableLayers)

	if d.onAvailableLayersChanged != nil {
		d.onAvailableLayersChanged(d)
	}
}

// OnCloseHandler method to be called on remote tracked removed
func (d *DownTrack) OnCloseHandler(fn func()) {
	d.onCloseHandler = fn
}

func (d *DownTrack) OnBind(fn func()) {
	d.onBind = fn
}

func (d *DownTrack) OnREMB(fn func(dt *DownTrack, remb *rtcp.ReceiverEstimatedMaximumBitrate)) {
	d.onREMB = fn
}

func (d *DownTrack) OnTransportCCFeedback(fn func(dt *DownTrack, cc *rtcp.TransportLayerCC)) {
	d.onTransportCCFeedback = fn
}

func (d *DownTrack) AddReceiverReportListener(listener ReceiverReportListener) {
	d.listenerLock.Lock()
	defer d.listenerLock.Unlock()

	d.receiverReportListeners = append(d.receiverReportListeners, listener)
}

func (d *DownTrack) OnAvailableLayersChanged(fn func(dt *DownTrack)) {
	d.onAvailableLayersChanged = fn
}

func (d *DownTrack) OnSubscriptionChanged(fn func(dt *DownTrack)) {
	d.onSubscriptionChanged = fn
}

func (d *DownTrack) OnSubscribedLayersChanged(fn func(dt *DownTrack, layers VideoLayers)) {
	d.onSubscribedLayersChanged = fn
}

func (d *DownTrack) OnPacketSent(fn func(dt *DownTrack, size int)) {
	d.onPacketSent = append(d.onPacketSent, fn)
}

func (d *DownTrack) OnPaddingSent(fn func(dt *DownTrack, size int)) {
	d.onPaddingSent = append(d.onPaddingSent, fn)
}

func (d *DownTrack) OnStatsUpdate(fn func(dt *DownTrack, stat *livekit.AnalyticsStat)) {
	d.onStatsUpdate = fn
}

func (d *DownTrack) OnMaxLayerChanged(fn func(dt *DownTrack, layer int32)) {
	d.onMaxLayerChanged = fn

	// have to send this immediately to set initial values
	if fn != nil && d.kind == webrtc.RTPCodecTypeVideo {
		go fn(d, d.forwarder.MaxLayers().spatial)
	}
}

func (d *DownTrack) IsDeficient() bool {
	return d.forwarder.IsDeficient()
}

func (d *DownTrack) BandwidthRequested() int64 {
	return d.forwarder.BandwidthRequested()
}

func (d *DownTrack) DistanceToDesired() int32 {
	return d.forwarder.DistanceToDesired()
}

func (d *DownTrack) Allocate(availableChannelCapacity int64, allowPause bool) VideoAllocation {
	return d.forwarder.Allocate(availableChannelCapacity, allowPause, d.receiver.GetBitrateTemporalCumulative())
}

func (d *DownTrack) ProvisionalAllocatePrepare() {
	d.forwarder.ProvisionalAllocatePrepare(d.receiver.GetBitrateTemporalCumulative())
}

func (d *DownTrack) ProvisionalAllocate(availableChannelCapacity int64, layers VideoLayers, allowPause bool) int64 {
	return d.forwarder.ProvisionalAllocate(availableChannelCapacity, layers, allowPause)
}

func (d *DownTrack) ProvisionalAllocateGetCooperativeTransition() VideoTransition {
	return d.forwarder.ProvisionalAllocateGetCooperativeTransition()
}

func (d *DownTrack) ProvisionalAllocateGetBestWeightedTransition() VideoTransition {
	return d.forwarder.ProvisionalAllocateGetBestWeightedTransition()
}

func (d *DownTrack) ProvisionalAllocateCommit() VideoAllocation {
	return d.forwarder.ProvisionalAllocateCommit()
}

func (d *DownTrack) FinalizeAllocate() VideoAllocation {
	return d.forwarder.FinalizeAllocate(d.receiver.GetBitrateTemporalCumulative())
}

func (d *DownTrack) AllocateNextHigher() (VideoAllocation, bool) {
	return d.forwarder.AllocateNextHigher(d.receiver.GetBitrateTemporalCumulative())
}

func (d *DownTrack) Pause() VideoAllocation {
	return d.forwarder.Pause(d.receiver.GetBitrateTemporalCumulative())
}

func (d *DownTrack) Resync() {
	d.forwarder.Resync()
}

func (d *DownTrack) CreateSourceDescriptionChunks() []rtcp.SourceDescriptionChunk {
	if !d.bound.get() {
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
	if !d.bound.get() {
		return nil
	}

	currentLayers := d.forwarder.CurrentLayers()
	if currentLayers == InvalidLayers {
		return nil
	}

	srRTP, srNTP := d.receiver.GetSenderReportTime(currentLayers.spatial)
	if srRTP == 0 {
		return nil
	}

	now := time.Now()
	nowNTP := toNtpTime(now)

	diff := (uint64(now.Sub(ntpTime(srNTP).Time())) * uint64(d.codec.ClockRate)) / uint64(time.Second)
	octets, packets := d.getSRStats()

	return &rtcp.SenderReport{
		SSRC:        d.ssrc,
		NTPTime:     uint64(nowNTP),
		RTPTime:     srRTP + uint32(diff),
		PacketCount: packets,
		OctetCount:  uint32(octets),
	}
}

func (d *DownTrack) UpdatePrimaryStats(packetLen uint32) {
	primaryStats, _ := d.primaryStats.Load().(*PacketStats)

	primaryStats.octets += uint64(packetLen)
	primaryStats.packets += 1

	d.primaryStats.Store(primaryStats)
}

func (d *DownTrack) UpdateRtxStats(packetLen uint32) {
	rtxStats, _ := d.rtxStats.Load().(*PacketStats)

	rtxStats.octets += uint64(packetLen)
	rtxStats.packets += 1

	d.rtxStats.Store(rtxStats)
}

func (d *DownTrack) UpdatePaddingStats(packetLen uint32) {
	paddingStats, _ := d.paddingStats.Load().(*PacketStats)

	paddingStats.octets += uint64(packetLen)
	paddingStats.packets += 1

	d.paddingStats.Store(paddingStats)
}

func (d *DownTrack) writeBlankFrameRTP() error {
	// don't send if nothing has been sent
	primaryStats := d.primaryStats.Load().(*PacketStats)
	if primaryStats.packets == 0 {
		return nil
	}

	// LK-TODO: Support other video codecs
	if d.kind == webrtc.RTPCodecTypeAudio || (d.mime != "video/vp8" && d.mime != "video/h264") {
		return nil
	}

	snts, frameEndNeeded, err := d.forwarder.GetSnTsForBlankFrames()
	if err != nil {
		return err
	}

	// send a number of blank frames just in case there is loss.
	// Intentionally ignoring check for mute or bandwidth constrained mute
	// as this is used to clear client side buffer.
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
			return err
		}

		var pktSize int
		switch d.mime {
		case "video/vp8":
			pktSize, err = d.writeVP8BlankFrame(&hdr, frameEndNeeded)
		case "video/h264":
			pktSize, err = d.writeH264BlankFrame(&hdr, frameEndNeeded)
		default:
			return nil
		}
		if err != nil {
			return err
		}

		for _, f := range d.onPacketSent {
			f(d, pktSize)
		}

		d.UpdatePrimaryStats(uint32(pktSize))

		// only the first frame will need frameEndNeeded to close out the
		// previous picture, rest are small key frames
		frameEndNeeded = false
	}

	return nil
}

func (d *DownTrack) writeVP8BlankFrame(hdr *rtp.Header, frameEndNeeded bool) (int, error) {
	blankVP8 := d.forwarder.GetPaddingVP8(frameEndNeeded)

	// 1x1 key frame
	// Used even when closing out a previous frame. Looks like receivers
	// do not care about content (it will probably end up being an undecodable
	// frame, but that should be okay as there are key frames following)
	payload := make([]byte, blankVP8.HeaderSize+len(VP8KeyFrame8x8))
	vp8Header := payload[:blankVP8.HeaderSize]
	err := blankVP8.MarshalTo(vp8Header)
	if err != nil {
		return 0, err
	}

	copy(payload[blankVP8.HeaderSize:], VP8KeyFrame8x8)

	_, err = d.writeStream.WriteRTP(hdr, payload)
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
	_, err := d.writeStream.WriteRTP(hdr, buf[:offset])
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
			targetLayers := d.forwarder.TargetLayers()
			if targetLayers != InvalidLayers {
				d.lastPli.set(time.Now().UnixNano())
				d.receiver.SendPLI(targetLayers.spatial)
				pliOnce = false
			}
		}
	}

	for _, pkt := range pkts {
		switch p := pkt.(type) {
		case *rtcp.PictureLossIndication:
			sendPliOnce()

		case *rtcp.FullIntraRequest:
			sendPliOnce()

		case *rtcp.ReceiverEstimatedMaximumBitrate:
			if d.onREMB != nil {
				d.onREMB(d, p)
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
			}
			if len(rr.Reports) > 0 {
				d.listenerLock.RLock()
				for _, l := range d.receiverReportListeners {
					l(d, rr)
				}
				d.listenerLock.RUnlock()
			}

		case *rtcp.TransportLayerNack:
			var nackedPackets []packetMeta
			for _, pair := range p.Nacks {
				nackedPackets = append(nackedPackets, d.sequencer.getSeqNoPairs(pair.PacketList())...)
			}
			go d.retransmitPackets(nackedPackets)

		case *rtcp.TransportLayerCC:
			if p.MediaSSRC == d.ssrc && d.onTransportCCFeedback != nil {
				d.onTransportCCFeedback(d, p)
			}
		}
	}

	d.connectionStats.RTCPFeedback(pkts, d.ssrc)
}

func (d *DownTrack) retransmitPackets(nackedPackets []packetMeta) {
	var pool *[]byte
	defer func() {
		if pool != nil {
			PacketFactory.Put(pool)
			pool = nil
		}
	}()

	src := PacketFactory.Get().(*[]byte)
	defer PacketFactory.Put(src)

	for _, meta := range nackedPackets {
		if pool != nil {
			PacketFactory.Put(pool)
			pool = nil
		}

		pktBuff := *src
		n, err := d.receiver.ReadRTP(pktBuff, meta.layer, meta.sourceSeqNo)
		if err != nil {
			if err == io.EOF {
				break
			}
			continue
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

			outbuf := &payload
			translatedVP8 := meta.unpackVP8()
			if incomingVP8.HeaderSize != translatedVP8.HeaderSize {
				pool = PacketFactory.Get().(*[]byte)
				outbuf = pool
			}
			payload, err = d.translateVP8PacketTo(&pkt, &incomingVP8, translatedVP8, outbuf)
			if err != nil {
				d.logger.Errorw("translating VP8 packet err", err)
				continue
			}
		}

		err = d.writeRTPHeaderExtensions(&pkt.Header)
		if err != nil {
			d.logger.Errorw("writing rtp header extensions err", err)
			continue
		}

		if _, err = d.writeStream.WriteRTP(&pkt.Header, payload); err != nil {
			d.logger.Errorw("writing rtx packet err", err)
		} else {
			pktSize := pkt.Header.MarshalSize() + len(payload)
			for _, f := range d.onPacketSent {
				f(d, pktSize)
			}

			d.UpdateRtxStats(uint32(pktSize))
		}
	}
}

func (d *DownTrack) getSRStats() (uint64, uint32) {
	primary := d.primaryStats.Load().(*PacketStats)
	rtx := d.rtxStats.Load().(*PacketStats)
	padding := d.paddingStats.Load().(*PacketStats)

	return primary.octets + rtx.octets + padding.octets, primary.packets + rtx.packets + padding.packets
}

// writes RTP header extensions of track
func (d *DownTrack) writeRTPHeaderExtensions(hdr *rtp.Header) error {
	// clear out extensions that may have been in the forwarded header
	hdr.Extension = false
	hdr.ExtensionProfile = 0
	hdr.Extensions = []rtp.Extension{}

	for _, ext := range d.rtpHeaderExtensions {
		if ext.URI != sdp.ABSSendTimeURI {
			// supporting only abs-send-time
			continue
		}

		sendTime := rtp.NewAbsSendTimeExtension(time.Now())
		b, err := sendTime.Marshal()
		if err != nil {
			return err
		}

		err = hdr.SetExtension(uint8(ext.ID), b)
		if err != nil {
			return err
		}
	}

	return nil
}

func (d *DownTrack) getTranslatedRTPHeader(extPkt *buffer.ExtPacket, tpRTP *TranslationParamsRTP) (*rtp.Header, error) {
	hdr := extPkt.Packet.Header
	hdr.PayloadType = d.payloadType
	hdr.Timestamp = tpRTP.timestamp
	hdr.SequenceNumber = tpRTP.sequenceNumber
	hdr.SSRC = d.ssrc

	err := d.writeRTPHeaderExtensions(&hdr)
	if err != nil {
		return nil, err
	}

	return &hdr, nil
}

func (d *DownTrack) translateVP8PacketTo(pkt *rtp.Packet, incomingVP8 *buffer.VP8, translatedVP8 *buffer.VP8, outbuf *[]byte) ([]byte, error) {
	var buf []byte
	if outbuf == &pkt.Payload {
		buf = pkt.Payload
	} else {
		buf = (*outbuf)[:len(pkt.Payload)+translatedVP8.HeaderSize-incomingVP8.HeaderSize]

		srcPayload := pkt.Payload[incomingVP8.HeaderSize:]
		dstPayload := buf[translatedVP8.HeaderSize:]
		copy(dstPayload, srcPayload)
	}

	err := translatedVP8.MarshalTo(buf[:translatedVP8.HeaderSize])
	return buf, err
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
		"LastRTP":           d.lastRTP.get(),
		"LastPli":           d.lastPli.get(),
		"PacketsDropped":    d.pktsDropped.get(),
	}

	senderReport := d.CreateSenderReport()
	if senderReport != nil {
		stats["NTPTime"] = senderReport.NTPTime
		stats["RTPTime"] = senderReport.RTPTime
		stats["PacketCount"] = senderReport.PacketCount
	}

	return map[string]interface{}{
		"PeerID":              d.peerID,
		"TrackID":             d.id,
		"StreamID":            d.streamID,
		"SSRC":                d.ssrc,
		"MimeType":            d.codec.MimeType,
		"Bound":               d.bound.get(),
		"Muted":               d.forwarder.IsMuted(),
		"CurrentSpatialLayer": d.forwarder.CurrentLayers().spatial,
		"Stats":               stats,
	}
}

func (d *DownTrack) GetConnectionScore() float64 {
	return d.connectionStats.GetScore()
}

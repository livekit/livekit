package sfu

import (
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/transport/packetio"
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
	UpTrackLayersChange(availableLayers []int32, exemptedLayers []int32)
	UpTrackBitrateAvailabilityChange()
	WriteRTP(p *buffer.ExtPacket, layer int32) error
	Close()
	IsClosed() bool
	// ID is the globally unique identifier for this Track.
	ID() string
	SubscriberID() livekit.ParticipantID
	TrackInfoAvailable()
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
	ErrDownTrackAlreadyBound             = errors.New("already bound")
	ErrDownTrackClosed                   = errors.New("downtrack closed")
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
	bindLock      sync.Mutex
	logger        logger.Logger
	id            livekit.TrackID
	subscriberID  livekit.ParticipantID
	bound         atomic.Bool
	kind          webrtc.RTPCodecType
	mime          string
	ssrc          uint32
	streamID      string
	maxTrack      int
	payloadType   uint8
	sequencer     *sequencer
	bufferFactory *buffer.Factory

	forwarder *Forwarder

	upstreamCodecs          []webrtc.RTPCodecParameters
	codec                   webrtc.RTPCodecCapability
	rtpHeaderExtensions     []webrtc.RTPHeaderExtensionParameter
	absSendTimeID           int
	dependencyDescriptorID  int
	receiver                TrackReceiver
	transceiver             *webrtc.RTPTransceiver
	writeStream             webrtc.TrackLocalWriter
	onCloseHandler          func(willBeResumed bool)
	onBind                  func()
	receiverReportListeners []ReceiverReportListener
	listenerLock            sync.RWMutex
	isClosed                atomic.Bool
	connected               atomic.Bool

	rtpStats *buffer.RTPStats

	statsLock          sync.RWMutex
	totalRepeatedNACKs uint32

	keyFrameRequestGeneration atomic.Uint32

	blankFramesGeneration atomic.Uint32

	connectionStats      *connectionquality.ConnectionStats
	deltaStatsSnapshotId uint32

	// Debug info
	pktsDropped   atomic.Uint32
	writeIOErrors atomic.Uint32

	isNACKThrottled atomic.Bool

	// RTCP callbacks
	onREMB                func(dt *DownTrack, remb *rtcp.ReceiverEstimatedMaximumBitrate)
	onTransportCCFeedback func(dt *DownTrack, cc *rtcp.TransportLayerCC)

	// simulcast layer availability change callback
	onAvailableLayersChanged atomic.Value // func(dt *DownTrack)

	// layer bitrate availability change callback
	onBitrateAvailabilityChanged func(dt *DownTrack)

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
	d.forwarder = NewForwarder(d.kind, d.logger)

	d.connectionStats = connectionquality.NewConnectionStats(connectionquality.ConnectionStatsParams{
		MimeType:      codecs[0].MimeType, // LK-TODO have to notify on codec change
		GetDeltaStats: d.getDeltaStats,
		GetMaxExpectedLayer: func() int32 {
			return d.forwarder.MaxLayers().Spatial
		},
		GetCurrentLayerSpatial: func() int32 {
			return d.forwarder.CurrentLayers().Spatial
		},
		GetIsReducedQuality: d.forwarder.IsReducedQuality,
		Logger:              d.logger,
	})
	d.connectionStats.OnStatsUpdate(func(_cs *connectionquality.ConnectionStats, stat *livekit.AnalyticsStat) {
		if d.onStatsUpdate != nil {
			d.onStatsUpdate(d, stat)
		}
	})

	d.rtpStats = buffer.NewRTPStats(buffer.RTPStatsParams{
		ClockRate:              d.codec.ClockRate,
		IsReceiverReportDriven: true,
		Logger:                 d.logger,
	})
	d.deltaStatsSnapshotId = d.rtpStats.NewSnapshotId()

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
	}
	d.sequencer = newSequencer(d.maxTrack, d.logger)
	d.codec = codec.RTPCodecCapability
	d.forwarder.DetermineCodec(d.codec)
	if d.onBind != nil {
		d.onBind()
	}
	d.bound.Store(true)
	d.bindLock.Unlock()

	d.logger.Debugw("downtrack bound")

	return codec, nil
}

// Unbind implements the teardown logic when the track is no longer needed. This happens
// because a track has been stopped.
func (d *DownTrack) Unbind(_ webrtc.TrackLocalContext) error {
	d.bound.Store(false)
	return nil
}

func (d *DownTrack) TrackInfoAvailable() {
	d.connectionStats.Start(d.receiver.TrackInfo())
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

	// TODO : for svc, don't need pli/lrr when layer comes down
	locked, layer := d.forwarder.CheckSync()
	if !locked {
		go d.keyFrameRequester(d.keyFrameRequestGeneration.Load(), layer)
	}
}

func (d *DownTrack) stopKeyFrameRequester() {
	d.keyFrameRequestGeneration.Inc()
}

func (d *DownTrack) keyFrameRequester(generation uint32, layer int32) {
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

	if !d.bound.Load() {
		return nil
	}

	tp, err := d.forwarder.GetTranslationParams(extPkt, layer)
	if tp.shouldDrop {
		if tp.isDroppingRelevant {
			d.pktsDropped.Inc()
		}
		if err != nil {
			d.logger.Errorw("write rtp packet failed", err)
		}
		return err
	}

	payload := extPkt.Packet.Payload
	if tp.vp8 != nil {
		incomingVP8, _ := extPkt.Payload.(buffer.VP8)
		pool = PacketFactory.Get().(*[]byte)
		payload, err = d.translateVP8PacketTo(extPkt.Packet, &incomingVP8, tp.vp8.Header, pool)
		if err != nil {
			d.pktsDropped.Inc()
			d.logger.Errorw("write rtp packet failed", err)
			return err
		}
	}

	var meta *packetMeta
	if d.sequencer != nil {
		meta = d.sequencer.push(extPkt.Packet.SequenceNumber, tp.rtp.sequenceNumber, tp.rtp.timestamp, int8(layer))
		if meta != nil && tp.vp8 != nil {
			meta.packVP8(tp.vp8.Header)
		}
	}

	hdr, err := d.getTranslatedRTPHeader(extPkt, tp)
	if err != nil {
		d.pktsDropped.Inc()
		d.logger.Errorw("write rtp packet failed", err)
		return err
	}

	if meta != nil && d.dependencyDescriptorID != 0 {
		meta.ddBytes = hdr.GetExtension(uint8(d.dependencyDescriptorID))
	}

	_, err = d.writeStream.WriteRTP(hdr, payload)
	if err != nil {
		d.pktsDropped.Inc()
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

	pktSize := hdr.MarshalSize() + len(payload)
	for _, f := range d.onPacketSent {
		f(d, pktSize)
	}

	if tp.isSwitchingToMaxLayer && d.onMaxLayerChanged != nil && d.kind == webrtc.RTPCodecTypeVideo {
		d.onMaxLayerChanged(d, layer)
	}

	if extPkt.KeyFrame || tp.switchingToTargetLayer {
		d.isNACKThrottled.Store(false)
		d.rtpStats.UpdateKeyFrame(1)

		locked, _ := d.forwarder.CheckSync()
		if locked {
			d.stopKeyFrameRequester()
		}

		if !tp.switchingToTargetLayer {
			d.logger.Debugw("forwarding key frame", "layer", layer)
		}
	}

	d.rtpStats.Update(hdr, len(payload), 0, time.Now().UnixNano())
	return nil
}

// WritePaddingRTP tries to write as many padding only RTP packets as necessary
// to satisfy given size to the DownTrack
func (d *DownTrack) WritePaddingRTP(bytesToSend int) int {
	if !d.rtpStats.IsActive() {
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
		for _, f := range d.onPaddingSent {
			f(d, size)
		}
		d.rtpStats.Update(&hdr, 0, len(payload), time.Now().UnixNano())

		//
		// Register with sequencer with invalid layer so that NACKs for these can be filtered out.
		// Retransmission is probably a sign of network congestion/badness.
		// So, retransmitting padding packets is only going to make matters worse.
		//
		if d.sequencer != nil {
			d.sequencer.push(0, hdr.SequenceNumber, hdr.Timestamp, int8(InvalidLayerSpatial))
		}

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

	if d.onMaxLayerChanged != nil && d.kind == webrtc.RTPCodecTypeVideo {
		notifyLayer := InvalidLayerSpatial
		if !muted {
			//
			// When unmuting, don't wait for layer lock as
			// client might need to be notified to start layers
			// before locking can happen in the forwarder.
			//
			notifyLayer = maxLayers.Spatial
		}
		d.onMaxLayerChanged(d, notifyLayer)
	}

	if d.onSubscriptionChanged != nil {
		d.onSubscriptionChanged(d)
	}

	// when muting, send a few silence frames to ensure residual noise does not
	// put the comfort noise generator on decoder side in a bad state where it
	// generates noise that is not so comfortable.
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
// 1. When transceiver is reused by other participant's video track,
//    set flush=true to avoid previous video shows before previous stream is displayed.
// 2. in case of session migration, participant migrate from other node, video track should
//    be resumed with same participant, set flush=false since we don't need to flush decoder.
func (d *DownTrack) CloseWithFlush(flush bool) {
	if d.isClosed.Swap(true) {
		// already closed
		return
	}

	d.bindLock.Lock()
	d.logger.Infow("close down track", "flushBlankFrame", flush)
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
	}

	d.bindLock.Unlock()
	d.connectionStats.Close()
	d.rtpStats.Stop()
	d.logger.Infow("rtp stats", "direction", "downstream", "stats", d.rtpStats.ToString())

	if d.onMaxLayerChanged != nil && d.kind == webrtc.RTPCodecTypeVideo {
		d.onMaxLayerChanged(d, InvalidLayerSpatial)
	}

	if d.onCloseHandler != nil {
		d.onCloseHandler(!flush)
	}

	d.stopKeyFrameRequester()
}

func (d *DownTrack) SetMaxSpatialLayer(spatialLayer int32) {
	changed, maxLayers, currentLayers := d.forwarder.SetMaxSpatialLayer(spatialLayer)
	if !changed {
		return
	}

	if d.onMaxLayerChanged != nil && d.kind == webrtc.RTPCodecTypeVideo && maxLayers.SpatialGreaterThanOrEqual(currentLayers) {
		//
		// Notify when new max is
		//   1. Equal to current -> already locked to the new max
		//   2. Greater than current -> two scenarios
		//      a. is higher than previous max -> client may need to start higher layer before forwarder can lock
		//      b. is lower than previous max -> client can stop higher layer(s)
		//
		d.onMaxLayerChanged(d, maxLayers.Spatial)
	}

	if d.onSubscribedLayersChanged != nil {
		d.onSubscribedLayersChanged(d, maxLayers)
	}
}

func (d *DownTrack) SetMaxTemporalLayer(temporalLayer int32) {
	changed, maxLayers, _ := d.forwarder.SetMaxTemporalLayer(temporalLayer)
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

func (d *DownTrack) GetForwarderState() ForwarderState {
	return d.forwarder.GetState()
}

func (d *DownTrack) SeedForwarderState(state ForwarderState) {
	d.forwarder.SeedState(state)
}

func (d *DownTrack) GetForwardingStatus() ForwardingStatus {
	return d.forwarder.GetForwardingStatus()
}

func (d *DownTrack) UpTrackLayersChange(availableLayers []int32, exemptedLayers []int32) {
	d.forwarder.UpTrackLayersChange(availableLayers, exemptedLayers)

	if onAvailableLayersChanged, ok := d.onAvailableLayersChanged.Load().(func(dt *DownTrack)); ok {
		onAvailableLayersChanged(d)
	}
}

func (d *DownTrack) UpTrackBitrateAvailabilityChange() {
	if d.onBitrateAvailabilityChanged != nil {
		d.onBitrateAvailabilityChanged(d)
	}
}

// OnCloseHandler method to be called on remote tracked removed
func (d *DownTrack) OnCloseHandler(fn func(willBeResumed bool)) {
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
	d.onAvailableLayersChanged.Store(fn)
}

func (d *DownTrack) OnBitrateAvailabilityChanged(fn func(dt *DownTrack)) {
	d.onBitrateAvailabilityChanged = fn
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

func (d *DownTrack) OnRttUpdate(fn func(dt *DownTrack, rtt uint32)) {
	d.onRttUpdate = fn
}

func (d *DownTrack) OnMaxLayerChanged(fn func(dt *DownTrack, layer int32)) {
	d.onMaxLayerChanged = fn
}

func (d *DownTrack) IsDeficient() bool {
	return d.forwarder.IsDeficient()
}

func (d *DownTrack) BandwidthRequested() int64 {
	return d.forwarder.BandwidthRequested(d.receiver.GetBitrateTemporalCumulative())
}

func (d *DownTrack) DistanceToDesired() int32 {
	return d.forwarder.DistanceToDesired()
}

func (d *DownTrack) AllocateOptimal(allowOvershoot bool) VideoAllocation {
	allocation := d.forwarder.AllocateOptimal(d.receiver.GetBitrateTemporalCumulative(), allowOvershoot)
	d.logger.Debugw("stream: allocation optimal available", "allocation", allocation)
	d.maybeStartKeyFrameRequester()
	return allocation
}

func (d *DownTrack) ProvisionalAllocatePrepare() {
	d.forwarder.ProvisionalAllocatePrepare(d.receiver.GetBitrateTemporalCumulative())
}

func (d *DownTrack) ProvisionalAllocate(availableChannelCapacity int64, layers VideoLayers, allowPause bool, allowOvershoot bool) int64 {
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
	d.logger.Debugw("stream: allocation commit", "allocation", allocation)
	d.maybeStartKeyFrameRequester()
	return allocation
}

func (d *DownTrack) AllocateNextHigher(availableChannelCapacity int64, allowOvershoot bool) (VideoAllocation, bool) {
	allocation, available := d.forwarder.AllocateNextHigher(availableChannelCapacity, d.receiver.GetBitrateTemporalCumulative(), allowOvershoot)
	d.logger.Debugw("stream: allocation next higher layer", "allocation", allocation, "available", available)
	d.maybeStartKeyFrameRequester()
	return allocation, available
}

func (d *DownTrack) GetNextHigherTransition(allowOvershoot bool) (VideoTransition, bool) {
	transition, available := d.forwarder.GetNextHigherTransition(d.receiver.GetBitrateTemporalCumulative(), allowOvershoot)
	d.logger.Debugw("stream: get next higher layer", "transition", transition, "available", available)
	return transition, available
}

func (d *DownTrack) Pause() VideoAllocation {
	allocation := d.forwarder.Pause(d.receiver.GetBitrateTemporalCumulative())
	d.logger.Debugw("stream: pause", "allocation", allocation)
	d.maybeStartKeyFrameRequester()
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

	return d.rtpStats.GetRtcpSenderReport(d.ssrc)
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
		case "video/vp8":
			writeBlankFrame = d.writeVP8BlankFrame
		case "video/h264":
			writeBlankFrame = d.writeH264BlankFrame
		default:
			close(done)
			return
		}

		frameRate := uint32(30)
		if d.mime == "audio/opus" {
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

				for _, f := range d.onPacketSent {
					f(d, pktSize)
				}

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

func (d *DownTrack) writeVP8BlankFrame(hdr *rtp.Header, frameEndNeeded bool) (int, error) {
	blankVP8 := d.forwarder.GetPaddingVP8(frameEndNeeded)

	// 8x8 key frame
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
			targetLayers := d.forwarder.TargetLayers()
			if targetLayers != InvalidLayers {
				d.logger.Debugw("sending PLI RTCP", "layer", targetLayers.Spatial)
				d.receiver.SendPLI(targetLayers.Spatial, false)
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

				rtt := getRttMs(&r)
				if rtt != d.rtpStats.GetRtt() {
					rttToReport = rtt
				}

				d.rtpStats.UpdateFromReceiverReport(r.LastSequenceNumber, r.TotalLost, rtt, float64(r.Jitter))
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
			if p.MediaSSRC == d.ssrc && d.onTransportCCFeedback != nil {
				d.onTransportCCFeedback(d, p)
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
		if d.bound.Load() && d.kind == webrtc.RTPCodecTypeVideo {
			targetLayers := d.forwarder.TargetLayers()
			if targetLayers != InvalidLayers {
				d.receiver.SendPLI(targetLayers.Spatial, true)
			}
		}
	}
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
	for _, meta := range d.sequencer.getPacketsMeta(filtered) {
		if meta.layer == int8(InvalidLayerSpatial) {
			// padding packet, no RTX for those
			continue
		}

		if disallowedLayers[meta.layer] {
			continue
		}

		nackAcks++

		if pool != nil {
			PacketFactory.Put(pool)
			pool = nil
		}

		pktBuff := *src
		n, err := d.receiver.ReadRTP(pktBuff, uint8(meta.layer), meta.sourceSeqNo)
		if err != nil {
			nackMisses++
			if err == io.EOF {
				break
			}
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

			translatedVP8 := meta.unpackVP8()
			pool = PacketFactory.Get().(*[]byte)
			payload, err = d.translateVP8PacketTo(&pkt, &incomingVP8, translatedVP8, pool)
			if err != nil {
				d.logger.Errorw("translating VP8 packet err", err)
				continue
			}
		}

		var extraExtensions []extensionData
		if len(meta.ddBytes) > 0 {
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
			pktSize := pkt.Header.MarshalSize() + len(payload)
			for _, f := range d.onPacketSent {
				f(d, pktSize)
			}

			d.rtpStats.Update(&pkt.Header, len(payload), 0, time.Now().UnixNano())
		}
	}

	d.statsLock.Lock()
	d.totalRepeatedNACKs += numRepeatedNACKs
	d.statsLock.Unlock()

	d.rtpStats.UpdateNackProcessed(nackAcks, nackMisses, numRepeatedNACKs)
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
		hdr.SetExtension(uint8(ext.id), ext.payload)
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
	if d.dependencyDescriptorID != 0 && tp.ddExtension != nil {
		bytes, err := tp.ddExtension.Marshal()
		if err != nil {
			d.logger.Warnw("error marshalling dependency descriptor extension", err)
		} else {
			extension = append(extension, extensionData{
				id:      uint8(d.dependencyDescriptorID),
				payload: bytes,
			})
		}
	}
	err := d.writeRTPHeaderExtensions(&hdr, extension...)
	if err != nil {
		return nil, err
	}

	return &hdr, nil
}

func (d *DownTrack) translateVP8PacketTo(pkt *rtp.Packet, incomingVP8 *buffer.VP8, translatedVP8 *buffer.VP8, outbuf *[]byte) ([]byte, error) {
	buf := (*outbuf)[:len(pkt.Payload)+translatedVP8.HeaderSize-incomingVP8.HeaderSize]
	srcPayload := pkt.Payload[incomingVP8.HeaderSize:]
	dstPayload := buf[translatedVP8.HeaderSize:]
	copy(dstPayload, srcPayload)

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
		"LastPli":           d.rtpStats.LastPli(),
		"PacketsDropped":    d.pktsDropped.Load(),
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
		"CurrentSpatialLayer": d.forwarder.CurrentLayers().Spatial,
		"Stats":               stats,
	}
}

func (d *DownTrack) GetConnectionScore() float32 {
	return d.connectionStats.GetScore()
}

func (d *DownTrack) GetTrackStats() *livekit.RTPStats {
	return d.rtpStats.ToProto()
}

func (d *DownTrack) getDeltaStats() map[uint32]*buffer.StreamStatsWithLayers {
	streamStats := make(map[uint32]*buffer.StreamStatsWithLayers, 1)

	deltaStats := d.rtpStats.DeltaInfo(d.deltaStatsSnapshotId)
	if deltaStats == nil {
		return nil
	}

	streamStats[d.ssrc] = &buffer.StreamStatsWithLayers{
		RTPStats: deltaStats,
		Layers: map[int32]*buffer.RTPDeltaInfo{
			0: deltaStats,
		},
	}

	return streamStats
}

func (d *DownTrack) GetNackStats() (totalPackets uint32, totalRepeatedNACKs uint32) {
	totalPackets = d.rtpStats.GetTotalPacketsPrimary()

	d.statsLock.RLock()
	totalRepeatedNACKs = d.totalRepeatedNACKs
	d.statsLock.RUnlock()

	return
}

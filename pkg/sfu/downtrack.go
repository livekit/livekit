package sfu

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/elliotchance/orderedmap"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/transport/packetio"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

// TrackSender defines a  interface send media to remote peer
type TrackSender interface {
	UptrackLayersChange(availableLayers []uint16, layerAdded bool) (int32, error)
	WriteRTP(p *buffer.ExtPacket, layer int32) error
	Close()
	// ID is the globally unique identifier for this Track.
	ID() string
	SetTrackType(isSimulcast bool)
	PeerID() string
}

// DownTrackType determines the type of track
type DownTrackType int

const (
	SimpleDownTrack DownTrackType = iota + 1
	SimulcastDownTrack
)

const (
	RTPPaddingMaxPayloadSize      = 255
	RTPPaddingEstimatedHeaderSize = 20
	RTPBlankFramesMax             = 6
)

type SequenceNumberOrdering int

const (
	SequenceNumberOrderingContiguous SequenceNumberOrdering = iota
	SequenceNumberOrderingOutOfOrder
	SequenceNumberOrderingGap
	SequenceNumberOrderingUnknown
)

var (
	ErrOutOfOrderSequenceNumberCacheMiss = errors.New("out-of-order sequence number not found in cache")
	ErrPaddingOnlyPacket                 = errors.New("padding only packet that need not be forwarded")
	ErrDuplicatePacket                   = errors.New("duplicate packet")
	ErrPaddingNotOnFrameBoundary         = errors.New("padding cannot send on non-frame boundary")
	ErrNotVP8                            = errors.New("not VP8")
	ErrOutOfOrderVP8PictureIdCacheMiss   = errors.New("out-of-order VP8 picture id not found in cache")
	ErrFilteredVP8TemporalLayer          = errors.New("filtered VP8 temporal layer")
	ErrNoRequiredBuff                    = errors.New("buff size if less than required")
)

var (
	VP8KeyFrame1x1 = []byte{0x10, 0x02, 0x00, 0x9d, 0x01, 0x2a, 0x01, 0x00, 0x01, 0x00, 0x0b, 0xc7, 0x08, 0x85, 0x85, 0x88, 0x85, 0x84, 0x88, 0x3f, 0x82, 0x00, 0x0c, 0x0d, 0x60, 0x00, 0xfe, 0xe6, 0xb5, 0x00}

	H264KeyFrame2x2SPS = []byte{0x67, 0x42, 0xc0, 0x1f, 0x0f, 0xd9, 0x1f, 0x88, 0x88, 0x84, 0x00, 0x00, 0x03, 0x00, 0x04, 0x00, 0x00, 0x03, 0x00, 0xc8, 0x3c, 0x60, 0xc9, 0x20}
	H264KeyFrame2x2PPS = []byte{0x68, 0x87, 0xcb, 0x83, 0xcb, 0x20}
	H264KeyFrame2x2IDR = []byte{0x65, 0x88, 0x84, 0x0a, 0xf2, 0x62, 0x80, 0x00, 0xa7, 0xbe}

	H264KeyFrame2x2 = [][]byte{H264KeyFrame2x2SPS, H264KeyFrame2x2PPS, H264KeyFrame2x2IDR}
)

type simulcastTrackHelpers struct {
	switchDelay       time.Time
	temporalSupported bool
	temporalEnabled   bool
	lTSCalc           atomicInt64
}

type ReceiverReportListener func(dt *DownTrack, report *rtcp.ReceiverReport)

// DownTrack  implements TrackLocal, is the track used to write packets
// to SFU Subscriber, the track handle the packets for simple, simulcast
// and SVC Publisher.
type DownTrack struct {
	id            string
	peerID        string
	bound         atomicBool
	mime          string
	ssrc          uint32
	streamID      string
	maxTrack      int
	payloadType   uint8
	sequencer     *sequencer
	trackType     DownTrackType
	bufferFactory *buffer.Factory
	payload       *[]byte

	currentSpatialLayer atomicInt32
	targetSpatialLayer  atomicInt32
	temporalLayer       atomicInt32

	enabled  atomicBool
	reSync   atomicBool
	lastSSRC atomicUint32

	munger    *Munger
	vp8Munger *VP8Munger

	simulcast        simulcastTrackHelpers
	maxSpatialLayer  atomicInt32
	maxTemporalLayer atomicInt32

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
	octetCount   atomicUint32
	packetCount  atomicUint32
	lossFraction atomicUint8

	// Debug info
	lastPli                         atomicInt64
	lastRTP                         atomicInt64
	pktsMuted                       atomicUint32
	pktsDropped                     atomicUint32
	pktsBandwidthConstrainedDropped atomicUint32

	maxPacketTs uint32

	bandwidthConstrainedMuted atomicBool

	// RTCP callbacks
	onRTCP func([]rtcp.Packet)
	onREMB func(dt *DownTrack, remb *rtcp.ReceiverEstimatedMaximumBitrate)

	// simulcast layer availability change callback
	onAvailableLayersChanged func(dt *DownTrack, layerAdded bool)

	// subscription change callback
	onSubscriptionChanged func(dt *DownTrack)

	// max layer change callback
	onSubscribedLayersChanged func(dt *DownTrack, maxSpatialLayer int32, maxTemporalLayer int32)

	// packet sent callback
	onPacketSent []func(dt *DownTrack, size int)
}

// NewDownTrack returns a DownTrack.
func NewDownTrack(c webrtc.RTPCodecCapability, r TrackReceiver, bf *buffer.Factory, peerID string, mt int) (*DownTrack, error) {
	d := &DownTrack{
		id:            r.TrackID(),
		peerID:        peerID,
		maxTrack:      mt,
		streamID:      r.StreamID(),
		bufferFactory: bf,
		receiver:      r,
		codec:         c,
		munger:        NewMunger(),
	}

	if strings.ToLower(c.MimeType) == "video/vp8" {
		d.vp8Munger = NewVP8Munger()
		d.payload = packetFactory.Get().(*[]byte)
	}

	if d.Kind() == webrtc.RTPCodecTypeVideo {
		d.maxSpatialLayer.set(2)
		d.maxTemporalLayer.set(2)
	} else {
		d.maxSpatialLayer.set(-1)
		d.maxTemporalLayer.set(-1)
	}

	return d, nil
}

func (d *DownTrack) SetTrackType(isSimulcast bool) {
	if isSimulcast {
		d.trackType = SimulcastDownTrack
	} else {
		d.trackType = SimpleDownTrack
	}
}

// Bind is called by the PeerConnection after negotiation is complete
// This asserts that the code requested is supported by the remote peer.
// If so it setups all the state (SSRC and PayloadType) to have a call
func (d *DownTrack) Bind(t webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	parameters := webrtc.RTPCodecParameters{RTPCodecCapability: d.codec}
	if codec, err := codecParametersFuzzySearch(parameters, t.CodecParameters()); err == nil {
		d.ssrc = uint32(t.SSRC())
		d.payloadType = uint8(codec.PayloadType)
		d.writeStream = t.WriteStream()
		d.mime = strings.ToLower(codec.MimeType)
		d.reSync.set(true)
		d.enabled.set(true)
		d.bandwidthConstrainedMute(false)
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
func (d *DownTrack) ID() string { return d.id }

// Codec returns current track codec capability
func (d *DownTrack) Codec() webrtc.RTPCodecCapability { return d.codec }

// StreamID is the group this track belongs too. This must be unique
func (d *DownTrack) StreamID() string { return d.streamID }

func (d *DownTrack) PeerID() string { return d.peerID }

// Sets RTP header extensions for this track
func (d *DownTrack) SetRTPHeaderExtensions(rtpHeaderExtensions []webrtc.RTPHeaderExtensionParameter) {
	d.rtpHeaderExtensions = rtpHeaderExtensions
}

// Kind controls if this TrackLocal is audio or video
func (d *DownTrack) Kind() webrtc.RTPCodecType {
	switch {
	case strings.HasPrefix(d.codec.MimeType, "audio/"):
		return webrtc.RTPCodecTypeAudio
	case strings.HasPrefix(d.codec.MimeType, "video/"):
		return webrtc.RTPCodecTypeVideo
	default:
		return webrtc.RTPCodecType(0)
	}
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

func (d *DownTrack) maybeTranslateVP8(pkt *rtp.Packet, meta packetMeta) error {
	if d.vp8Munger == nil || len(pkt.Payload) == 0 {
		return nil
	}

	var incomingVP8 buffer.VP8
	if err := incomingVP8.Unmarshal(pkt.Payload); err != nil {
		return err
	}

	translatedVP8 := meta.unpackVP8()
	payload, err := d.translateVP8Packet(pkt, &incomingVP8, translatedVP8, false)
	if err != nil {
		return err
	}

	pkt.Payload = payload
	return nil
}

// Writes RTP header extensions of track
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

// WriteRTP writes a RTP Packet to the DownTrack
func (d *DownTrack) WriteRTP(p *buffer.ExtPacket, layer int32) error {
	d.lastRTP.set(time.Now().UnixNano())

	if !d.bound.get() {
		return nil
	}
	if !d.enabled.get() {
		d.pktsMuted.add(1)
		return nil
	}

	if d.bandwidthConstrainedMuted.get() {
		d.pktsBandwidthConstrainedDropped.add(1)
		return nil
	}

	switch d.trackType {
	case SimpleDownTrack:
		return d.writeSimpleRTP(p)
	case SimulcastDownTrack:
		return d.writeSimulcastRTP(p, layer)
	}
	return nil
}

// WritePaddingRTP tries to write as many padding only RTP packets as necessary
// to satisfy given size to the DownTrack
func (d *DownTrack) WritePaddingRTP(bytesToSend int) int {
	// LK-TODO-START
	// Potentially write padding even if muted. Given that padding
	// can be sent only on frame boudaries, writing on disabled tracks
	// will give more options. But, it is possible that forwarding stopped
	// on a non-frame boundary when the track is muted.
	// LK-TODO-END
	if !d.enabled.get() || d.packetCount.get() == 0 {
		return 0
	}

	// LK-TODO-START
	// Ideally should look at header extensions negotiated for
	// track and decide if padding can be sent. But, browsers behave
	// in unexpected ways when using audio for bandwidth estimation and
	// padding is mainly used to probe for excess available bandwidth.
	// So, to be safe, limit to video tracks
	// LK-TODO-END
	if d.Kind() == webrtc.RTPCodecTypeAudio {
		return 0
	}

	// LK-TODO Look at load balancing a la sfu.Receiver to spread across available CPUs
	bytesSent := 0
	for {
		size := bytesToSend
		// RTP padding maximum is 255 bytes. Break it up.
		// Use 20 byte as estimate of RTP header size (12 byte header + 8 byte extension)
		if size > RTPPaddingMaxPayloadSize+RTPPaddingEstimatedHeaderSize {
			size = RTPPaddingMaxPayloadSize + RTPPaddingEstimatedHeaderSize
		}

		sn, ts, err := d.munger.UpdateAndGetPaddingSnTs(false)
		if err != nil {
			return bytesSent
		}

		// LK-TODO-START
		// Hold sending padding packets till first RTCP-RR is received for this RTP stream.
		// That is definitive proof that the remote side knows about this RTP stream.
		// The packet count check at the beginning of this function gates sending padding
		// on as yet unstarted streams which is a reasonble check.
		// LK-TODO-END

		// intentionally ignoring check for bandwidth constrained mute
		// as padding is typically used to probe for channel capacity
		// and sending it on any track achieves the purpose of probing
		// the channel.

		hdr := rtp.Header{
			Version:        2,
			Padding:        true,
			Marker:         false,
			PayloadType:    d.payloadType,
			SequenceNumber: sn,
			Timestamp:      ts,
			SSRC:           d.ssrc,
			CSRC:           []uint32{},
		}

		err = d.writeRTPHeaderExtensions(&hdr)
		if err != nil {
			return bytesSent
		}

		payloadSize := size - RTPPaddingEstimatedHeaderSize
		payload := make([]byte, payloadSize)
		// last byte of padding has padding size including that byte
		payload[payloadSize-1] = byte(payloadSize)

		_, err = d.writeStream.WriteRTP(&hdr, payload)
		if err != nil {
			return bytesSent
		}

		// LK-TODO - check if we should keep separate padding stats
		size = hdr.MarshalSize() + len(payload)
		d.UpdateStats(uint32(size))

		// LK-TODO-START
		// NACK buffer for these probe packets.
		// Probably okay to absorb the NACKs for these and ignore them.
		// Retransmssion is probably a sign of network congestion/badness.
		// So, retransmitting padding packets is only going to make matters worse.
		// LK-TODO-END

		bytesSent += size
		bytesToSend -= size
		if bytesToSend <= 0 {
			break
		}
	}

	return bytesSent
}

func (d *DownTrack) Enabled() bool {
	return d.enabled.get()
}

// Mute enables or disables media forwarding
func (d *DownTrack) Mute(val bool) {
	if d.enabled.get() != val {
		return
	}
	d.enabled.set(!val)
	if val {
		d.lossFraction.set(0)
		d.reSync.set(val)
	}

	if d.onSubscriptionChanged != nil {
		d.onSubscriptionChanged(d)
	}
}

// Close track
func (d *DownTrack) Close() {
	d.enabled.set(false)

	// write blank frames after disabling so that other frames do not interfere.
	// Idea here is to send blank 1x1 key frames to flush the decoder buffer at the remote end.
	// Otherwise, with transceiver re-use last frame from previous stream is held in the
	// display buffer and there could be a brief moment where the previous stream is displayed.
	d.writeBlankFrameRTP()

	d.closeOnce.Do(func() {
		Logger.V(1).Info("Closing sender", "peer_id", d.peerID, "kind", d.Kind())
		if d.payload != nil {
			packetFactory.Put(d.payload)
		}
		if d.onCloseHandler != nil {
			d.onCloseHandler()
		}
	})
}

func (d *DownTrack) CurrentSpatialLayer() int32 {
	return d.currentSpatialLayer.get()
}

func (d *DownTrack) TargetSpatialLayer() int32 {
	return d.targetSpatialLayer.get()
}

func (d *DownTrack) SetMaxSpatialLayer(spatialLayer int32) error {
	// LK-TODO: support SVC
	if d.trackType != SimulcastDownTrack {
		return ErrSpatialNotSupported
	}

	if spatialLayer == d.MaxSpatialLayer() {
		return nil
	}

	d.maxSpatialLayer.set(spatialLayer)

	if d.onSubscribedLayersChanged != nil {
		d.onSubscribedLayersChanged(d, spatialLayer, d.MaxTemporalLayer())
	}

	return nil
}

func (d *DownTrack) MaxSpatialLayer() int32 {
	return d.maxSpatialLayer.get()
}

func (d *DownTrack) SetMaxTemporalLayer(temporalLayer int32) {
	if temporalLayer == d.MaxTemporalLayer() {
		return
	}

	d.maxTemporalLayer.set(temporalLayer)

	if d.onSubscribedLayersChanged != nil {
		d.onSubscribedLayersChanged(d, d.MaxSpatialLayer(), temporalLayer)
	}
}

func (d *DownTrack) MaxTemporalLayer() int32 {
	return d.maxTemporalLayer.get()
}

// switchSpatialLayer switches the current layer
func (d *DownTrack) switchSpatialLayer(targetLayer int32) error {
	if d.trackType != SimulcastDownTrack {
		return ErrSpatialNotSupported
	}

	// already set
	if d.CurrentSpatialLayer() == targetLayer {
		return nil
	}

	d.targetSpatialLayer.set(targetLayer)
	return nil
}

func (d *DownTrack) UptrackLayersChange(availableLayers []uint16, layerAdded bool) (int32, error) {
	if d.trackType == SimulcastDownTrack {
		currentLayer := uint16(d.CurrentSpatialLayer())
		maxLayer := uint16(d.maxSpatialLayer.get())

		var maxFound uint16 = 0
		layerFound := false
		var minFound uint16 = 0
		for _, target := range availableLayers {
			if target <= maxLayer {
				if target > maxFound {
					maxFound = target
					layerFound = true
				}
			} else {
				if minFound > target {
					minFound = target
				}
			}
		}
		var targetLayer uint16
		if layerFound {
			targetLayer = maxFound
		} else {
			targetLayer = minFound
		}
		if currentLayer != targetLayer {
			// LK-TODO-START
			// This layer switch should be removed when StreamAllocator is used.
			// Available layers change should be signalled to StreamAllocator
			// and StreamAllocator will take care of adjusting allocations.
			// LK-TODO-END
			if err := d.switchSpatialLayer(int32(targetLayer)); err != nil {
				return int32(targetLayer), err
			}
		}

		if d.onAvailableLayersChanged != nil {
			d.onAvailableLayersChanged(d, layerAdded)
		}

		return int32(targetLayer), nil
	}
	return -1, fmt.Errorf("downtrack %s does not support simulcast", d.id)
}

func (d *DownTrack) switchTemporalLayer(targetLayer int32) {
	if d.trackType != SimulcastDownTrack {
		return
	}

	layer := d.temporalLayer.get()
	currentLayer := uint16(layer)
	currentTargetLayer := uint16(layer >> 16)

	// Don't switch until previous switch is done or canceled
	if currentLayer != currentTargetLayer {
		return
	}

	d.temporalLayer.set((targetLayer << 16) | int32(currentLayer))
}

// OnCloseHandler method to be called on remote tracked removed
func (d *DownTrack) OnCloseHandler(fn func()) {
	d.onCloseHandler = fn
}

func (d *DownTrack) OnBind(fn func()) {
	d.onBind = fn
}

func (d *DownTrack) OnRTCP(fn func([]rtcp.Packet)) {
	d.onRTCP = fn
}

func (d *DownTrack) OnREMB(fn func(dt *DownTrack, remb *rtcp.ReceiverEstimatedMaximumBitrate)) {
	d.onREMB = fn
}

func (d *DownTrack) CurrentMaxLossFraction() uint8 {
	return d.lossFraction.get()
}

func (d *DownTrack) AddReceiverReportListener(listener ReceiverReportListener) {
	d.listenerLock.Lock()
	defer d.listenerLock.Unlock()
	d.receiverReportListeners = append(d.receiverReportListeners, listener)
}

func (d *DownTrack) OnAvailableLayersChanged(fn func(dt *DownTrack, layerAdded bool)) {
	d.onAvailableLayersChanged = fn
}

func (d *DownTrack) OnSubscriptionChanged(fn func(dt *DownTrack)) {
	d.onSubscriptionChanged = fn
}

func (d *DownTrack) OnSubscribedLayersChanged(fn func(dt *DownTrack, maxSpatialLayer int32, maxTemporalLayer int32)) {
	d.onSubscribedLayersChanged = fn
}

func (d *DownTrack) OnPacketSent(fn func(dt *DownTrack, size int)) {
	d.onPacketSent = append(d.onPacketSent, fn)
}

func (d *DownTrack) AdjustAllocation(availableChannelCapacity uint64) (isPausing, isResuming bool, bandwidthRequested, optimalBandwidthNeeded uint64) {
	isPausing = false
	isResuming = false
	bandwidthRequested = 0
	optimalBandwidthNeeded = 0

	if d.Kind() == webrtc.RTPCodecTypeAudio || !d.enabled.get() {
		return
	}

	// LK-TODO for temporal preference, traverse the bitrates array the other way
	optimalBandwidthNeeded = uint64(0)
	brs := d.receiver.GetBitrateTemporalCumulative()
	for i := d.maxSpatialLayer.get(); i >= 0; i-- {
		for j := d.maxTemporalLayer.get(); j >= 0; j-- {
			if brs[i][j] == 0 {
				continue
			}
			if optimalBandwidthNeeded == 0 {
				optimalBandwidthNeeded = brs[i][j]
			}
			if brs[i][j] < availableChannelCapacity {
				isResuming = d.bandwidthConstrainedMuted.get()
				bandwidthRequested = brs[i][j]

				d.bandwidthConstrainedMute(false) // just in case it was muted
				d.switchSpatialLayer(int32(i))
				d.switchTemporalLayer(int32(j))

				return
			}
		}
	}

	// no layer fits in the available channel capacity, disable the track
	isPausing = !d.bandwidthConstrainedMuted.get()
	d.bandwidthConstrainedMute(true)
	return
}

func (d *DownTrack) IncreaseAllocation() (bool, uint64, uint64) {
	// LK-TODO-START
	// This is mainly used in probing to try a slightly higher layer.
	// But, if down track is not a simulcast track, then the next
	// available layer (i. e. the only layer of simple track) may boost
	// things by a lot (it could happen in simulcast jumps too).
	// May need to take in a layer increase threshold as an argument
	// (in terms of bps) and increase layer only if the jump is within
	// that threshold.
	// LK-TODO-END
	if d.Kind() == webrtc.RTPCodecTypeAudio || !d.enabled.get() {
		return false, 0, 0
	}

	currentSpatialLayer := d.CurrentSpatialLayer()
	targetSpatialLayer := d.TargetSpatialLayer()

	temporalLayer := d.temporalLayer.get()
	currentTemporalLayer := temporalLayer & 0x0f
	targetTemporalLayer := temporalLayer >> 16

	// if targets are still pending, don't increase
	if targetSpatialLayer != currentSpatialLayer || targetTemporalLayer != currentTemporalLayer {
		return false, 0, 0
	}

	// move to the next available layer
	optimalBandwidthNeeded := uint64(0)
	brs := d.receiver.GetBitrateTemporalCumulative()
	for i := d.maxSpatialLayer.get(); i >= 0; i-- {
		for j := d.maxTemporalLayer.get(); j >= 0; j-- {
			if brs[i][j] == 0 {
				continue
			}
			if optimalBandwidthNeeded == 0 {
				optimalBandwidthNeeded = brs[i][j]
				break
			}
		}

		if optimalBandwidthNeeded != 0 {
			break
		}
	}

	if d.bandwidthConstrainedMuted.get() {
		// try the lowest spatial and temporal layer if available
		// LK-TODO-START
		// note that this will never be zero because we do not track
		// layer 0 in available layers. So, this will need fixing.
		// LK-TODO-END
		if brs[0][0] == 0 {
			// no feed available
			return false, 0, 0
		}

		d.bandwidthConstrainedMute(false)
		d.switchSpatialLayer(int32(0))
		d.switchTemporalLayer(int32(0))
		return true, brs[0][0], optimalBandwidthNeeded
	}

	// try moving temporal layer up in the current spatial layer
	// LK-TODO currentTemporalLayer may be outside available range because of inital value being out of range, fix it
	nextTemporalLayer := currentTemporalLayer + 1
	if nextTemporalLayer <= d.maxTemporalLayer.get() && brs[currentSpatialLayer][nextTemporalLayer] > 0 {
		d.switchTemporalLayer(nextTemporalLayer)
		return true, brs[currentSpatialLayer][nextTemporalLayer], optimalBandwidthNeeded
	}

	// try moving spatial layer up if already at max temporal layer of current spatial layer
	// LK-TODO currentTemporalLayer may be outside available range because of inital value being out of range, fix it
	nextSpatialLayer := currentSpatialLayer + 1
	if nextSpatialLayer <= d.maxSpatialLayer.get() && brs[nextSpatialLayer][0] > 0 {
		d.switchSpatialLayer(nextSpatialLayer)
		d.switchTemporalLayer(0)
		return true, brs[nextSpatialLayer][0], optimalBandwidthNeeded
	}

	return false, 0, 0
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

	srRTP, srNTP := d.receiver.GetSenderReportTime(d.CurrentSpatialLayer())
	if srRTP == 0 {
		return nil
	}

	now := time.Now()
	nowNTP := toNtpTime(now)

	diff := (uint64(now.Sub(ntpTime(srNTP).Time())) * uint64(d.codec.ClockRate)) / uint64(time.Second)
	if diff < 0 {
		diff = 0
	}
	octets, packets := d.getSRStats()
	return &rtcp.SenderReport{
		SSRC:        d.ssrc,
		NTPTime:     uint64(nowNTP),
		RTPTime:     srRTP + uint32(diff),
		PacketCount: packets,
		OctetCount:  octets,
	}
}

func (d *DownTrack) UpdateStats(packetLen uint32) {
	d.octetCount.add(packetLen)
	d.packetCount.add(1)
}

// bandwidthConstrainedMute enables or disables media forwarding dictated by channel bandwidth constraints
func (d *DownTrack) bandwidthConstrainedMute(val bool) {
	if d.bandwidthConstrainedMuted.get() == val {
		return
	}
	d.bandwidthConstrainedMuted.set(val)
	if val {
		d.reSync.set(val)
	}
}

func (d *DownTrack) writeSimpleRTP(extPkt *buffer.ExtPacket) error {
	if d.reSync.get() {
		if d.Kind() == webrtc.RTPCodecTypeVideo {
			if !extPkt.KeyFrame {
				d.lastPli.set(time.Now().UnixNano())
				d.receiver.SendPLI(0)
				d.pktsDropped.add(1)
				return nil
			}
		}
		if d.packetCount.get() > 0 {
			// LK-TODO-START
			// TS offset of 1 is not accurate. It should ideally
			// be driven by packetization of the incoming track.
			// But, this handles track switch on a simple track scenario.
			// It is not a supported use case. So, it is okay. But, if
			// we support switch track (i. e. same down track switches
			// to a different up track), this needs to be looked at.
			// LK-TODO-END
			d.munger.UpdateSnTsOffsets(extPkt, 1, 1)
		} else {
			d.munger.SetLastSnTs(extPkt)
			if d.vp8Munger != nil {
				d.vp8Munger.SetLast(extPkt)
			}
		}
		d.lastSSRC.set(extPkt.Packet.SSRC)
		d.reSync.set(false)
	}

	newSN, newTS, ordering, err := d.munger.UpdateAndGetSnTs(extPkt)
	if err != nil {
		if err == ErrPaddingOnlyPacket || err == ErrDuplicatePacket || err == ErrOutOfOrderSequenceNumberCacheMiss {
			return nil
		}

		d.pktsDropped.add(1)
		return err
	}

	payload := extPkt.Packet.Payload

	var translatedVP8 *buffer.VP8
	if d.vp8Munger != nil && len(payload) > 0 {
		// LK-TODO-START
		// Errors below do not update sequence number. That is a problem if the stream
		// is expected to continue past the error. The translation should not error out.
		// But, if there is a legitimate error case and the stream can continue beyond
		// that, the sequence numbers should be updated to ensure that subsequent packet
		// translations works fine and produce proper translated sequence numbers.
		// LK-TODO-END
		translatedVP8, err = d.vp8Munger.UpdateAndGet(extPkt, ordering, d.temporalLayer.get()>>16)
		if err != nil {
			if err == ErrFilteredVP8TemporalLayer || err == ErrOutOfOrderVP8PictureIdCacheMiss {
				if err == ErrFilteredVP8TemporalLayer {
					// filtered temporal layer, update sequence number offset to prevent holes
					d.munger.PacketDropped(extPkt)
				}
				d.pktsDropped.add(1)
				return nil
			}

			d.pktsDropped.add(1)
			return err
		}

		incomingVP8, ok := extPkt.Payload.(buffer.VP8)
		if !ok {
			d.pktsDropped.add(1)
			return ErrNotVP8
		}

		payload, err = d.translateVP8Packet(&extPkt.Packet, &incomingVP8, translatedVP8, ordering != SequenceNumberOrderingOutOfOrder)
		if err != nil {
			d.pktsDropped.add(1)
			return err
		}
	}

	if d.sequencer != nil {
		meta := d.sequencer.push(extPkt.Packet.SequenceNumber, newSN, newTS, 0, extPkt.Head)
		if meta != nil && translatedVP8 != nil {
			meta.packVP8(translatedVP8)
		}
	}

	// LK-TODO maybe include RTP header size also
	d.UpdateStats(uint32(len(payload)))

	hdr := extPkt.Packet.Header
	hdr.PayloadType = d.payloadType
	hdr.Timestamp = newTS
	hdr.SequenceNumber = newSN
	hdr.SSRC = d.ssrc

	err = d.writeRTPHeaderExtensions(&hdr)
	if err != nil {
		return err
	}

	_, err = d.writeStream.WriteRTP(&hdr, payload)
	if err == nil {
		for _, f := range d.onPacketSent {
			f(d, hdr.MarshalSize()+len(payload))
		}
	}

	return err
}

func (d *DownTrack) writeSimulcastRTP(extPkt *buffer.ExtPacket, layer int32) error {
	tsl := d.TargetSpatialLayer()
	csl := d.CurrentSpatialLayer()
	if tsl == layer && csl != tsl {
		if extPkt.KeyFrame {
			d.currentSpatialLayer.set(layer)
			csl = layer
		} else {
			d.lastPli.set(time.Now().UnixNano())
			d.receiver.SendPLI(layer)
		}
	}

	if tsl < csl && tsl < d.MaxSpatialLayer() {
		//
		// If target layer is lower than both the current and
		// maximum subscribed layer, it is due to bandwidth
		// constraints that the target layer has been switched down.
		// Continuing to send higher layer will only exacerbate the
		// situation by putting more stress on the channel. So, drop it.
		//
		// In the other direction, it is okay to keep forwarding till
		// switch point to get a smoother stream till the higher
		// layer key frame arrives.
		//
		// Note that in the case of client subscription layer restriction
		// coinciding with server restriction due to bandwidth limitation,
		// this will take client subscription as the winning vote and
		// continue to stream current spatial layer till switch point.
		// That could lead to congesting the channel.
		// LK-TODO: Improve the above case, i. e. distinguish server
		// applied restriction from client requested restriction.
		//
		d.pktsDropped.add(1)
		return nil
	}

	if csl != layer {
		d.pktsDropped.add(1)
		return nil
	}

	// Check if packet SSRC is different from before
	// if true, the video source changed
	lastSSRC := d.lastSSRC.get()
	reSync := d.reSync.get()
	if lastSSRC != extPkt.Packet.SSRC || reSync {
		// Wait for a keyframe to sync new source
		if reSync && !extPkt.KeyFrame {
			// Packet is not a keyframe, discard it
			// LK-TODO-START
			// Some of this happens is happening in sfu.Receiver also.
			// If performance is not a concern, sfu.Receiver should send
			// all the packets to down tracks and down track should be
			// the only one deciding whether to switch/forward/drop
			// LK-TODO-END
			d.receiver.SendPLI(layer)
			d.lastPli.set(time.Now().UnixNano())
			d.pktsDropped.add(1)
			return nil
		}

		if reSync && d.simulcast.lTSCalc.get() != 0 {
			d.simulcast.lTSCalc.set(extPkt.Arrival)
		}

		d.lastSSRC.set(extPkt.Packet.SSRC)
		d.reSync.set(false)
	}

	// LK-TODO-START
	// The below offset calculation is not technically correct.
	// Timestamps based on the system time of an intermediate box like
	// SFU is not going to be accurate. Packets arrival/processing
	// are subject to vagaries of network delays, SFU processing etc.
	// But, the correct way is a lot harder. Will have to
	// look at RTCP SR to get timestamps and figure out alignment
	// of layers and use that during layer switch. That can
	// get tricky. Given the complexity of that approach, maybe
	// this is just fine till it is not :-).
	// LK-TODO-END

	// Compute how much time passed between the old RTP extPkt
	// and the current packet, and fix timestamp on source change
	lTSCalc := d.simulcast.lTSCalc.get()
	if lTSCalc != 0 && lastSSRC != extPkt.Packet.SSRC {
		tDiff := (extPkt.Arrival - lTSCalc) / 1e6
		// LK-TODO-START
		// this is assuming clock rate of 90000.
		// Should be fine for video, but ideally should use ClockRate of the track
		// LK-TODO-END
		td := uint32((tDiff * 90) / 1000)
		if td == 0 {
			td = 1
		}
		d.munger.UpdateSnTsOffsets(extPkt, 1, td)
		if d.vp8Munger != nil {
			d.vp8Munger.UpdateOffsets(extPkt)
		}
	} else if lTSCalc == 0 {
		d.munger.SetLastSnTs(extPkt)
		if d.vp8Munger != nil {
			d.vp8Munger.SetLast(extPkt)
		}
	}

	newSN, newTS, ordering, err := d.munger.UpdateAndGetSnTs(extPkt)
	if err != nil {
		if err == ErrPaddingOnlyPacket || err == ErrDuplicatePacket || err == ErrOutOfOrderSequenceNumberCacheMiss {
			return nil
		}

		d.pktsDropped.add(1)
		return err
	}

	payload := extPkt.Packet.Payload

	var translatedVP8 *buffer.VP8
	if d.vp8Munger != nil && len(payload) > 0 {
		// LK-TODO-START
		// Errors below do not update sequence number. That is a problem if the stream
		// is expected to continue past the error. The translation should not error out.
		// But, if there is a legitimate error case and the stream can continue beyond
		// that, the sequence numbers should be updated to ensure that subsequent packet
		// translations works fine and produce proper translated sequence numbers.
		// LK-TODO-END
		translatedVP8, err = d.vp8Munger.UpdateAndGet(extPkt, ordering, d.temporalLayer.get()>>16)
		if err != nil {
			if err == ErrFilteredVP8TemporalLayer || err == ErrOutOfOrderVP8PictureIdCacheMiss {
				if err == ErrFilteredVP8TemporalLayer {
					// filtered temporal layer, update sequence number offset to prevent holes
					d.munger.PacketDropped(extPkt)
				}
				d.pktsDropped.add(1)
				return nil
			}

			d.pktsDropped.add(1)
			return err
		}

		incomingVP8, ok := extPkt.Payload.(buffer.VP8)
		if !ok {
			d.pktsDropped.add(1)
			return ErrNotVP8
		}

		payload, err = d.translateVP8Packet(&extPkt.Packet, &incomingVP8, translatedVP8, ordering != SequenceNumberOrderingOutOfOrder)
		if err != nil {
			d.pktsDropped.add(1)
			return err
		}
	}

	if d.sequencer != nil {
		meta := d.sequencer.push(extPkt.Packet.SequenceNumber, newSN, newTS, uint8(csl), extPkt.Head)
		if meta != nil && translatedVP8 != nil {
			meta.packVP8(translatedVP8)
		}
	}

	// LK-TODO - maybe include RTP header?
	d.UpdateStats(uint32(len(payload)))

	// Update base
	d.simulcast.lTSCalc.set(extPkt.Arrival)

	// Update extPkt headers
	hdr := extPkt.Packet.Header
	hdr.SequenceNumber = newSN
	hdr.Timestamp = newTS
	hdr.SSRC = d.ssrc
	hdr.PayloadType = d.payloadType

	err = d.writeRTPHeaderExtensions(&hdr)
	if err != nil {
		return err
	}

	_, err = d.writeStream.WriteRTP(&hdr, payload)
	if err == nil {
		for _, f := range d.onPacketSent {
			f(d, hdr.MarshalSize()+len(payload))
		}
	}

	return err
}

func (d *DownTrack) writeBlankFrameRTP() error {
	// don't send if nothing has been sent
	if d.packetCount.get() == 0 {
		return nil
	}

	// LK-TODO: Support other video codecs
	if d.Kind() == webrtc.RTPCodecTypeAudio || (d.mime != "video/vp8" && d.mime != "video/h264") {
		return nil
	}

	// send a number of blank frames just in case there is loss.
	// Intentionally ignoring check for mute or bandwidth constrained mute
	// as this is used to clear client side buffer.
	for i := 0; i < RTPBlankFramesMax; {
		frameEndNeeded := false
		if !d.munger.IsOnFrameBoundary() {
			frameEndNeeded = true
		}

		sn, ts, err := d.munger.UpdateAndGetPaddingSnTs(frameEndNeeded)
		if err != nil {
			return err
		}

		adjustedTs := ts + uint32(i+1)*(d.codec.ClockRate/30) // assume 30 fps
		if frameEndNeeded {
			adjustedTs = ts
		}
		hdr := rtp.Header{
			Version:        2,
			Padding:        false,
			Marker:         true,
			PayloadType:    d.payloadType,
			SequenceNumber: sn,
			Timestamp:      adjustedTs,
			SSRC:           d.ssrc,
			CSRC:           []uint32{},
		}

		err = d.writeRTPHeaderExtensions(&hdr)
		if err != nil {
			return err
		}

		switch d.mime {
		case "video/vp8":
			err = d.writeVP8BlankFrame(&hdr, frameEndNeeded)
		case "video/h264":
			err = d.writeH264BlankFrame(&hdr, frameEndNeeded)
		default:
			return nil
		}

		if !frameEndNeeded {
			i++
		}

	}

	return nil
}

func (d *DownTrack) writeVP8BlankFrame(hdr *rtp.Header, frameEndNeeded bool) error {
	blankVP8, err := d.vp8Munger.UpdateAndGetPadding(!frameEndNeeded)
	if err != nil {
		return err
	}

	// 1x1 key frame
	// Used even when closing out a previous frame. Looks like receivers
	// do not care about content (it will probably end up being an undecodable
	// frame, but that should be okay as there are key frames following)
	payload := make([]byte, blankVP8.HeaderSize+len(VP8KeyFrame1x1))
	vp8Header := payload[:blankVP8.HeaderSize]
	err = blankVP8.MarshalTo(vp8Header)
	if err != nil {
		return err
	}

	copy(payload[blankVP8.HeaderSize:], VP8KeyFrame1x1)

	_, err = d.writeStream.WriteRTP(hdr, payload)
	return err
}

func (d *DownTrack) writeH264BlankFrame(hdr *rtp.Header, frameEndNeeded bool) error {
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
	return err
}

func (d *DownTrack) handleRTCP(bytes []byte) {
	// LK-TODO - should probably handle RTCP even if muted
	enabled := d.enabled.get()
	if !enabled && d.onRTCP == nil {
		return
	}

	pkts, err := rtcp.Unmarshal(bytes)
	if err != nil {
		Logger.Error(err, "Unmarshal rtcp receiver packets err")
	}

	if d.onRTCP != nil {
		d.onRTCP(pkts)
		if !enabled {
			return
		}
	}

	pliOnce := true

	var (
		maxRatePacketLoss uint8
	)

	ssrc := d.lastSSRC.get()
	if ssrc == 0 {
		return
	}

	sendPliOnce := func() {
		if pliOnce {
			d.lastPli.set(time.Now().UnixNano())
			d.receiver.SendPLI(d.TargetSpatialLayer())
			pliOnce = false
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
				if maxRatePacketLoss == 0 || maxRatePacketLoss < r.FractionLost {
					maxRatePacketLoss = r.FractionLost
				}
			}
			d.lossFraction.set(maxRatePacketLoss)
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
		}
	}
}

func (d *DownTrack) retransmitPackets(nackedPackets []packetMeta) {
	src := packetFactory.Get().(*[]byte)
	defer packetFactory.Put(src)
	for _, meta := range nackedPackets {
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

		err = d.maybeTranslateVP8(&pkt, meta)
		if err != nil {
			Logger.Error(err, "translating VP8 packet err")
			continue
		}

		err = d.writeRTPHeaderExtensions(&pkt.Header)
		if err != nil {
			Logger.Error(err, "writing rtp header extensions err")
			continue
		}

		if _, err = d.writeStream.WriteRTP(&pkt.Header, pkt.Payload); err != nil {
			Logger.Error(err, "Writing rtx packet err")
		} else {
			d.UpdateStats(uint32(n))
		}
	}
}

func (d *DownTrack) getSRStats() (octets, packets uint32) {
	return d.octetCount.get(), d.packetCount.get()
}

func (d *DownTrack) translateVP8Packet(pkt *rtp.Packet, incomingVP8 *buffer.VP8, translatedVP8 *buffer.VP8, adjustTemporal bool) (buf []byte, err error) {
	if adjustTemporal {
		temporalLayer := d.temporalLayer.get()
		currentLayer := uint16(temporalLayer)
		currentTargetLayer := uint16(temporalLayer >> 16)
		// catch up temporal layer if necessary
		if currentTargetLayer != currentLayer {
			if incomingVP8.TIDPresent == 1 && incomingVP8.TID <= uint8(currentTargetLayer) {
				d.temporalLayer.set(int32(currentTargetLayer)<<16 | int32(currentTargetLayer))
			}
		}
	}

	buf = *d.payload
	buf = buf[:len(pkt.Payload)+translatedVP8.HeaderSize-incomingVP8.HeaderSize]

	srcPayload := pkt.Payload[incomingVP8.HeaderSize:]
	dstPayload := buf[translatedVP8.HeaderSize:]
	copy(dstPayload, srcPayload)

	hdr := buf[:translatedVP8.HeaderSize]
	err = translatedVP8.MarshalTo(hdr)

	return
}

func (d *DownTrack) DebugInfo() map[string]interface{} {
	mungerParams := d.munger.getParams()
	stats := map[string]interface{}{
		"HighestIncomingSN": mungerParams.highestIncomingSN,
		"LastSN":            mungerParams.lastSN,
		"SNOffset":          mungerParams.snOffset,
		"LastTS":            mungerParams.lastTS,
		"TSOffset":          mungerParams.tsOffset,
		"LastMarker":        mungerParams.lastMarker,
		"LastRTP":           d.lastRTP.get(),
		"LastPli":           d.lastPli.get(),
		"PacketsDropped":    d.pktsDropped.get(),
		"PacketsMuted":      d.pktsMuted.get(),
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
		"Enabled":             d.enabled.get(),
		"Resync":              d.reSync.get(),
		"CurrentSpatialLayer": d.CurrentSpatialLayer(),
		"Stats":               stats,
	}
}

//
// munger
//
type MungerParams struct {
	highestIncomingSN uint16
	lastSN            uint16
	snOffset          uint16
	lastTS            uint32
	tsOffset          uint32
	lastMarker        bool

	missingSNs map[uint16]uint16
}

type Munger struct {
	lock sync.RWMutex

	MungerParams
}

func NewMunger() *Munger {
	return &Munger{MungerParams: MungerParams{
		missingSNs: make(map[uint16]uint16, 10),
	}}
}

func (m *Munger) getParams() MungerParams {
	m.lock.RLock()
	defer m.lock.RUnlock()

	return MungerParams{
		highestIncomingSN: m.highestIncomingSN,
		lastSN:            m.lastSN,
		snOffset:          m.snOffset,
		lastTS:            m.lastTS,
		tsOffset:          m.tsOffset,
		lastMarker:        m.lastMarker,
	}
}

func (m *Munger) SetLastSnTs(extPkt *buffer.ExtPacket) {
	m.lock.Lock()
	defer m.lock.Unlock()

	m.highestIncomingSN = extPkt.Packet.SequenceNumber - 1
	m.lastSN = extPkt.Packet.SequenceNumber
	m.lastTS = extPkt.Packet.Timestamp
}

func (m *Munger) UpdateSnTsOffsets(extPkt *buffer.ExtPacket, snAdjust uint16, tsAdjust uint32) {
	m.lock.Lock()
	defer m.lock.Unlock()

	m.highestIncomingSN = extPkt.Packet.SequenceNumber - 1
	m.snOffset = extPkt.Packet.SequenceNumber - m.lastSN - snAdjust
	m.tsOffset = extPkt.Packet.Timestamp - m.lastTS - tsAdjust

	// clear incoming missing sequence numbers on layer switch
	m.missingSNs = make(map[uint16]uint16, 10)
}

func (m *Munger) PacketDropped(extPkt *buffer.ExtPacket) {
	m.lock.Lock()
	defer m.lock.Unlock()

	if !extPkt.Head {
		return
	}

	m.highestIncomingSN = extPkt.Packet.SequenceNumber
	m.snOffset += 1
}

func (m *Munger) UpdateAndGetSnTs(extPkt *buffer.ExtPacket) (uint16, uint32, SequenceNumberOrdering, error) {
	m.lock.Lock()
	defer m.lock.Unlock()

	// if out-of-order, look up missing sequence number cache
	if !extPkt.Head {
		snOffset, ok := m.missingSNs[extPkt.Packet.SequenceNumber]
		if !ok {
			return 0, 0, SequenceNumberOrderingOutOfOrder, ErrOutOfOrderSequenceNumberCacheMiss
		}

		delete(m.missingSNs, extPkt.Packet.SequenceNumber)
		return extPkt.Packet.SequenceNumber - snOffset, extPkt.Packet.Timestamp - m.tsOffset, SequenceNumberOrderingOutOfOrder, nil
	}

	ordering := SequenceNumberOrderingContiguous

	// if there are gaps, record it in missing sequence number cache
	diff := extPkt.Packet.SequenceNumber - m.highestIncomingSN
	if diff > 1 {
		ordering = SequenceNumberOrderingGap
		var lossStartSN, lossEndSN int
		lossStartSN = int(m.highestIncomingSN) + 1
		if extPkt.Packet.SequenceNumber > m.highestIncomingSN {
			lossEndSN = int(extPkt.Packet.SequenceNumber) - 1
		} else {
			lossEndSN = int(extPkt.Packet.SequenceNumber) - 1 + buffer.MaxSN
		}
		for lostSN := lossStartSN; lostSN <= lossEndSN; lostSN++ {
			m.missingSNs[uint16(lostSN&0xffff)] = m.snOffset
		}
	} else {
		// can get duplicate packet due to FEC
		if diff == 0 {
			return 0, 0, SequenceNumberOrderingUnknown, ErrDuplicatePacket
		}

		// if padding only packet, can be dropped and sequence number adjusted
		// as it is contiguous and in order. That means this is the highest
		// incoming sequence number and it is a good point to adjust
		// sequence number offset.
		if len(extPkt.Packet.Payload) == 0 {
			m.highestIncomingSN = extPkt.Packet.SequenceNumber
			m.snOffset += 1
			return 0, 0, SequenceNumberOrderingContiguous, ErrPaddingOnlyPacket
		}
	}

	// in-order incoming packet, may or may not be contiguous.
	// In the case of loss (i. e. incoming sequence number is not contiguous),
	// forward even if it is a padding only packet. With temporal scalability,
	// it is unclear if the current packet should be dropped if it is not
	// contiguous. Hence forward anything that is not contiguous.
	// Reference: http://www.rtcbits.com/2017/04/howto-implement-temporal-scalability.html
	mungedSN := extPkt.Packet.SequenceNumber - m.snOffset
	mungedTS := extPkt.Packet.Timestamp - m.tsOffset

	m.highestIncomingSN = extPkt.Packet.SequenceNumber
	m.lastSN = mungedSN
	m.lastTS = mungedTS
	m.lastMarker = extPkt.Packet.Marker

	return mungedSN, mungedTS, ordering, nil
}

func (m *Munger) UpdateAndGetPaddingSnTs(forceMarker bool) (uint16, uint32, error) {
	m.lock.Lock()
	defer m.lock.Unlock()

	if !m.lastMarker && !forceMarker {
		return 0, 0, ErrPaddingNotOnFrameBoundary
	}

	sn := m.lastSN + 1
	ts := m.lastTS

	m.lastSN = sn
	m.snOffset -= 1

	if forceMarker {
		m.lastMarker = true
	}

	return sn, ts, nil
}

func (m *Munger) IsOnFrameBoundary() bool {
	m.lock.RLock()
	defer m.lock.RUnlock()

	return m.lastMarker
}

//
// VP8 munger
//
type VP8MungerParams struct {
	pictureIdWrapHandler VP8PictureIdWrapHandler
	extLastPictureId     int32
	pictureIdOffset      int32
	pictureIdUsed        int
	lastTl0PicIdx        uint8
	tl0PicIdxOffset      uint8
	tl0PicIdxUsed        int
	tidUsed              int
	lastKeyIdx           uint8
	keyIdxOffset         uint8
	keyIdxUsed           int

	missingPictureIds    *orderedmap.OrderedMap
	lastDroppedPictureId int32
}

type VP8Munger struct {
	lock sync.Mutex

	VP8MungerParams
}

func NewVP8Munger() *VP8Munger {
	return &VP8Munger{VP8MungerParams: VP8MungerParams{
		missingPictureIds:    orderedmap.NewOrderedMap(),
		lastDroppedPictureId: -1,
	}}
}

func (v *VP8Munger) SetLast(extPkt *buffer.ExtPacket) {
	v.lock.Lock()
	defer v.lock.Unlock()

	vp8, ok := extPkt.Payload.(buffer.VP8)
	if !ok {
		return
	}

	v.pictureIdUsed = vp8.PictureIDPresent
	if v.pictureIdUsed == 1 {
		v.pictureIdWrapHandler.Init(int32(vp8.PictureID)-1, vp8.MBit)
		v.extLastPictureId = int32(vp8.PictureID)
	}

	v.tl0PicIdxUsed = vp8.TL0PICIDXPresent
	if v.tl0PicIdxUsed == 1 {
		v.lastTl0PicIdx = vp8.TL0PICIDX
	}

	v.tidUsed = vp8.TIDPresent

	v.keyIdxUsed = vp8.KEYIDXPresent
	if v.keyIdxUsed == 1 {
		v.lastKeyIdx = vp8.KEYIDX
	}

	v.lastDroppedPictureId = -1
}

func (v *VP8Munger) UpdateOffsets(extPkt *buffer.ExtPacket) {
	v.lock.Lock()
	defer v.lock.Unlock()

	vp8, ok := extPkt.Payload.(buffer.VP8)
	if !ok {
		return
	}

	if v.pictureIdUsed == 1 {
		v.pictureIdWrapHandler.Init(int32(vp8.PictureID)-1, vp8.MBit)
		v.pictureIdOffset = int32(vp8.PictureID) - v.extLastPictureId - 1
	}

	if v.tl0PicIdxUsed == 1 {
		v.tl0PicIdxOffset = vp8.TL0PICIDX - v.lastTl0PicIdx - 1
	}

	if v.keyIdxUsed == 1 {
		v.keyIdxOffset = (vp8.KEYIDX - v.lastKeyIdx - 1) & 0x1f
	}

	// clear missing picture ids on layer switch
	v.missingPictureIds = orderedmap.NewOrderedMap()

	v.lastDroppedPictureId = -1
}

func (v *VP8Munger) UpdateAndGet(extPkt *buffer.ExtPacket, ordering SequenceNumberOrdering, maxTemporalLayer int32) (*buffer.VP8, error) {
	v.lock.Lock()
	defer v.lock.Unlock()

	vp8, ok := extPkt.Payload.(buffer.VP8)
	if !ok {
		return nil, ErrNotVP8
	}

	extPictureId, newer := v.pictureIdWrapHandler.Unwrap(vp8.PictureID, vp8.MBit)

	// if out-of-order, look up missing picture id cache
	if !newer {
		value, ok := v.missingPictureIds.Get(extPictureId)
		if !ok {
			return nil, ErrOutOfOrderVP8PictureIdCacheMiss
		}
		pictureIdOffset := value.(int32)

		// the out-of-order picture id cannot be deleted from the cache
		// as there could more than one packet in a picture and more
		// than one packet of a picture could come out-of-order.
		// To prevent picture id cache from growing, it is truncated
		// when it reaches a certain size.

		mungedPictureId := uint16((extPictureId - pictureIdOffset) & 0x7fff)
		vp8Packet := &buffer.VP8{
			FirstByte:        vp8.FirstByte,
			PictureIDPresent: vp8.PictureIDPresent,
			PictureID:        mungedPictureId,
			MBit:             mungedPictureId > 127,
			TL0PICIDXPresent: vp8.TL0PICIDXPresent,
			TL0PICIDX:        vp8.TL0PICIDX - v.tl0PicIdxOffset,
			TIDPresent:       vp8.TIDPresent,
			TID:              vp8.TID,
			Y:                vp8.Y,
			KEYIDXPresent:    vp8.KEYIDXPresent,
			KEYIDX:           vp8.KEYIDX - v.keyIdxOffset,
			IsKeyFrame:       vp8.IsKeyFrame,
			HeaderSize:       vp8.HeaderSize + buffer.VP8PictureIdSizeDiff(mungedPictureId > 127, vp8.MBit),
		}
		return vp8Packet, nil
	}

	prevMaxPictureId := v.pictureIdWrapHandler.MaxPictureId()
	v.pictureIdWrapHandler.UpdateMaxPictureId(extPictureId, vp8.MBit)

	// if there is a gap in sequence number, record possible pictures that
	// the missing packets can belong to in missing picture id cache.
	// The missing picture cache should contain the previous picture id
	// and the current picture id and all the intervening pictures.
	// This is to handle a scenario as follows
	//   o Packet 10 -> Picture ID 10
	//   o Packet 11 -> missing
	//   o Packet 12 -> Picture ID 11
	// In this case, Packet 11 could belong to either Picture ID 10 (last packet of that picture)
	// or Picture ID 11 (first packet of the current picture). Although in this simple case,
	// it is possible to deduce that (for example by looking at previous packet's RTP marker
	// and check if that was the last packet of Picture 10), it could get complicated when
	// the gap is larger.
	if ordering == SequenceNumberOrderingGap {
		// can drop packet if it belongs to the last dropped picture.
		// Example:
		//   o Packet 10 - Picture 11 - TID that should be dropped
		//   o Packet 11 - missing
		//   o Packet 12 - Picture 11 - will be reported as GAP, but belongs to a picture that was dropped and hence can be dropped
		// If Packet 11 comes around, it will be reported as OUT_OF_ORDER, but the missing
		// picture id cache will not have an entry and hence will be dropped.
		if extPictureId == v.lastDroppedPictureId {
			return nil, ErrFilteredVP8TemporalLayer
		} else {
			for lostPictureId := prevMaxPictureId; lostPictureId <= extPictureId; lostPictureId++ {
				v.missingPictureIds.Set(lostPictureId, v.pictureIdOffset)
			}

			// trim cache if necessary
			for v.missingPictureIds.Len() > 50 {
				el := v.missingPictureIds.Front()
				v.missingPictureIds.Delete(el.Key)
			}
		}
	} else {
		if vp8.TIDPresent == 1 && vp8.TID > uint8(maxTemporalLayer) {
			// adjust only once per picture as a picture could have multiple packets
			if vp8.PictureIDPresent == 1 && prevMaxPictureId != extPictureId {
				v.lastDroppedPictureId = extPictureId
				v.pictureIdOffset += 1
			}
			return nil, ErrFilteredVP8TemporalLayer
		}
	}

	// in-order incoming sequence number, may or may not be contiguous.
	// In the case of loss (i. e. incoming sequence number is not contiguous),
	// forward even if it is a filtered layer. With temporal scalability,
	// it is unclear if the current packet should be dropped if it is not
	// contiguous. Hence forward anything that is not contiguous.
	// Reference: http://www.rtcbits.com/2017/04/howto-implement-temporal-scalability.html
	extMungedPictureId := extPictureId - v.pictureIdOffset
	mungedPictureId := uint16(extMungedPictureId & 0x7fff)
	mungedTl0PicIdx := vp8.TL0PICIDX - v.tl0PicIdxOffset
	mungedKeyIdx := (vp8.KEYIDX - v.keyIdxOffset) & 0x1f

	v.extLastPictureId = extMungedPictureId
	v.lastTl0PicIdx = mungedTl0PicIdx
	v.lastKeyIdx = mungedKeyIdx

	vp8Packet := &buffer.VP8{
		FirstByte:        vp8.FirstByte,
		PictureIDPresent: vp8.PictureIDPresent,
		PictureID:        mungedPictureId,
		MBit:             mungedPictureId > 127,
		TL0PICIDXPresent: vp8.TL0PICIDXPresent,
		TL0PICIDX:        mungedTl0PicIdx,
		TIDPresent:       vp8.TIDPresent,
		TID:              vp8.TID,
		Y:                vp8.Y,
		KEYIDXPresent:    vp8.KEYIDXPresent,
		KEYIDX:           mungedKeyIdx,
		IsKeyFrame:       vp8.IsKeyFrame,
		HeaderSize:       vp8.HeaderSize + buffer.VP8PictureIdSizeDiff(mungedPictureId > 127, vp8.MBit),
	}
	return vp8Packet, nil
}

func (v *VP8Munger) UpdateAndGetPadding(newPicture bool) (*buffer.VP8, error) {
	v.lock.Lock()
	defer v.lock.Unlock()

	offset := 0
	if newPicture {
		offset = 1
	}

	headerSize := 1
	if (v.pictureIdUsed + v.tl0PicIdxUsed + v.tidUsed + v.keyIdxUsed) != 0 {
		headerSize += 1
	}

	extPictureId := v.extLastPictureId
	if v.pictureIdUsed == 1 {
		extPictureId = v.extLastPictureId + int32(offset)
		v.extLastPictureId = extPictureId
		v.pictureIdOffset -= int32(offset)
		if (extPictureId & 0x7fff) > 127 {
			headerSize += 2
		} else {
			headerSize += 1
		}
	}
	pictureId := uint16(extPictureId & 0x7fff)

	tl0PicIdx := uint8(0)
	if v.tl0PicIdxUsed == 1 {
		tl0PicIdx = v.lastTl0PicIdx + uint8(offset)
		v.lastTl0PicIdx = tl0PicIdx
		v.tl0PicIdxOffset -= uint8(offset)
		headerSize += 1
	}

	if (v.tidUsed + v.keyIdxUsed) != 0 {
		headerSize += 1
	}

	keyIdx := uint8(0)
	if v.keyIdxUsed == 1 {
		keyIdx = (v.lastKeyIdx + uint8(offset)) & 0x1f
		v.lastKeyIdx = keyIdx
		v.keyIdxOffset -= uint8(offset)
	}

	vp8Packet := &buffer.VP8{
		FirstByte:        0x10, // partition 0, start of VP8 Partition, reference frame
		PictureIDPresent: v.pictureIdUsed,
		PictureID:        pictureId,
		MBit:             pictureId > 127,
		TL0PICIDXPresent: v.tl0PicIdxUsed,
		TL0PICIDX:        tl0PicIdx,
		TIDPresent:       v.tidUsed,
		TID:              0,
		Y:                1,
		KEYIDXPresent:    v.keyIdxUsed,
		KEYIDX:           keyIdx,
		IsKeyFrame:       true,
		HeaderSize:       headerSize,
	}
	return vp8Packet, nil
}

func isWrapping7Bit(val1 int32, val2 int32) bool {
	return val2 < val1 && (val1-val2) > (1<<6)
}

func isWrapping15Bit(val1 int32, val2 int32) bool {
	return val2 < val1 && (val1-val2) > (1<<14)
}

type VP8PictureIdWrapHandler struct {
	maxPictureId int32
	maxMBit      bool
	totalWrap    int32
	lastWrap     int32
}

func (v *VP8PictureIdWrapHandler) Init(extPictureId int32, mBit bool) {
	v.maxPictureId = extPictureId
	v.maxMBit = mBit
	v.totalWrap = 0
	v.lastWrap = 0
}

func (v *VP8PictureIdWrapHandler) MaxPictureId() int32 {
	return v.maxPictureId
}

// unwrap picture id and update the maxPictureId. return unwrapped value, and whether picture id is newer
func (v *VP8PictureIdWrapHandler) Unwrap(pictureId uint16, mBit bool) (int32, bool) {
	//
	// VP8 Picture ID is specified very flexibly.
	//
	// Reference: https://datatracker.ietf.org/doc/html/draft-ietf-payload-vp8
	//
	// Quoting from the RFC
	// ----------------------------
	// PictureID:  7 or 15 bits (shown left and right, respectively, in
	//    Figure 2) not including the M bit.  This is a running index of
	//    the frames, which MAY start at a random value, MUST increase by
	//    1 for each subsequent frame, and MUST wrap to 0 after reaching
	//    the maximum ID (all bits set).  The 7 or 15 bits of the
	//    PictureID go from most significant to least significant,
	//    beginning with the first bit after the M bit.  The sender
	//    chooses a 7 or 15 bit index and sets the M bit accordingly.
	//    The receiver MUST NOT assume that the number of bits in
	//    PictureID stay the same through the session.  Having sent a
	//    7-bit PictureID with all bits set to 1, the sender may either
	//    wrap the PictureID to 0, or extend to 15 bits and continue
	//    incrementing
	// ----------------------------
	//
	// While in practice, senders may not switch between modes indiscriminately,
	// it is possible that small picture ids are sent in 7 bits and then switch
	// to 15 bits. But, to ensure correctness, this code keeps track of how much
	// quantity has wrapped and uses that to figure out if the incoming picture id
	// is newer OR out-of-order.
	//
	maxPictureId := v.maxPictureId
	// maxPictureId can be -1 at the start
	if maxPictureId > 0 {
		if v.maxMBit {
			maxPictureId = v.maxPictureId & 0x7fff
		} else {
			maxPictureId = v.maxPictureId & 0x7f
		}
	}

	var newPictureId int32
	if mBit {
		newPictureId = int32(pictureId & 0x7fff)
	} else {
		newPictureId = int32(pictureId & 0x7f)
	}

	//
	// if the new picture id is too far ahead of max, i.e. more than half of last wrap,
	// it is out-of-order, unwrap backwards
	//
	if v.totalWrap > 0 {
		if (v.maxPictureId + (v.lastWrap >> 1)) < (newPictureId + v.totalWrap) {
			return newPictureId + v.totalWrap - v.lastWrap, false
		}
	}

	//
	// check for wrap around based on mode of previous picture id.
	// There are three cases here
	//   1. Wrapping from 15-bit -> 8-bit (32767 -> 0)
	//   2. Wrapping from 15-bit -> 15-bit (32767 -> 0)
	//   3. Wrapping from 8-bit -> 8-bit (127 -> 0)
	// In all cases, looking at the mode of previous picture id will
	// ensure that we are calculating the rap properly.
	//
	wrap := int32(0)
	if v.maxMBit {
		if isWrapping15Bit(maxPictureId, newPictureId) {
			wrap = 1 << 15
		}
	} else {
		if isWrapping7Bit(maxPictureId, newPictureId) {
			wrap = 1 << 7
		}
	}

	v.totalWrap += wrap
	if wrap != 0 {
		v.lastWrap = wrap
	}
	newPictureId += v.totalWrap

	// >= in the below check as there could be multiple packets per picture
	return newPictureId, newPictureId >= v.maxPictureId
}

func (v *VP8PictureIdWrapHandler) UpdateMaxPictureId(extPictureId int32, mBit bool) {
	v.maxPictureId = extPictureId
	v.maxMBit = mBit
}

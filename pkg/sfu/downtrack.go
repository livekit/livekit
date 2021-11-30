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
	UptrackLayersChange(availableLayers []uint16)
	WriteRTP(p *buffer.ExtPacket, layer int32) error
	Close()
	// ID is the globally unique identifier for this Track.
	ID() string
	SetTrackType(isSimulcast bool)
	Codec() webrtc.RTPCodecCapability
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

	InvalidSpatialLayer  = -1
	InvalidTemporalLayer = -1
)

type SequenceNumberOrdering int

const (
	SequenceNumberOrderingContiguous SequenceNumberOrdering = iota
	SequenceNumberOrderingOutOfOrder
	SequenceNumberOrderingGap
	SequenceNumberOrderingDuplicate
)

type ForwardingStatus int

const (
	ForwardingStatusOff ForwardingStatus = iota
	ForwardingStatusPartial
	ForwardingStatusOptimal
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
	ErrNoRequiredBuff                    = errors.New("buff size if less than required")
)

var (
	VP8KeyFrame1x1 = []byte{0x10, 0x02, 0x00, 0x9d, 0x01, 0x2a, 0x01, 0x00, 0x01, 0x00, 0x0b, 0xc7, 0x08, 0x85, 0x85, 0x88, 0x85, 0x84, 0x88, 0x3f, 0x82, 0x00, 0x0c, 0x0d, 0x60, 0x00, 0xfe, 0xe6, 0xb5, 0x00}

	H264KeyFrame2x2SPS = []byte{0x67, 0x42, 0xc0, 0x1f, 0x0f, 0xd9, 0x1f, 0x88, 0x88, 0x84, 0x00, 0x00, 0x03, 0x00, 0x04, 0x00, 0x00, 0x03, 0x00, 0xc8, 0x3c, 0x60, 0xc9, 0x20}
	H264KeyFrame2x2PPS = []byte{0x68, 0x87, 0xcb, 0x83, 0xcb, 0x20}
	H264KeyFrame2x2IDR = []byte{0x65, 0x88, 0x84, 0x0a, 0xf2, 0x62, 0x80, 0x00, 0xa7, 0xbe}

	H264KeyFrame2x2 = [][]byte{H264KeyFrame2x2SPS, H264KeyFrame2x2PPS, H264KeyFrame2x2IDR}
)

type TranslationParamsRTP struct {
	snOrdering     SequenceNumberOrdering
	sequenceNumber uint16
	timestamp      uint32
}

type TranslationParamsVP8 struct {
	header *buffer.VP8
}

type TranslationParams struct {
	shouldDrop    bool
	shouldSendPLI bool
	rtp           *TranslationParamsRTP
	vp8           *TranslationParamsVP8
}

type SnTs struct {
	sequenceNumber uint16
	timestamp      uint32
}

type VideoLayers struct {
	spatial int32
	temporal int32
}

type ReceiverReportListener func(dt *DownTrack, report *rtcp.ReceiverReport)

// DownTrack  implements TrackLocal, is the track used to write packets
// to SFU Subscriber, the track handle the packets for simple, simulcast
// and SVC Publisher.
type DownTrack struct {
	id            string
	peerID        string
	bound         atomicBool
	kind          webrtc.RTPCodecType
	mime          string
	ssrc          uint32
	streamID      string
	maxTrack      int
	payloadType   uint8
	sequencer     *sequencer
	trackType     DownTrackType
	bufferFactory *buffer.Factory
	payload       *[]byte

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
	octetCount   atomicUint32
	packetCount  atomicUint32
	lossFraction atomicUint8

	// Debug info
	lastPli     atomicInt64
	lastRTP     atomicInt64
	pktsDropped atomicUint32

	// RTCP callbacks
	onRTCP func([]rtcp.Packet)
	onREMB func(dt *DownTrack, remb *rtcp.ReceiverEstimatedMaximumBitrate)

	// simulcast layer availability change callback
	onAvailableLayersChanged func(dt *DownTrack)

	// subscription change callback
	onSubscriptionChanged func(dt *DownTrack)

	// max layer change callback
	onSubscribedLayersChanged func(dt *DownTrack, layers VideoLayers)

	// packet sent callback
	onPacketSent []func(dt *DownTrack, size int)
}

// NewDownTrack returns a DownTrack.
func NewDownTrack(c webrtc.RTPCodecCapability, r TrackReceiver, bf *buffer.Factory, peerID string, mt int) (*DownTrack, error) {
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
		id:            r.TrackID(),
		peerID:        peerID,
		maxTrack:      mt,
		streamID:      r.StreamID(),
		bufferFactory: bf,
		receiver:      r,
		codec:         c,
		kind:          kind,
		forwarder:     NewForwarder(c, kind),
	}

	if strings.ToLower(c.MimeType) == "video/vp8" {
		d.payload = PacketFactory.Get().(*[]byte)
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
	return d.kind
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

// WriteRTP writes a RTP Packet to the DownTrack
func (d *DownTrack) WriteRTP(extPkt *buffer.ExtPacket, layer int32) error {
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
		d.pktsDropped.add(1)
		return err
	}

	payload := extPkt.Packet.Payload
	if tp.vp8 != nil {
		incomingVP8, _ := extPkt.Payload.(buffer.VP8)
		payload, err = d.translateVP8Packet(&extPkt.Packet, &incomingVP8, tp.vp8.header)
		if err != nil {
			d.pktsDropped.add(1)
			return err
		}
	}

	if d.sequencer != nil {
		meta := d.sequencer.push(extPkt.Packet.SequenceNumber, tp.rtp.sequenceNumber, tp.rtp.timestamp, 0, extPkt.Head)
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
		for _, f := range d.onPacketSent {
			f(d, hdr.MarshalSize()+len(payload))
		}
	} else {
		d.pktsDropped.add(1)
	}

	// LK-TODO maybe include RTP header size also
	d.UpdateStats(uint32(len(payload)))

	return err
}

// WritePaddingRTP tries to write as many padding only RTP packets as necessary
// to satisfy given size to the DownTrack
func (d *DownTrack) WritePaddingRTP(bytesToSend int) int {
	if d.packetCount.get() == 0 {
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
	// can be sent only on frame boudaries, writing on disabled tracks
	// will give more options.
	// LK-TODO-END
	if d.forwarder.Muted() {
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
		// on as yet unstarted streams which is a reasonble check.
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

		// LK-TODO - check if we should keep separate padding stats
		size := hdr.MarshalSize() + len(payload)
		d.UpdateStats(uint32(size))

		// LK-TODO-START
		// NACK buffer for these probe packets.
		// Probably okay to absorb the NACKs for these and ignore them.
		// Retransmssion is probably a sign of network congestion/badness.
		// So, retransmitting padding packets is only going to make matters worse.
		// LK-TODO-END

		bytesSent += size
	}

	return bytesSent
}

// Mute enables or disables media forwarding
func (d *DownTrack) Mute(val bool) {
	changed := d.forwarder.Mute(val)
	if !changed {
		return
	}

	if val {
		d.lossFraction.set(0)
	}

	if d.onSubscriptionChanged != nil {
		d.onSubscriptionChanged(d)
	}
}

// Close track
func (d *DownTrack) Close() {
	d.forwarder.Mute(true)

	// write blank frames after disabling so that other frames do not interfere.
	// Idea here is to send blank 1x1 key frames to flush the decoder buffer at the remote end.
	// Otherwise, with transceiver re-use last frame from previous stream is held in the
	// display buffer and there could be a brief moment where the previous stream is displayed.
	d.writeBlankFrameRTP()

	d.closeOnce.Do(func() {
		Logger.V(1).Info("Closing sender", "peer_id", d.peerID, "kind", d.kind)
		if d.payload != nil {
			PacketFactory.Put(d.payload)
		}
		if d.onCloseHandler != nil {
			d.onCloseHandler()
		}
	})
}

func (d *DownTrack) SetMaxSpatialLayer(spatialLayer int32) {
	changed, maxLayers := d.forwarder.SetMaxSpatialLayer(spatialLayer)
	if !changed {
		return
	}

	if d.onSubscribedLayersChanged != nil {
		d.onSubscribedLayersChanged(d, maxLayers)
	}
}

func (d *DownTrack) SetMaxTemporalLayer(temporalLayer int32) {
	changed, maxLayers := d.forwarder.SetMaxTemporalLayer(temporalLayer)
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

func (d *DownTrack) UptrackLayersChange(availableLayers []uint16) {
	d.forwarder.UptrackLayersChange(availableLayers)

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

func (d *DownTrack) Allocate(availableChannelCapacity int64) VideoAllocationResult {
	return d.forwarder.Allocate(availableChannelCapacity, d.receiver.GetBitrateTemporalCumulative())
}

func (d *DownTrack) TryAllocate(additionalChannelCapacity int64) VideoAllocationResult {
	return d.forwarder.TryAllocate(additionalChannelCapacity, d.receiver.GetBitrateTemporalCumulative())
}

func (d *DownTrack) AllocateNextHigher() VideoBoostResult {
	return d.forwarder.AllocateNextHigher(d.receiver.GetBitrateTemporalCumulative())
}

func (d *DownTrack) AllocationState() VideoAllocationState {
	return d.forwarder.AllocationState()
}

func (d *DownTrack) AllocationBandwidth() int64 {
	return d.forwarder.AllocationBandwidth()
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

	currentSpatialLayer := d.forwarder.CurrentSpatialLayer()
	if currentSpatialLayer == InvalidSpatialLayer {
		return nil
	}

	srRTP, srNTP := d.receiver.GetSenderReportTime(currentSpatialLayer)
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
		OctetCount:  octets,
	}
}

func (d *DownTrack) UpdateStats(packetLen uint32) {
	d.octetCount.add(packetLen)
	d.packetCount.add(1)
}

func (d *DownTrack) writeBlankFrameRTP() error {
	// don't send if nothing has been sent
	if d.packetCount.get() == 0 {
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

		switch d.mime {
		case "video/vp8":
			err = d.writeVP8BlankFrame(&hdr, frameEndNeeded)
		case "video/h264":
			err = d.writeH264BlankFrame(&hdr, frameEndNeeded)
		default:
			return nil
		}

		// only the first frame will need frameEndNeeded to close out the
		// previous picture, rest are small key frames
		frameEndNeeded = false
	}

	return nil
}

func (d *DownTrack) writeVP8BlankFrame(hdr *rtp.Header, frameEndNeeded bool) error {
	blankVP8, err := d.forwarder.GetPaddingVP8(frameEndNeeded)
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
	pkts, err := rtcp.Unmarshal(bytes)
	if err != nil {
		Logger.Error(err, "Unmarshal rtcp receiver packets err")
		return
	}

	if d.onRTCP != nil {
		d.onRTCP(pkts)
	}

	pliOnce := true
	sendPliOnce := func() {
		if pliOnce {
			targetSpatialLayer := d.forwarder.TargetSpatialLayer()
			if targetSpatialLayer != InvalidSpatialLayer {
				d.lastPli.set(time.Now().UnixNano())
				d.receiver.SendPLI(targetSpatialLayer)
				pliOnce = false
			}
		}
	}

	maxRatePacketLoss := uint8(0)
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

func (d *DownTrack) maybeTranslateVP8(pkt *rtp.Packet, meta packetMeta) error {
	if d.mime != "video/vp8" || len(pkt.Payload) == 0 {
		return nil
	}

	var incomingVP8 buffer.VP8
	if err := incomingVP8.Unmarshal(pkt.Payload); err != nil {
		return err
	}

	translatedVP8 := meta.unpackVP8()
	payload, err := d.translateVP8Packet(pkt, &incomingVP8, translatedVP8)
	if err != nil {
		return err
	}

	pkt.Payload = payload
	return nil
}

func (d *DownTrack) retransmitPackets(nackedPackets []packetMeta) {
	src := PacketFactory.Get().(*[]byte)
	defer PacketFactory.Put(src)
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

func (d *DownTrack) translateVP8Packet(pkt *rtp.Packet, incomingVP8 *buffer.VP8, translatedVP8 *buffer.VP8) (buf []byte, err error) {
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
		"Muted":               d.forwarder.Muted(),
		"CurrentSpatialLayer": d.forwarder.CurrentSpatialLayer,
		"Stats":               stats,
	}
}

//---------------------------------------------------

//
// Forwarder
//
type VideoStreamingChange int

const (
	VideoStreamingChangeNone VideoStreamingChange = iota
	VideoStreamingChangePausing
	VideoStreamingChangeResuming
)

type VideoAllocationState int

const (
	VideoAllocationStateNone VideoAllocationState = iota
	VideoAllocationStateMuted
	VideoAllocationStateFeedDry
	VideoAllocationStateAwaitingMeasurement
	VideoAllocationStateOptimal
	VideoAllocationStateDeficient
)

type VideoAllocationResult struct {
	change VideoStreamingChange
	state VideoAllocationState
	bandwidthDelta int64
}

type VideoBoostResult struct {
	boosted bool
	bandwidthRequested int64
	optimalBandwidthNeeded int64
}

type Forwarder struct {
	lock  sync.RWMutex
	codec webrtc.RTPCodecCapability
	kind  webrtc.RTPCodecType

	muted bool

	started  bool
	lastSSRC uint32
	lTSCalc  int64

	maxSpatialLayer     int32
	currentSpatialLayer int32
	targetSpatialLayer  int32

	maxTemporalLayer     int32
	currentTemporalLayer int32
	targetTemporalLayer  int32

	lastAllocationState VideoAllocationState
	lastAllocationRequestBps int64

	availableLayers []uint16

	rtpMunger *RTPMunger
	vp8Munger *VP8Munger
}

func NewForwarder(codec webrtc.RTPCodecCapability, kind webrtc.RTPCodecType) *Forwarder {
	f := &Forwarder{
		codec: codec,
		kind:  kind,

		// start off with nothing, let streamallocator set things
		currentSpatialLayer:  InvalidSpatialLayer,
		targetSpatialLayer:   InvalidSpatialLayer,
		currentTemporalLayer: InvalidTemporalLayer,
		targetTemporalLayer:  InvalidTemporalLayer,

		lastAllocationState: VideoAllocationStateNone,

		rtpMunger: NewRTPMunger(),
	}

	if strings.ToLower(codec.MimeType) == "video/vp8" {
		f.vp8Munger = NewVP8Munger()
	}

	if f.kind == webrtc.RTPCodecTypeVideo {
		f.maxSpatialLayer = 2
		f.maxTemporalLayer = 2
	} else {
		f.maxSpatialLayer = InvalidSpatialLayer
		f.maxTemporalLayer = InvalidTemporalLayer
	}

	return f
}

func (f *Forwarder) Mute(val bool) bool {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.muted == val {
		return false
	}

	f.muted = val
	return true
}

func (f *Forwarder) Muted() bool {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.muted
}

func (f *Forwarder) SetMaxSpatialLayer(spatialLayer int32) (bool, VideoLayers) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if spatialLayer == f.maxSpatialLayer {
		return false, VideoLayers{}
	}

	f.maxSpatialLayer = spatialLayer

	return true, VideoLayers{
		spatial: f.maxSpatialLayer,
		temporal: f.maxTemporalLayer,
	}
}

func (f *Forwarder) CurrentSpatialLayer() int32 {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.currentSpatialLayer
}

func (f *Forwarder) TargetSpatialLayer() int32 {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.targetSpatialLayer
}

func (f *Forwarder) SetMaxTemporalLayer(temporalLayer int32) (bool, VideoLayers) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if temporalLayer == f.maxTemporalLayer {
		return false, VideoLayers{}
	}

	f.maxTemporalLayer = temporalLayer

	return true, VideoLayers{
		spatial: f.maxSpatialLayer,
		temporal: f.maxTemporalLayer,
	}
}

func (f *Forwarder) MaxLayers() VideoLayers {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return VideoLayers{
		spatial: f.maxSpatialLayer,
		temporal: f.maxTemporalLayer,
	}
}

func (f *Forwarder) GetForwardingStatus() ForwardingStatus {
	f.lock.RLock()
	defer f.lock.RUnlock()

	if f.targetSpatialLayer == InvalidSpatialLayer {
		return ForwardingStatusOff
	}

	if f.targetSpatialLayer < f.maxSpatialLayer {
		return ForwardingStatusPartial
	}

	return ForwardingStatusOptimal
}

func (f *Forwarder) UptrackLayersChange(availableLayers []uint16) {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.availableLayers = availableLayers
}

func (f *Forwarder) disable() {
	f.currentSpatialLayer = InvalidSpatialLayer
	f.targetSpatialLayer = InvalidSpatialLayer

	f.currentTemporalLayer = InvalidTemporalLayer
	f.targetTemporalLayer = InvalidTemporalLayer
}

func (f *Forwarder) getOptimalBandwidthNeeded(brs [3][4]int64) int64 {
	optimalBandwidthNeeded := int64(0)
	for i := f.maxSpatialLayer; i >= 0; i-- {
		for j := f.maxTemporalLayer; j >= 0; j-- {
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

	return optimalBandwidthNeeded
}

func (f *Forwarder) allocate(availableChannelCapacity int64, canPause bool, brs [3][4]int64) (result VideoAllocationResult) {
	// should never get called on audio tracks, just for safety
	if f.kind == webrtc.RTPCodecTypeAudio {
		return
	}

	if f.muted {
		result.state = VideoAllocationStateMuted
		result.bandwidthDelta = -f.lastAllocationRequestBps

		f.lastAllocationRequestBps = 0
		return
	}

	optimalBandwidthNeeded := f.getOptimalBandwidthNeeded(brs)
	if optimalBandwidthNeeded == 0 {
		if len(f.availableLayers) == 0 {
			// feed is dry
			result.state = VideoAllocationStateFeedDry
			result.bandwidthDelta = -f.lastAllocationRequestBps

			f.lastAllocationState = result.state
			f.lastAllocationRequestBps = 0

			return
		}

		// feed bitrate is not yet calculated
		result.state = VideoAllocationStateAwaitingMeasurement
		f.lastAllocationState = result.state

		if availableChannelCapacity == ChannelCapacityInfinity {
			// channel capacity allows a free pass.
			// So, resume with the highest layer available <= max subscribed layer

			// if already optimistically started, nothing else to do
			if f.targetSpatialLayer != InvalidSpatialLayer {
				return
			}

			f.targetSpatialLayer = int32(f.availableLayers[len(f.availableLayers) - 1])
			if f.targetSpatialLayer > f.maxSpatialLayer {
				f.targetSpatialLayer = f.maxSpatialLayer
			}

			f.targetTemporalLayer = f.maxTemporalLayer
			if f.targetTemporalLayer == InvalidTemporalLayer {
				f.targetTemporalLayer = 0
			}

			result.change = VideoStreamingChangeResuming
		} else {
			// if not optimistically started, nothing else to do
			if f.targetSpatialLayer == InvalidSpatialLayer {
				return
			}

			if canPause {
				// disable it as it is not known how big this stream is
				// and if it will fit in the available channel capacity
				result.change = VideoStreamingChangePausing
				result.state = VideoAllocationStateDeficient
				result.bandwidthDelta = -f.lastAllocationRequestBps

				f.lastAllocationState = result.state
				f.lastAllocationRequestBps = 0

				f.disable()
			}
		}
		return
	}

	// LK-TODO for temporal preference, traverse the bitrates array the other way
	for i := f.maxSpatialLayer; i >= 0; i-- {
		for j := f.maxTemporalLayer; j >= 0; j-- {
			if brs[i][j] == 0 {
				continue
			}
			if brs[i][j] < availableChannelCapacity {
				if f.targetSpatialLayer == InvalidSpatialLayer {
					result.change = VideoStreamingChangeResuming
				}
				result.bandwidthDelta = brs[i][j] - f.lastAllocationRequestBps
				if brs[i][j] == optimalBandwidthNeeded {
					result.state = VideoAllocationStateOptimal
				} else {
					result.state = VideoAllocationStateDeficient
				}

				f.lastAllocationState = result.state
				f.lastAllocationRequestBps = brs[i][j]

				f.targetSpatialLayer = int32(i)
				f.targetTemporalLayer = int32(j)
				return
			}
		}
	}

	if !canPause {
		// do not pause if preserving
		// although preserving, currently streamed layers could have a different bitrate,
		// but not updating to prevent entropy increase.
		result.state = f.lastAllocationState
		return
	}

	// no layer fits in the available channel capacity, disable the track
	if f.targetSpatialLayer != InvalidSpatialLayer {
		result.change = VideoStreamingChangePausing
	}
	result.state = VideoAllocationStateDeficient
	result.bandwidthDelta = -f.lastAllocationRequestBps

	f.lastAllocationState = result.state
	f.lastAllocationRequestBps = 0

	f.disable()
	return
}

func (f *Forwarder) Allocate(availableChannelCapacity int64, brs [3][4]int64) VideoAllocationResult {
	f.lock.Lock()
	defer f.lock.Unlock()

	return f.allocate(availableChannelCapacity, true, brs)
}

func (f *Forwarder) TryAllocate(additionalChannelCapacity int64, brs [3][4]int64) VideoAllocationResult {
	f.lock.Lock()
	defer f.lock.Unlock()

	return f.allocate(f.lastAllocationRequestBps + additionalChannelCapacity, false, brs)
}

func (f *Forwarder) AllocateNextHigher(brs [3][4]int64) (result VideoBoostResult) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.kind == webrtc.RTPCodecTypeAudio {
		return
	}

	// if targets are still pending, don't increase
	if f.targetSpatialLayer != InvalidSpatialLayer {
		if f.targetSpatialLayer != f.currentSpatialLayer || f.targetTemporalLayer != f.currentTemporalLayer {
			return
		}
	}

	result.optimalBandwidthNeeded = f.getOptimalBandwidthNeeded(brs)
	if result.optimalBandwidthNeeded == 0 {
		// either feed is dry or bitrates not available yet
		return
	}

	// try moving temporal layer up in the current spatial layer
	nextTemporalLayer := f.currentTemporalLayer + 1
	currentSpatialLayer := f.currentSpatialLayer
	if currentSpatialLayer == InvalidSpatialLayer {
		currentSpatialLayer = 0
	}
	if nextTemporalLayer <= f.maxTemporalLayer && brs[currentSpatialLayer][nextTemporalLayer] > 0 {
		f.targetSpatialLayer = currentSpatialLayer
		f.targetTemporalLayer = nextTemporalLayer

		result.boosted = true
		result.bandwidthRequested = brs[currentSpatialLayer][nextTemporalLayer]
		return
	}

	// try moving spatial layer up if already at max temporal layer of current spatial layer
	nextSpatialLayer := f.currentSpatialLayer + 1
	if nextSpatialLayer <= f.maxSpatialLayer && brs[nextSpatialLayer][0] > 0 {
		f.targetSpatialLayer = nextSpatialLayer
		f.targetTemporalLayer = 0

		result.boosted = true
		result.bandwidthRequested = brs[nextSpatialLayer][0]
		return
	}

	return
}

func (f *Forwarder) AllocationState() VideoAllocationState {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.lastAllocationState
}

func (f *Forwarder) AllocationBandwidth() int64 {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.lastAllocationRequestBps
}

func (f *Forwarder) GetTranslationParams(extPkt *buffer.ExtPacket, layer int32) (*TranslationParams, error) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.muted {
		return &TranslationParams{
			shouldDrop: true,
		}, nil
	}

	switch f.kind {
	case webrtc.RTPCodecTypeAudio:
		return f.getTranslationParamsAudio(extPkt)
	case webrtc.RTPCodecTypeVideo:
		return f.getTranslationParamsVideo(extPkt, layer)
	}

	return nil, ErrUnknownKind
}

// should be called with lock held
func (f *Forwarder) getTranslationParamsAudio(extPkt *buffer.ExtPacket) (*TranslationParams, error) {
	if f.lastSSRC != extPkt.Packet.SSRC {
		if !f.started {
			// start of stream
			f.started = true
			f.rtpMunger.SetLastSnTs(extPkt)
		} else {
			// LK-TODO-START
			// TS offset of 1 is not accurate. It should ideally
			// be driven by packetization of the incoming track.
			// But, on a track switch, won't have any historic data
			// of a new track though.
			// LK-TODO-END
			f.rtpMunger.UpdateSnTsOffsets(extPkt, 1, 1)
		}

		f.lastSSRC = extPkt.Packet.SSRC
	}

	tp := &TranslationParams{}

	tpRTP, err := f.rtpMunger.UpdateAndGetSnTs(extPkt)
	if err != nil {
		tp.shouldDrop = true
		if err == ErrPaddingOnlyPacket || err == ErrDuplicatePacket || err == ErrOutOfOrderSequenceNumberCacheMiss {
			return tp, nil
		}

		return tp, err
	}

	tp.rtp = tpRTP
	return tp, nil
}

// should be called with lock held
func (f *Forwarder) getTranslationParamsVideo(extPkt *buffer.ExtPacket, layer int32) (*TranslationParams, error) {
	tp := &TranslationParams{}

	if f.targetSpatialLayer == InvalidSpatialLayer {
		// stream is paused by streamallocator
		tp.shouldDrop = true
		return tp, nil
	}

	tp.shouldSendPLI = false
	if f.targetSpatialLayer != f.currentSpatialLayer {
		if f.targetSpatialLayer == layer {
			if extPkt.KeyFrame {
				// lock to target layer
				f.currentSpatialLayer = f.targetSpatialLayer
			} else {
				tp.shouldSendPLI = true
			}
		}
	}

	if f.currentSpatialLayer != layer {
		tp.shouldDrop = true
		return tp, nil
	}

	if f.targetSpatialLayer < f.currentSpatialLayer && f.targetSpatialLayer < f.maxSpatialLayer {
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
		tp.shouldDrop = true
		return tp, nil
	}

	if f.lastSSRC != extPkt.Packet.SSRC {
		if !f.started {
			f.started = true
			f.rtpMunger.SetLastSnTs(extPkt)
			if f.vp8Munger != nil {
				f.vp8Munger.SetLast(extPkt)
			}
		} else {
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
			tDiffMs := (extPkt.Arrival - f.lTSCalc) / 1e6
			td := uint32((tDiffMs * (int64(f.codec.ClockRate) / 1000)) / 1000)
			if td == 0 {
				td = 1
			}
			f.rtpMunger.UpdateSnTsOffsets(extPkt, 1, td)
			if f.vp8Munger != nil {
				f.vp8Munger.UpdateOffsets(extPkt)
			}
		}

		f.lastSSRC = extPkt.Packet.SSRC
	}

	f.lTSCalc = extPkt.Arrival

	tpRTP, err := f.rtpMunger.UpdateAndGetSnTs(extPkt)
	if err != nil {
		tp.shouldDrop = true
		if err == ErrPaddingOnlyPacket || err == ErrDuplicatePacket || err == ErrOutOfOrderSequenceNumberCacheMiss {
			return tp, nil
		}

		return tp, err
	}

	if f.vp8Munger == nil {
		tp.rtp = tpRTP
		return tp, nil
	}

	tpVP8, err := f.vp8Munger.UpdateAndGet(extPkt, tpRTP.snOrdering, f.currentTemporalLayer)
	if err != nil {
		tp.shouldDrop = true
		if err == ErrFilteredVP8TemporalLayer || err == ErrOutOfOrderVP8PictureIdCacheMiss {
			if err == ErrFilteredVP8TemporalLayer {
				// filtered temporal layer, update sequence number offset to prevent holes
				f.rtpMunger.PacketDropped(extPkt)
			}
			return tp, nil
		}

		return tp, err
	}

	// catch up temporal layer if necessary
	if tpVP8 != nil && f.currentTemporalLayer != f.targetTemporalLayer {
		if tpVP8.header.TIDPresent == 1 && tpVP8.header.TID <= uint8(f.targetTemporalLayer) {
			f.currentTemporalLayer = f.targetTemporalLayer
		}
	}

	tp.rtp = tpRTP
	tp.vp8 = tpVP8
	return tp, nil
}

func (f *Forwarder) GetSnTsForPadding(num int) ([]SnTs, error) {
	f.lock.Lock()
	defer f.lock.Unlock()

	// padding is used for probing. Padding packets should be
	// at frame boundaries only to ensure decoder sequencer does
	// not get out-of-sync. But, when a stream is paused,
	// force a frame marker as a restart of the stream will
	// start with a key frame which will reset the decoder.
	forceMarker := false
	if f.targetSpatialLayer == InvalidSpatialLayer {
		forceMarker = true
	}
	return f.rtpMunger.UpdateAndGetPaddingSnTs(num, 0, 0, forceMarker)
}

func (f *Forwarder) GetSnTsForBlankFrames() ([]SnTs, bool, error) {
	f.lock.Lock()
	defer f.lock.Unlock()

	num := RTPBlankFramesMax
	frameEndNeeded := !f.rtpMunger.IsOnFrameBoundary()
	if frameEndNeeded {
		num++
	}
	snts, err := f.rtpMunger.UpdateAndGetPaddingSnTs(num, f.codec.ClockRate, 30, frameEndNeeded)
	return snts, frameEndNeeded, err
}

func (f *Forwarder) GetPaddingVP8(frameEndNeeded bool) (*buffer.VP8, error) {
	f.lock.Lock()
	defer f.lock.Unlock()

	return f.vp8Munger.UpdateAndGetPadding(!frameEndNeeded)
}

func (f *Forwarder) GetRTPMungerParams() RTPMungerParams {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.rtpMunger.GetParams()
}

//---------------------------------------------------

//
// RTPMunger
//
type RTPMungerParams struct {
	highestIncomingSN uint16
	lastSN            uint16
	snOffset          uint16
	lastTS            uint32
	tsOffset          uint32
	lastMarker        bool

	missingSNs map[uint16]uint16
}

type RTPMunger struct {
	RTPMungerParams
}

func NewRTPMunger() *RTPMunger {
	return &RTPMunger{RTPMungerParams: RTPMungerParams{
		missingSNs: make(map[uint16]uint16, 10),
	}}
}

func (r *RTPMunger) GetParams() RTPMungerParams {
	return RTPMungerParams{
		highestIncomingSN: r.highestIncomingSN,
		lastSN:            r.lastSN,
		snOffset:          r.snOffset,
		lastTS:            r.lastTS,
		tsOffset:          r.tsOffset,
		lastMarker:        r.lastMarker,
	}
}

func (r *RTPMunger) SetLastSnTs(extPkt *buffer.ExtPacket) {
	r.highestIncomingSN = extPkt.Packet.SequenceNumber - 1
	r.lastSN = extPkt.Packet.SequenceNumber
	r.lastTS = extPkt.Packet.Timestamp
}

func (r *RTPMunger) UpdateSnTsOffsets(extPkt *buffer.ExtPacket, snAdjust uint16, tsAdjust uint32) {
	r.highestIncomingSN = extPkt.Packet.SequenceNumber - 1
	r.snOffset = extPkt.Packet.SequenceNumber - r.lastSN - snAdjust
	r.tsOffset = extPkt.Packet.Timestamp - r.lastTS - tsAdjust

	// clear incoming missing sequence numbers on layer/source switch
	r.missingSNs = make(map[uint16]uint16, 10)
}

func (r *RTPMunger) PacketDropped(extPkt *buffer.ExtPacket) {
	if !extPkt.Head {
		return
	}

	r.highestIncomingSN = extPkt.Packet.SequenceNumber
	r.snOffset += 1
}

func (r *RTPMunger) UpdateAndGetSnTs(extPkt *buffer.ExtPacket) (*TranslationParamsRTP, error) {
	// if out-of-order, look up missing sequence number cache
	if !extPkt.Head {
		snOffset, ok := r.missingSNs[extPkt.Packet.SequenceNumber]
		if !ok {
			return &TranslationParamsRTP{
				snOrdering: SequenceNumberOrderingOutOfOrder,
			}, ErrOutOfOrderSequenceNumberCacheMiss
		}

		delete(r.missingSNs, extPkt.Packet.SequenceNumber)
		return &TranslationParamsRTP{
			snOrdering:     SequenceNumberOrderingOutOfOrder,
			sequenceNumber: extPkt.Packet.SequenceNumber - snOffset,
			timestamp:      extPkt.Packet.Timestamp - r.tsOffset,
		}, nil
	}

	ordering := SequenceNumberOrderingContiguous

	// if there are gaps, record it in missing sequence number cache
	diff := extPkt.Packet.SequenceNumber - r.highestIncomingSN
	if diff > 1 {
		ordering = SequenceNumberOrderingGap

		for i := r.highestIncomingSN + 1; i != extPkt.Packet.SequenceNumber; i++ {
			r.missingSNs[i] = r.snOffset
		}
	} else {
		// can get duplicate packet due to FEC
		if diff == 0 {
			return &TranslationParamsRTP{
				snOrdering: SequenceNumberOrderingDuplicate,
			}, ErrDuplicatePacket
		}

		// if padding only packet, can be dropped and sequence number adjusted
		// as it is contiguous and in order. That means this is the highest
		// incoming sequence number and it is a good point to adjust
		// sequence number offset.
		if len(extPkt.Packet.Payload) == 0 {
			r.highestIncomingSN = extPkt.Packet.SequenceNumber
			r.snOffset += 1

			return &TranslationParamsRTP{
				snOrdering: SequenceNumberOrderingContiguous,
			}, ErrPaddingOnlyPacket
		}
	}

	// in-order incoming packet, may or may not be contiguous.
	// In the case of loss (i. e. incoming sequence number is not contiguous),
	// forward even if it is a padding only packet. With temporal scalability,
	// it is unclear if the current packet should be dropped if it is not
	// contiguous. Hence forward anything that is not contiguous.
	// Reference: http://www.rtcbits.com/2017/04/howto-implement-temporal-scalability.html
	mungedSN := extPkt.Packet.SequenceNumber - r.snOffset
	mungedTS := extPkt.Packet.Timestamp - r.tsOffset

	r.highestIncomingSN = extPkt.Packet.SequenceNumber
	r.lastSN = mungedSN
	r.lastTS = mungedTS
	r.lastMarker = extPkt.Packet.Marker

	return &TranslationParamsRTP{
		snOrdering:     ordering,
		sequenceNumber: mungedSN,
		timestamp:      mungedTS,
	}, nil
}

func (r *RTPMunger) UpdateAndGetPaddingSnTs(num int, clockRate uint32, frameRate uint32, forceMarker bool) ([]SnTs, error) {
	tsOffset := 0
	if !r.lastMarker {
		if !forceMarker {
			return nil, ErrPaddingNotOnFrameBoundary
		} else {
			// if forcing frame end, use timestamp of latest received frame for the first one
			tsOffset = 1
		}
	}

	vals := make([]SnTs, num)
	for i := 0; i < num; i++ {
		vals[i].sequenceNumber = r.lastSN + uint16(i) + 1
		if frameRate != 0 {
			vals[i].timestamp = r.lastTS + uint32(i+1-tsOffset)*(clockRate/frameRate)
		} else {
			vals[i].timestamp = r.lastTS
		}
	}

	r.lastSN = vals[num-1].sequenceNumber
	r.snOffset -= uint16(num)

	if forceMarker {
		r.lastMarker = true
	}

	return vals, nil
}

func (r *RTPMunger) IsOnFrameBoundary() bool {
	return r.lastMarker
}

//---------------------------------------------------

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
	VP8MungerParams
}

func NewVP8Munger() *VP8Munger {
	return &VP8Munger{VP8MungerParams: VP8MungerParams{
		missingPictureIds:    orderedmap.NewOrderedMap(),
		lastDroppedPictureId: -1,
	}}
}

func (v *VP8Munger) SetLast(extPkt *buffer.ExtPacket) {
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

func (v *VP8Munger) UpdateAndGet(extPkt *buffer.ExtPacket, ordering SequenceNumberOrdering, maxTemporalLayer int32) (*TranslationParamsVP8, error) {
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
		return &TranslationParamsVP8{
			header: vp8Packet,
		}, nil
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
	return &TranslationParamsVP8{
		header: vp8Packet,
	}, nil
}

func (v *VP8Munger) UpdateAndGetPadding(newPicture bool) (*buffer.VP8, error) {
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

//-----------------------------

//
// VP8Munger
//
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

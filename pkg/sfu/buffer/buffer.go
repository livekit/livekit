// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package buffer

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/gammazero/deque"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
	"go.uber.org/atomic"

	"github.com/livekit/livekit-server/pkg/sfu/audio"
	"github.com/livekit/livekit-server/pkg/sfu/mime"
	act "github.com/livekit/livekit-server/pkg/sfu/rtpextension/abscapturetime"
	dd "github.com/livekit/livekit-server/pkg/sfu/rtpextension/dependencydescriptor"
	"github.com/livekit/livekit-server/pkg/sfu/rtpstats"
	"github.com/livekit/livekit-server/pkg/sfu/utils"
	sutils "github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/mediatransportutil/pkg/bucket"
	"github.com/livekit/mediatransportutil/pkg/nack"
	"github.com/livekit/mediatransportutil/pkg/twcc"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/mono"
)

const (
	ReportDelta = 1e9

	InitPacketBufferSizeVideo = 300
	InitPacketBufferSizeAudio = 70
)

type pendingPacket struct {
	arrivalTime int64
	packet      []byte
}

type ExtPacket struct {
	VideoLayer
	Arrival              int64
	ExtSequenceNumber    uint64
	ExtTimestamp         uint64
	Packet               *rtp.Packet
	Payload              interface{}
	KeyFrame             bool
	RawPacket            []byte
	DependencyDescriptor *ExtDependencyDescriptor
	AbsCaptureTimeExt    *act.AbsCaptureTime
	IsOutOfOrder         bool
}

// VideoSize represents video resolution
type VideoSize struct {
	Width  uint32
	Height uint32
}

// Buffer contains all packets
type Buffer struct {
	sync.RWMutex
	readCond        *sync.Cond
	bucket          *bucket.Bucket[uint64]
	nacker          *nack.NackQueue
	maxVideoPkts    int
	maxAudioPkts    int
	codecType       webrtc.RTPCodecType
	extPackets      deque.Deque[*ExtPacket]
	pPackets        []pendingPacket
	closeOnce       sync.Once
	mediaSSRC       uint32
	clockRate       uint32
	lastReport      int64
	twccExtID       uint8
	audioLevelExtID uint8
	bound           bool
	closed          atomic.Bool

	rtpParameters  webrtc.RTPParameters
	payloadType    uint8
	rtxPayloadType uint8
	mime           mime.MimeType

	snRangeMap *utils.RangeMap[uint64, uint64]

	latestTSForAudioLevelInitialized bool
	latestTSForAudioLevel            uint32

	twcc                    *twcc.Responder
	audioLevelParams        audio.AudioLevelParams
	audioLevel              *audio.AudioLevel
	enableAudioLossProxying bool

	lastPacketRead int

	pliThrottle int64

	rtpStats             *rtpstats.RTPStatsReceiver
	rrSnapshotId         uint32
	deltaStatsSnapshotId uint32
	ppsSnapshotId        uint32

	lastFractionLostToReport uint8 // Last fraction lost from subscribers, should report to publisher; Audio only

	// callbacks
	onClose            func()
	onRtcpFeedback     func([]rtcp.Packet)
	onRtcpSenderReport func()
	onFpsChanged       func()
	onFinalRtpStats    func(*livekit.RTPStats)
	onCodecChange      func(webrtc.RTPCodecParameters)
	onVideoSizeChanged func([]VideoSize)

	// video size tracking for multiple spatial layers
	currentVideoSize [DefaultMaxLayerSpatial + 1]VideoSize

	// logger
	logger logger.Logger

	// dependency descriptor
	ddExtID  uint8
	ddParser *DependencyDescriptorParser

	paused              bool
	frameRateCalculator [DefaultMaxLayerSpatial + 1]FrameRateCalculator
	frameRateCalculated bool

	packetNotFoundCount   atomic.Uint32
	packetTooOldCount     atomic.Uint32
	extPacketTooMuchCount atomic.Uint32

	primaryBufferForRTX *Buffer
	rtxPktBuf           []byte

	absCaptureTimeExtID uint8

	keyFrameSeederGeneration atomic.Int32
}

// NewBuffer constructs a new Buffer
func NewBuffer(ssrc uint32, maxVideoPkts, maxAudioPkts int) *Buffer {
	l := logger.GetLogger() // will be reset with correct context via SetLogger
	b := &Buffer{
		mediaSSRC:    ssrc,
		maxVideoPkts: maxVideoPkts,
		maxAudioPkts: maxAudioPkts,
		snRangeMap:   utils.NewRangeMap[uint64, uint64](100),
		pliThrottle:  int64(500 * time.Millisecond),
		logger:       l.WithComponent(sutils.ComponentPub).WithComponent(sutils.ComponentSFU),
	}
	b.readCond = sync.NewCond(&b.RWMutex)
	b.extPackets.SetBaseCap(128)
	return b
}

func (b *Buffer) SetLogger(logger logger.Logger) {
	b.Lock()
	defer b.Unlock()

	b.logger = logger.WithComponent(sutils.ComponentPub).WithComponent(sutils.ComponentSFU).WithValues("ssrc", b.mediaSSRC)
	if b.rtpStats != nil {
		b.rtpStats.SetLogger(b.logger)
	}
}

func (b *Buffer) SetPaused(paused bool) {
	b.Lock()
	defer b.Unlock()

	b.paused = paused
}

func (b *Buffer) SetTWCCAndExtID(twcc *twcc.Responder, extID uint8) {
	b.Lock()
	defer b.Unlock()

	b.twcc = twcc
	b.twccExtID = extID
}

func (b *Buffer) SetAudioLevelParams(audioLevelParams audio.AudioLevelParams) {
	b.Lock()
	defer b.Unlock()

	b.audioLevelParams = audioLevelParams
}

func (b *Buffer) SetAudioLossProxying(enable bool) {
	b.Lock()
	defer b.Unlock()

	b.enableAudioLossProxying = enable
}

func (b *Buffer) Bind(params webrtc.RTPParameters, codec webrtc.RTPCodecCapability, bitrates int) {
	b.Lock()
	defer b.Unlock()
	if b.bound {
		return
	}

	b.rtpStats = rtpstats.NewRTPStatsReceiver(rtpstats.RTPStatsParams{
		ClockRate: codec.ClockRate,
		Logger:    b.logger,
	})
	b.rrSnapshotId = b.rtpStats.NewSnapshotId()
	b.deltaStatsSnapshotId = b.rtpStats.NewSnapshotId()
	b.ppsSnapshotId = b.rtpStats.NewSnapshotId()

	b.clockRate = codec.ClockRate
	b.lastReport = mono.UnixNano()
	b.mime = mime.NormalizeMimeType(codec.MimeType)
	b.rtpParameters = params
	for _, codecParameter := range params.Codecs {
		if mime.IsMimeTypeStringEqual(codecParameter.MimeType, codec.MimeType) {
			b.payloadType = uint8(codecParameter.PayloadType)
			break
		}
	}

	if b.payloadType == 0 {
		b.logger.Warnw("could not find payload type for codec", nil, "codec", codec.MimeType, "parameters", params)
		b.payloadType = uint8(params.Codecs[0].PayloadType)
	}

	// find RTX payload type
	for _, codec := range params.Codecs {
		if mime.IsMimeTypeStringRTX(codec.MimeType) && strings.Contains(codec.SDPFmtpLine, fmt.Sprintf("apt=%d", b.payloadType)) {
			b.rtxPayloadType = uint8(codec.PayloadType)
			break
		}
	}

	for _, ext := range params.HeaderExtensions {
		switch ext.URI {
		case dd.ExtensionURI:
			if b.ddExtID != 0 {
				b.logger.Warnw("multiple dependency descriptor extensions found", nil, "id", ext.ID, "previous", b.ddExtID)
				continue
			}
			b.ddExtID = uint8(ext.ID)
			b.createDDParserAndFrameRateCalculator()

		case sdp.AudioLevelURI:
			b.audioLevelExtID = uint8(ext.ID)
			b.audioLevel = audio.NewAudioLevel(b.audioLevelParams)

		case act.AbsCaptureTimeURI:
			b.absCaptureTimeExtID = uint8(ext.ID)
		}
	}

	switch {
	case mime.IsMimeTypeAudio(b.mime):
		b.codecType = webrtc.RTPCodecTypeAudio
		b.bucket = bucket.NewBucket[uint64](InitPacketBufferSizeAudio)

	case mime.IsMimeTypeVideo(b.mime):
		b.codecType = webrtc.RTPCodecTypeVideo
		b.bucket = bucket.NewBucket[uint64](InitPacketBufferSizeVideo)
		if b.frameRateCalculator[0] == nil {
			b.createFrameRateCalculator()
		}
		if bitrates > 0 {
			pps := bitrates / 8 / 1200
			for pps > b.bucket.Capacity() {
				if b.bucket.Grow() >= b.maxVideoPkts {
					break
				}
			}
		}

	default:
		b.codecType = webrtc.RTPCodecType(0)
	}

	for _, fb := range codec.RTCPFeedback {
		switch fb.Type {
		case webrtc.TypeRTCPFBGoogREMB:
			b.logger.Debugw("Setting feedback", "type", webrtc.TypeRTCPFBGoogREMB)
			b.logger.Debugw("REMB not supported, RTCP feedback will not be generated")
		case webrtc.TypeRTCPFBNACK:
			// pion use a single mediaengine to manage negotiated codecs of peerconnection, that means we can't have different
			// codec settings at track level for same codec type, so enable nack for all audio receivers but don't create nack queue
			// for red codec.
			if b.mime == mime.MimeTypeRED {
				break
			}
			b.logger.Debugw("Setting feedback", "type", webrtc.TypeRTCPFBNACK)
			b.nacker = nack.NewNACKQueue(nack.NackQueueParamsDefault)
		}
	}

	for _, pp := range b.pPackets {
		b.calc(pp.packet, nil, pp.arrivalTime, false)
	}
	b.pPackets = nil
	b.bound = true

	if mime.IsMimeTypeVideo(b.mime) {
		go b.seedKeyFrame(b.keyFrameSeederGeneration.Inc())
	}
}

func (b *Buffer) OnCodecChange(fn func(webrtc.RTPCodecParameters)) {
	b.Lock()
	b.onCodecChange = fn
	b.Unlock()
}

func (b *Buffer) createDDParserAndFrameRateCalculator() {
	if mime.IsMimeTypeSVCCapable(b.mime) || b.mime == mime.MimeTypeVP8 {
		frc := NewFrameRateCalculatorDD(b.clockRate, b.logger)
		for i := range b.frameRateCalculator {
			b.frameRateCalculator[i] = frc.GetFrameRateCalculatorForSpatial(int32(i))
		}
		b.ddParser = NewDependencyDescriptorParser(b.ddExtID, b.logger, func(spatial, temporal int32) {
			frc.SetMaxLayer(spatial, temporal)
		}, false)
	}
}

func (b *Buffer) createFrameRateCalculator() {
	switch b.mime {
	case mime.MimeTypeVP8:
		b.frameRateCalculator[0] = NewFrameRateCalculatorVP8(b.clockRate, b.logger)

	case mime.MimeTypeVP9:
		frc := NewFrameRateCalculatorVP9(b.clockRate, b.logger)
		for i := range b.frameRateCalculator {
			b.frameRateCalculator[i] = frc.GetFrameRateCalculatorForSpatial(int32(i))
		}

	case mime.MimeTypeH265:
		b.frameRateCalculator[0] = NewFrameRateCalculatorH26x(b.clockRate, b.logger)
	}
}

// Write adds an RTP Packet, ordering is not guaranteed, newer packets may arrive later
func (b *Buffer) Write(pkt []byte) (n int, err error) {
	var rtpPacket rtp.Packet
	err = rtpPacket.Unmarshal(pkt)
	if err != nil {
		return
	}

	b.Lock()
	if b.closed.Load() {
		b.Unlock()
		err = io.EOF
		return
	}

	now := mono.UnixNano()
	if b.twcc != nil && b.twccExtID != 0 && !b.closed.Load() {
		if ext := rtpPacket.GetExtension(b.twccExtID); ext != nil {
			b.twcc.Push(rtpPacket.SSRC, binary.BigEndian.Uint16(ext[0:2]), now, rtpPacket.Marker)
		}
	}

	// libwebrtc will use 0 ssrc for probing, don't push the packet to pending queue to avoid memory increasing since
	// the Bind will not be called to consume the pending packets. More details in https://github.com/pion/webrtc/pull/2816
	if rtpPacket.SSRC == 0 {
		b.Unlock()
		return
	}

	// handle RTX packet
	if pb := b.primaryBufferForRTX; pb != nil {
		b.Unlock()

		// skip padding only packets
		if rtpPacket.Padding && len(rtpPacket.Payload) == 0 {
			return
		}

		pb.writeRTX(&rtpPacket, now)
		return
	}

	if !b.bound {
		packet := make([]byte, len(pkt))
		copy(packet, pkt)

		startIdx := 0
		overflow := len(b.pPackets) - max(b.maxVideoPkts, b.maxAudioPkts)
		if overflow > 0 {
			startIdx = overflow
		}
		b.pPackets = append(b.pPackets[startIdx:], pendingPacket{
			packet:      packet,
			arrivalTime: now,
		})

		b.readCond.Broadcast()
		b.Unlock()
		return
	}

	b.calc(pkt, &rtpPacket, now, false)
	b.readCond.Broadcast()
	b.Unlock()
	return
}

func (b *Buffer) SetPrimaryBufferForRTX(primaryBuffer *Buffer) {
	b.Lock()
	b.primaryBufferForRTX = primaryBuffer
	pkts := b.pPackets
	b.pPackets = nil
	b.Unlock()
	for _, pp := range pkts {
		var rtpPacket rtp.Packet
		err := rtpPacket.Unmarshal(pp.packet)
		if err != nil {
			continue
		}
		if rtpPacket.Padding && len(rtpPacket.Payload) == 0 {
			continue
		}
		primaryBuffer.writeRTX(&rtpPacket, pp.arrivalTime)
	}
}

func (b *Buffer) writeRTX(rtxPkt *rtp.Packet, arrivalTime int64) (n int, err error) {
	b.Lock()
	defer b.Unlock()
	if !b.bound {
		return
	}

	if rtxPkt.PayloadType != b.rtxPayloadType {
		b.logger.Debugw("unexpected rtx payload type", "expected", b.rtxPayloadType, "actual", rtxPkt.PayloadType)
		return
	}

	if b.rtxPktBuf == nil {
		b.rtxPktBuf = make([]byte, bucket.MaxPktSize)
	}

	repairedPkt := *rtxPkt
	repairedPkt.PayloadType = b.payloadType
	repairedPkt.SequenceNumber = binary.BigEndian.Uint16(rtxPkt.Payload[:2])
	repairedPkt.SSRC = b.mediaSSRC
	repairedPkt.Payload = rtxPkt.Payload[2:]
	n, err = repairedPkt.MarshalTo(b.rtxPktBuf)
	if err != nil {
		b.logger.Errorw("could not marshal repaired packet", err, "ssrc", b.mediaSSRC, "sn", repairedPkt.SequenceNumber)
		return
	}

	b.calc(b.rtxPktBuf[:n], &repairedPkt, arrivalTime, true)
	return
}

func (b *Buffer) Read(buff []byte) (n int, err error) {
	b.Lock()
	for {
		if b.closed.Load() {
			b.Unlock()
			return 0, io.EOF
		}
		if b.pPackets != nil && len(b.pPackets) > b.lastPacketRead {
			if len(buff) < len(b.pPackets[b.lastPacketRead].packet) {
				b.Unlock()
				return 0, bucket.ErrBufferTooSmall
			}

			n = copy(buff, b.pPackets[b.lastPacketRead].packet)
			b.lastPacketRead++
			b.Unlock()
			return
		}
		b.readCond.Wait()
	}
}

func (b *Buffer) ReadExtended(buf []byte) (*ExtPacket, error) {
	b.Lock()
	for {
		if b.closed.Load() {
			b.Unlock()
			return nil, io.EOF
		}
		if b.extPackets.Len() > 0 {
			ep := b.extPackets.PopFront()
			ep = b.patchExtPacket(ep, buf)
			if ep == nil {
				continue
			}

			b.Unlock()
			return ep, nil
		}
		b.readCond.Wait()
	}
}

func (b *Buffer) Close() error {
	b.closeOnce.Do(func() {
		b.closed.Store(true)

		b.RLock()
		rtpStats := b.rtpStats
		b.readCond.Broadcast()
		b.RUnlock()

		if rtpStats != nil {
			rtpStats.Stop()
			b.logger.Debugw("rtp stats",
				"direction", "upstream",
				"stats", rtpStats,
			)
			if cb := b.getOnFinalRtpStats(); cb != nil {
				cb(rtpStats.ToProto())
			}
		}

		if cb := b.getOnClose(); cb != nil {
			cb()
		}
	})
	return nil
}

func (b *Buffer) OnClose(fn func()) {
	b.Lock()
	b.onClose = fn
	b.Unlock()
}

func (b *Buffer) getOnClose() func() {
	b.RLock()
	defer b.RUnlock()

	return b.onClose
}

func (b *Buffer) SetPLIThrottle(duration int64) {
	b.Lock()
	defer b.Unlock()

	b.pliThrottle = duration
}

func (b *Buffer) SendPLI(force bool) {
	b.RLock()
	rtpStats := b.rtpStats
	pliThrottle := b.pliThrottle
	b.RUnlock()

	if (rtpStats == nil && !force) || !rtpStats.CheckAndUpdatePli(pliThrottle, force) {
		return
	}

	b.logger.Debugw("send pli", "ssrc", b.mediaSSRC, "force", force)
	pli := []rtcp.Packet{
		&rtcp.PictureLossIndication{SenderSSRC: b.mediaSSRC, MediaSSRC: b.mediaSSRC},
	}

	if cb := b.getOnRtcpFeedback(); cb != nil {
		cb(pli)
	}
}

func (b *Buffer) SetRTT(rtt uint32) {
	b.Lock()
	defer b.Unlock()

	if rtt == 0 {
		return
	}

	if b.nacker != nil {
		b.nacker.SetRTT(rtt)
	}

	if b.rtpStats != nil {
		b.rtpStats.UpdateRtt(rtt)
	}
}

func (b *Buffer) calc(rawPkt []byte, rtpPacket *rtp.Packet, arrivalTime int64, isRTX bool) {
	defer func() {
		b.doNACKs()

		b.doReports(arrivalTime)
	}()

	if rtpPacket == nil {
		rtpPacket = &rtp.Packet{}
		if err := rtpPacket.Unmarshal(rawPkt); err != nil {
			b.logger.Errorw("could not unmarshal RTP packet", err)
			return
		}
	}

	// process header extensions always as padding packets could be used for probing
	b.processHeaderExtensions(rtpPacket, arrivalTime, isRTX)

	flowState := b.updateStreamState(rtpPacket, arrivalTime)
	if flowState.IsNotHandled {
		return
	}

	if len(rtpPacket.Payload) == 0 && (!flowState.IsOutOfOrder || flowState.IsDuplicate) {
		// drop padding only in-order or duplicate packet
		if !flowState.IsOutOfOrder {
			// in-order packet - increment sequence number offset for subsequent packets
			// Example:
			//   40 - regular packet - pass through as sequence number 40
			//   41 - missing packet - don't know what it is, could be padding or not
			//   42 - padding only packet - in-order - drop - increment sequence number offset to 1 -
			//        range[0, 42] = 0 offset
			//   41 - arrives out of order - get offset 0 from cache - passed through as sequence number 41
			//   43 - regular packet - offset = 1 (running offset) - passes through as sequence number 42
			//   44 - padding only - in order - drop - increment sequence number offset to 2
			//        range[0, 42] = 0 offset, range[43, 44] = 1 offset
			//   43 - regular packet - out of order + duplicate - offset = 1 from cache -
			//        adjusted sequence number is 42, will be dropped by RTX buffer AddPacket method as duplicate
			//   45 - regular packet - offset = 2 (running offset) - passed through with adjusted sequence number as 43
			//   44 - padding only - out-of-order + duplicate - dropped as duplicate
			//
			if err := b.snRangeMap.ExcludeRange(flowState.ExtSequenceNumber, flowState.ExtSequenceNumber+1); err != nil {
				b.logger.Errorw(
					"could not exclude range", err,
					"sn", rtpPacket.SequenceNumber,
					"esn", flowState.ExtSequenceNumber,
					"rtpStats", b.rtpStats,
					"snRangeMap", b.snRangeMap,
				)
			}
		}
		return
	}

	if !flowState.IsOutOfOrder && rtpPacket.PayloadType != b.payloadType && b.codecType == webrtc.RTPCodecTypeVideo {
		b.handleCodecChange(rtpPacket.PayloadType)
	}

	// add to RTX buffer using sequence number after accounting for dropped padding only packets
	snAdjustment, err := b.snRangeMap.GetValue(flowState.ExtSequenceNumber)
	if err != nil {
		b.logger.Errorw(
			"could not get sequence number adjustment", err,
			"sn", rtpPacket.SequenceNumber,
			"esn", flowState.ExtSequenceNumber,
			"payloadSize", len(rtpPacket.Payload),
			"rtpStats", b.rtpStats,
			"snRangeMap", b.snRangeMap,
		)
		return
	}
	flowState.ExtSequenceNumber -= snAdjustment
	rtpPacket.Header.SequenceNumber = uint16(flowState.ExtSequenceNumber)
	_, err = b.bucket.AddPacketWithSequenceNumber(rawPkt, flowState.ExtSequenceNumber)
	if err != nil {
		if !flowState.IsDuplicate {
			if errors.Is(err, bucket.ErrPacketTooOld) {
				packetTooOldCount := b.packetTooOldCount.Inc()
				if (packetTooOldCount-1)%100 == 0 {
					b.logger.Warnw(
						"could not add packet to bucket", err,
						"count", packetTooOldCount,
						"flowState", &flowState,
						"snAdjustment", snAdjustment,
						"incomingSequenceNumber", flowState.ExtSequenceNumber+snAdjustment,
						"rtpStats", b.rtpStats,
						"snRangeMap", b.snRangeMap,
					)
				}
			} else if err != bucket.ErrRTXPacket {
				b.logger.Warnw(
					"could not add packet to bucket", err,
					"flowState", &flowState,
					"snAdjustment", snAdjustment,
					"incomingSequenceNumber", flowState.ExtSequenceNumber+snAdjustment,
					"rtpStats", b.rtpStats,
					"snRangeMap", b.snRangeMap,
				)
			}
		}
		return
	}

	ep := b.getExtPacket(rtpPacket, arrivalTime, flowState)
	if ep == nil {
		return
	}
	b.extPackets.PushBack(ep)

	if b.extPackets.Len() > b.bucket.Capacity() {
		if (b.extPacketTooMuchCount.Inc()-1)%100 == 0 {
			b.logger.Warnw("too much ext packets", nil, "count", b.extPackets.Len())
		}
	}

	b.doFpsCalc(ep)
}

func (b *Buffer) patchExtPacket(ep *ExtPacket, buf []byte) *ExtPacket {
	n, err := b.getPacket(buf, ep.ExtSequenceNumber)
	if err != nil {
		packetNotFoundCount := b.packetNotFoundCount.Inc()
		if (packetNotFoundCount-1)%20 == 0 {
			b.logger.Warnw(
				"could not get packet from bucket", err,
				"sn", ep.Packet.SequenceNumber,
				"headSN", b.bucket.HeadSequenceNumber(),
				"count", packetNotFoundCount,
				"rtpStats", b.rtpStats,
				"snRangeMap", b.snRangeMap,
			)
		}
		return nil
	}
	ep.RawPacket = buf[:n]

	// patch RTP packet to point payload to new buffer
	pkt := *ep.Packet
	payloadStart := ep.Packet.Header.MarshalSize()
	payloadEnd := payloadStart + len(ep.Packet.Payload)
	if payloadEnd > n {
		b.logger.Warnw("unexpected marshal size", nil, "max", n, "need", payloadEnd)
		return nil
	}
	pkt.Payload = buf[payloadStart:payloadEnd]
	ep.Packet = &pkt

	return ep
}

func (b *Buffer) doFpsCalc(ep *ExtPacket) {
	if b.paused || b.frameRateCalculated || len(ep.Packet.Payload) == 0 {
		return
	}
	spatial := ep.Spatial
	if spatial < 0 || int(spatial) >= len(b.frameRateCalculator) {
		spatial = 0
	}
	if fr := b.frameRateCalculator[spatial]; fr != nil {
		if fr.RecvPacket(ep) {
			complete := true
			for _, fr2 := range b.frameRateCalculator {
				if fr2 != nil && !fr2.Completed() {
					complete = false
					break
				}
			}
			if complete {
				b.frameRateCalculated = true
				if f := b.onFpsChanged; f != nil {
					go f()
				}
			}
		}
	}
}

func (b *Buffer) handleCodecChange(newPT uint8) {
	var (
		codecFound, rtxFound bool
		rtxPt                uint8
		newCodec             webrtc.RTPCodecParameters
	)
	for _, codec := range b.rtpParameters.Codecs {
		if !codecFound && uint8(codec.PayloadType) == newPT {
			newCodec = codec
			codecFound = true
		}

		if mime.IsMimeTypeStringRTX(codec.MimeType) && strings.Contains(codec.SDPFmtpLine, fmt.Sprintf("apt=%d", newPT)) {
			rtxFound = true
			rtxPt = uint8(codec.PayloadType)
		}

		if codecFound && rtxFound {
			break
		}
	}
	if !codecFound {
		b.logger.Errorw("could not find codec for new payload type", nil, "pt", newPT, "rtpParameters", b.rtpParameters)
		return
	}
	b.logger.Infow(
		"codec changed",
		"oldPayload", b.payloadType, "newPayload", newPT,
		"oldRtxPayload", b.rtxPayloadType, "newRtxPayload", rtxPt,
		"oldMime", b.mime, "newMime", newCodec.MimeType)
	b.payloadType = newPT
	b.rtxPayloadType = rtxPt
	b.mime = mime.NormalizeMimeType(newCodec.MimeType)
	b.frameRateCalculated = false

	if b.ddExtID != 0 {
		b.createDDParserAndFrameRateCalculator()
	}

	if b.frameRateCalculator[0] == nil {
		b.createFrameRateCalculator()
	}

	b.bucket.ResyncOnNextPacket()

	if f := b.onCodecChange; f != nil {
		go f(newCodec)
	}

	if mime.IsMimeTypeVideo(b.mime) {
		go b.seedKeyFrame(b.keyFrameSeederGeneration.Inc())
	}
}

func (b *Buffer) updateStreamState(p *rtp.Packet, arrivalTime int64) rtpstats.RTPFlowState {
	flowState := b.rtpStats.Update(
		arrivalTime,
		p.Header.SequenceNumber,
		p.Header.Timestamp,
		p.Header.Marker,
		p.Header.MarshalSize(),
		len(p.Payload),
		int(p.PaddingSize),
	)

	if b.nacker != nil {
		b.nacker.Remove(p.SequenceNumber)

		for lost := flowState.LossStartInclusive; lost != flowState.LossEndExclusive; lost++ {
			b.nacker.Push(uint16(lost))
		}
	}

	return flowState
}

func (b *Buffer) processHeaderExtensions(p *rtp.Packet, arrivalTime int64, isRTX bool) {
	if b.audioLevelExtID != 0 && !isRTX {
		if !b.latestTSForAudioLevelInitialized {
			b.latestTSForAudioLevelInitialized = true
			b.latestTSForAudioLevel = p.Timestamp
		}
		if e := p.GetExtension(b.audioLevelExtID); e != nil {
			ext := rtp.AudioLevelExtension{}
			if err := ext.Unmarshal(e); err == nil {
				if (p.Timestamp - b.latestTSForAudioLevel) < (1 << 31) {
					duration := (int64(p.Timestamp) - int64(b.latestTSForAudioLevel)) * 1e3 / int64(b.clockRate)
					if duration > 0 {
						b.audioLevel.Observe(ext.Level, uint32(duration), arrivalTime)
					}

					b.latestTSForAudioLevel = p.Timestamp
				}
			}
		}
	}
}

func (b *Buffer) getExtPacket(rtpPacket *rtp.Packet, arrivalTime int64, flowState rtpstats.RTPFlowState) *ExtPacket {
	ep := &ExtPacket{
		Arrival:           arrivalTime,
		ExtSequenceNumber: flowState.ExtSequenceNumber,
		ExtTimestamp:      flowState.ExtTimestamp,
		Packet:            rtpPacket,
		VideoLayer: VideoLayer{
			Spatial:  InvalidLayerSpatial,
			Temporal: InvalidLayerTemporal,
		},
		IsOutOfOrder: flowState.IsOutOfOrder,
	}

	if len(rtpPacket.Payload) == 0 {
		// padding only packet, nothing else to do
		return ep
	}

	ep.Temporal = 0
	var videoSize []VideoSize
	if b.ddParser != nil {
		ddVal, videoLayer, err := b.ddParser.Parse(ep.Packet)
		if err != nil {
			if errors.Is(err, ErrDDExtentionNotFound) {
				if b.mime == mime.MimeTypeVP8 || b.mime == mime.MimeTypeVP9 {
					b.logger.Infow("dd extension not found,  disable dd parser")
					b.ddParser = nil
					b.createFrameRateCalculator()
				}
			} else {
				return nil
			}
		} else if ddVal != nil {
			ep.DependencyDescriptor = ddVal
			ep.VideoLayer = videoLayer
			videoSize = ExtractDependencyDescriptorVideoSize(ddVal.Descriptor)
			// DD-TODO : notify active decode target change if changed.
		}
	}

	switch b.mime {
	case mime.MimeTypeVP8:
		vp8Packet := VP8{}
		if err := vp8Packet.Unmarshal(rtpPacket.Payload); err != nil {
			b.logger.Warnw("could not unmarshal VP8 packet", err)
			return nil
		}
		ep.KeyFrame = vp8Packet.IsKeyFrame
		if ep.DependencyDescriptor == nil {
			ep.Temporal = int32(vp8Packet.TID)

			if ep.KeyFrame {
				if sz := ExtractVP8VideoSize(&vp8Packet, rtpPacket.Payload); sz.Width > 0 && sz.Height > 0 {
					videoSize = append(videoSize, sz)
				}
			}
		} else {
			// vp8 with DependencyDescriptor enabled, use the TID from the descriptor
			vp8Packet.TID = uint8(ep.Temporal)
		}
		ep.Payload = vp8Packet
		ep.Spatial = InvalidLayerSpatial // vp8 don't have spatial scalability, reset to invalid

	case mime.MimeTypeVP9:
		if ep.DependencyDescriptor == nil {
			var vp9Packet codecs.VP9Packet
			_, err := vp9Packet.Unmarshal(rtpPacket.Payload)
			if err != nil {
				b.logger.Warnw("could not unmarshal VP9 packet", err)
				return nil
			}
			ep.VideoLayer = VideoLayer{
				Spatial:  int32(vp9Packet.SID),
				Temporal: int32(vp9Packet.TID),
			}
			ep.Payload = vp9Packet
			ep.KeyFrame = IsVP9KeyFrame(&vp9Packet, rtpPacket.Payload)

			if ep.KeyFrame {
				for i := 0; i < len(vp9Packet.Width); i++ {
					videoSize = append(videoSize, VideoSize{
						Width:  uint32(vp9Packet.Width[i]),
						Height: uint32(vp9Packet.Height[i]),
					})
				}
			}
		} else {
			ep.KeyFrame = IsVP9KeyFrame(nil, rtpPacket.Payload)
		}

	case mime.MimeTypeH264:
		ep.KeyFrame = IsH264KeyFrame(rtpPacket.Payload)
		ep.Spatial = InvalidLayerSpatial // h.264 don't have spatial scalability, reset to invalid

		// Check H264 key frame video size
		if ep.KeyFrame {
			if sz := ExtractH264VideoSize(rtpPacket.Payload); sz.Width > 0 && sz.Height > 0 {
				videoSize = append(videoSize, sz)
			}
		}

	case mime.MimeTypeAV1:
		ep.KeyFrame = IsAV1KeyFrame(rtpPacket.Payload)

	case mime.MimeTypeH265:
		ep.KeyFrame = IsH265KeyFrame(rtpPacket.Payload)
		if ep.DependencyDescriptor == nil {
			if len(rtpPacket.Payload) < 2 {
				b.logger.Warnw("invalid H265 packet", nil)
				return nil
			}
			ep.VideoLayer = VideoLayer{
				Temporal: int32(rtpPacket.Payload[1]&0x07) - 1,
			}
			ep.Spatial = InvalidLayerSpatial

			if ep.KeyFrame {
				if sz := ExtractH265VideoSize(rtpPacket.Payload); sz.Width > 0 && sz.Height > 0 {
					videoSize = append(videoSize, sz)
				}
			}
		}
	}

	if ep.KeyFrame {
		if b.rtpStats != nil {
			b.rtpStats.UpdateKeyFrame(1)
		}
	}

	if b.absCaptureTimeExtID != 0 {
		extData := rtpPacket.GetExtension(b.absCaptureTimeExtID)

		var actExt act.AbsCaptureTime
		if err := actExt.Unmarshal(extData); err == nil {
			ep.AbsCaptureTimeExt = &actExt
		}
	}

	if len(videoSize) > 0 {
		b.checkVideoSizeChange(videoSize)
	}

	return ep
}

func (b *Buffer) doNACKs() {
	if b.nacker == nil {
		return
	}

	if r, numSeqNumsNacked := b.buildNACKPacket(); r != nil {
		if cb := b.onRtcpFeedback; cb != nil {
			cb(r)
		}
		if b.rtpStats != nil {
			b.rtpStats.UpdateNack(uint32(numSeqNumsNacked))
		}
	}
}

func (b *Buffer) doReports(arrivalTime int64) {
	if arrivalTime-b.lastReport < ReportDelta {
		return
	}

	b.lastReport = arrivalTime

	// RTCP reports
	pkts := b.getRTCP()
	if pkts != nil {
		if cb := b.onRtcpFeedback; cb != nil {
			cb(pkts)
		}
	}

	b.mayGrowBucket()
}

func (b *Buffer) mayGrowBucket() {
	cap := b.bucket.Capacity()
	maxPkts := b.maxVideoPkts
	if b.codecType == webrtc.RTPCodecTypeAudio {
		maxPkts = b.maxAudioPkts
	}
	if cap >= maxPkts {
		return
	}
	oldCap := cap
	if deltaInfo := b.rtpStats.DeltaInfo(b.ppsSnapshotId); deltaInfo != nil {
		duration := deltaInfo.EndTime.Sub(deltaInfo.StartTime)
		if duration > 500*time.Millisecond {
			pps := int(time.Duration(deltaInfo.Packets) * time.Second / duration)
			for pps > cap && cap < maxPkts {
				cap = b.bucket.Grow()
			}
			if cap > oldCap {
				b.logger.Debugw("grow bucket", "from", oldCap, "to", cap, "pps", pps)
			}
		}
	}
}

func (b *Buffer) buildNACKPacket() ([]rtcp.Packet, int) {
	if nacks, numSeqNumsNacked := b.nacker.Pairs(); len(nacks) > 0 {
		pkts := []rtcp.Packet{&rtcp.TransportLayerNack{
			SenderSSRC: b.mediaSSRC,
			MediaSSRC:  b.mediaSSRC,
			Nacks:      nacks,
		}}
		return pkts, numSeqNumsNacked
	}
	return nil, 0
}

func (b *Buffer) buildReceptionReport() *rtcp.ReceptionReport {
	if b.rtpStats == nil {
		return nil
	}

	proxyLoss := b.lastFractionLostToReport
	if b.codecType == webrtc.RTPCodecTypeAudio && !b.enableAudioLossProxying {
		proxyLoss = 0
	}

	return b.rtpStats.GetRtcpReceptionReport(b.mediaSSRC, proxyLoss, b.rrSnapshotId)
}

func (b *Buffer) SetSenderReportData(rtpTime uint32, ntpTime uint64, packets uint32, octets uint32) {
	b.RLock()
	srData := &livekit.RTCPSenderReportState{
		RtpTimestamp: rtpTime,
		NtpTimestamp: ntpTime,
		At:           mono.UnixNano(),
		Packets:      packets,
		Octets:       uint64(octets),
	}

	didSet := false
	if b.rtpStats != nil {
		didSet = b.rtpStats.SetRtcpSenderReportData(srData)
	}
	b.RUnlock()

	if didSet {
		if cb := b.getOnRtcpSenderReport(); cb != nil {
			cb()
		}
	}
}

func (b *Buffer) GetSenderReportData() *livekit.RTCPSenderReportState {
	b.RLock()
	defer b.RUnlock()

	if b.rtpStats != nil {
		return b.rtpStats.GetRtcpSenderReportData()
	}

	return nil
}

func (b *Buffer) SetLastFractionLostReport(lost uint8) {
	b.Lock()
	defer b.Unlock()

	b.lastFractionLostToReport = lost
}

func (b *Buffer) getRTCP() []rtcp.Packet {
	var pkts []rtcp.Packet

	rr := b.buildReceptionReport()
	if rr != nil {
		pkts = append(pkts, &rtcp.ReceiverReport{
			SSRC:    b.mediaSSRC,
			Reports: []rtcp.ReceptionReport{*rr},
		})
	}

	return pkts
}

func (b *Buffer) GetPacket(buff []byte, esn uint64) (int, error) {
	b.Lock()
	defer b.Unlock()

	return b.getPacket(buff, esn)
}

func (b *Buffer) getPacket(buff []byte, esn uint64) (int, error) {
	if b.closed.Load() {
		return 0, io.EOF
	}
	return b.bucket.GetPacket(buff, esn)
}

func (b *Buffer) OnRtcpFeedback(fn func(fb []rtcp.Packet)) {
	b.Lock()
	b.onRtcpFeedback = fn
	b.Unlock()
}

func (b *Buffer) getOnRtcpFeedback() func(fb []rtcp.Packet) {
	b.RLock()
	defer b.RUnlock()

	return b.onRtcpFeedback
}

func (b *Buffer) OnRtcpSenderReport(fn func()) {
	b.Lock()
	b.onRtcpSenderReport = fn
	b.Unlock()
}

func (b *Buffer) getOnRtcpSenderReport() func() {
	b.RLock()
	defer b.RUnlock()

	return b.onRtcpSenderReport
}

func (b *Buffer) OnFinalRtpStats(fn func(*livekit.RTPStats)) {
	b.Lock()
	b.onFinalRtpStats = fn
	b.Unlock()
}

func (b *Buffer) getOnFinalRtpStats() func(*livekit.RTPStats) {
	b.RLock()
	defer b.RUnlock()

	return b.onFinalRtpStats
}

// GetMediaSSRC returns the associated SSRC of the RTP stream
func (b *Buffer) GetMediaSSRC() uint32 {
	return b.mediaSSRC
}

// GetClockRate returns the RTP clock rate
func (b *Buffer) GetClockRate() uint32 {
	return b.clockRate
}

func (b *Buffer) GetStats() *livekit.RTPStats {
	b.RLock()
	defer b.RUnlock()

	if b.rtpStats == nil {
		return nil
	}

	return b.rtpStats.ToProto()
}

func (b *Buffer) GetDeltaStats() *StreamStatsWithLayers {
	b.RLock()
	defer b.RUnlock()

	if b.rtpStats == nil {
		return nil
	}

	deltaStats := b.rtpStats.DeltaInfo(b.deltaStatsSnapshotId)
	if deltaStats == nil {
		return nil
	}

	return &StreamStatsWithLayers{
		RTPStats: deltaStats,
		Layers: map[int32]*rtpstats.RTPDeltaInfo{
			0: deltaStats,
		},
	}
}

func (b *Buffer) GetLastSenderReportTime() time.Time {
	b.RLock()
	defer b.RUnlock()

	if b.rtpStats == nil {
		return time.Time{}
	}

	return b.rtpStats.LastSenderReportTime()
}

func (b *Buffer) GetAudioLevel() (float64, bool) {
	b.RLock()
	defer b.RUnlock()

	if b.audioLevel == nil {
		return 0, false
	}

	return b.audioLevel.GetLevel(mono.UnixNano())
}

func (b *Buffer) OnFpsChanged(f func()) {
	b.Lock()
	b.onFpsChanged = f
	b.Unlock()
}

func (b *Buffer) OnVideoSizeChanged(fn func([]VideoSize)) {
	b.Lock()
	b.onVideoSizeChanged = fn
	b.Unlock()
}

// checkVideoSizeChange checks if video size has changed for a specific spatial layer and fires callback
func (b *Buffer) checkVideoSizeChange(videoSizes []VideoSize) {
	if len(videoSizes) > len(b.currentVideoSize) {
		b.logger.Warnw("video size index out of range", nil, "newSize", videoSizes, "currentVideoSize", b.currentVideoSize)
		return
	}

	if len(videoSizes) < len(b.currentVideoSize) {
		videoSizes = append(videoSizes, make([]VideoSize, len(b.currentVideoSize)-len(videoSizes))...)
	}

	changed := false
	for i, sz := range videoSizes {
		if b.currentVideoSize[i].Width != sz.Width || b.currentVideoSize[i].Height != sz.Height {
			changed = true
			break
		}
	}

	if changed {
		b.logger.Debugw("video size changed", "from", b.currentVideoSize, "to", videoSizes)
		copy(b.currentVideoSize[:], videoSizes[:])
		if b.onVideoSizeChanged != nil {
			go b.onVideoSizeChanged(videoSizes)
		}
	}
}

func (b *Buffer) GetTemporalLayerFpsForSpatial(layer int32) []float32 {
	if int(layer) >= len(b.frameRateCalculator) {
		return nil
	}

	if fc := b.frameRateCalculator[layer]; fc != nil {
		return fc.GetFrameRate()
	}
	return nil
}

func (b *Buffer) seedKeyFrame(keyFrameSeederGeneration int32) {
	// a key frame is needed especially when using Dependency Descriptor
	// to get the DD structure which is used in parsing subsequent packets,
	// till then packets are dropped which results in stream tracker not
	// getting any data which means it does not declare layer start.
	//
	// send gratuitous PLIs for some time or until a key frame is seen to
	// get the engine rolling
	b.logger.Debugw("starting key frame seeder")
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	initialCount := uint32(0)
	b.RLock()
	rtpStats := b.rtpStats
	b.RUnlock()
	if rtpStats != nil {
		initialCount, _ = rtpStats.KeyFrame()
	}

	for {
		if b.closed.Load() || b.keyFrameSeederGeneration.Load() != keyFrameSeederGeneration {
			return
		}

		select {
		case <-timer.C:
			b.logger.Infow("stopping key frame seeder: timeout")
			return

		case <-ticker.C:
			if rtpStats != nil {
				cnt, last := rtpStats.KeyFrame()
				if cnt > initialCount {
					b.logger.Debugw(
						"stopping key frame seeder: received key frame",
						"keyFrameCountInitial", initialCount,
						"keyFrameCount", cnt,
						"lastKeyFrame", last,
					)
					return
				}

				b.SendPLI(false)
			}
		}
	}
}

// ---------------------------------------------------------------

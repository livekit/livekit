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
	"io"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	sutils "github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/mediatransportutil/pkg/bucket"
	"github.com/livekit/mediatransportutil/pkg/twcc"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils/mono"
)

const (
	rtcpReceiverReportDelta = 1e9

	InitPacketBufferSizeVideo = 300
	InitPacketBufferSizeAudio = 70
)

var (
	errInvalidCodec = errors.New("invalid codec")
)

var _ BufferProvider = (*Buffer)(nil)

type pendingPacket struct {
	arrivalTime int64
	packet      []byte
}

// Buffer contains all packets
type Buffer struct {
	*BufferBase

	pPackets     []pendingPacket
	lastReportAt int64
	isBound      bool

	/* RAJA-REMOVE
	rtpParameters  webrtc.RTPParameters
	payloadType    uint8
	rtxPayloadType uint8
	*/

	twcc      *twcc.Responder
	twccExtID uint8

	enableAudioLossProxying  bool
	lastFractionLostToReport uint8 // Last fraction lost from subscribers, should report to publisher; Audio only

	lastPacketRead int

	// RAJA-TODO rrSnapshotId         uint32
	// RAJA-TODO deltaStatsSnapshotId uint32

	// callbacks
	onClose        func()
	onRtcpFeedback func([]rtcp.Packet)
	// RAJA-TODO onRtcpSenderReport func()
	onFinalRtpStats func(*livekit.RTPStats)
	// RAJA-TODO onCodecChange   func(webrtc.RTPCodecParameters)

	primaryBufferForRTX *Buffer
	rtxPktBuf           []byte
}

func NewBuffer(ssrc uint32, maxVideoPkts, maxAudioPkts int) *Buffer {
	b := &Buffer{}
	b.BufferBase = NewBufferBase(
		ssrc,
		maxVideoPkts,
		maxAudioPkts,
		[]string{sutils.ComponentPub, sutils.ComponentSFU},
		b.sendPLI,
		true,
	)
	return b
}

func (b *Buffer) SetTWCCAndExtID(twcc *twcc.Responder, extID uint8) {
	b.Lock()
	defer b.Unlock()

	b.twcc = twcc
	b.twccExtID = extID
}

func (b *Buffer) SetAudioLossProxying(enable bool) {
	b.Lock()
	defer b.Unlock()

	b.enableAudioLossProxying = enable
}

// RAJA-TODO: somehow move a bunch of this to base
func (b *Buffer) Bind(params webrtc.RTPParameters, codec webrtc.RTPCodecCapability, bitrates int) error {
	b.Lock()
	defer b.Unlock()
	if b.isBound {
		return nil
	}

	/* RAJA-TODO
	b.logger.Debugw("binding track")
	if codec.ClockRate == 0 {
		b.logger.Warnw("invalid codec", nil, "params", params, "codec", codec, "bitrates", bitrates)
		return errInvalidCodec
	}

	b.setupRTPStats(codec.ClockRate)

	b.clockRate = codec.ClockRate
	b.lastReportAt = mono.UnixNano()
	b.mime = mime.NormalizeMimeType(codec.MimeType)
	b.rtpParameters = params
	for _, codecParameter := range params.Codecs {
		if mime.IsMimeTypeStringEqual(codecParameter.MimeType, codec.MimeType) {
			b.payloadType = uint8(codecParameter.PayloadType)
			break
		}
	}

	if b.payloadType == 0 && !mime.IsMimeTypeStringEqual(codec.MimeType, webrtc.MimeTypePCMU) {
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
		b.bucket = bucket.NewBucket[uint64, uint16](InitPacketBufferSizeAudio, bucket.RTPMaxPktSize, bucket.RTPSeqNumOffset)

	case mime.IsMimeTypeVideo(b.mime):
		b.codecType = webrtc.RTPCodecTypeVideo
		b.bucket = bucket.NewBucket[uint64, uint16](InitPacketBufferSizeVideo, bucket.RTPMaxPktSize, bucket.RTPSeqNumOffset)
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

	if len(b.pPackets) != 0 {
		b.logger.Debugw("releasing queued packets on bind", "count", len(b.pPackets))
	}
	for _, pp := range b.pPackets {
		b.calc(pp.packet, nil, pp.arrivalTime, true)
	}
	b.pPackets = nil
	b.isBound = true

	b.BufferBase.StartKeyFrameSeeder()
	*/

	if err := b.BufferBase.BindLocked(params, codec, bitrates); err != nil {
		return err
	}

	b.lastReportAt = mono.UnixNano()

	if len(b.pPackets) != 0 {
		b.logger.Debugw("releasing queued packets on bind", "count", len(b.pPackets))
	}
	for _, pp := range b.pPackets {
		b.calc(pp.packet, nil, pp.arrivalTime, true)
	}
	b.pPackets = nil

	b.isBound = true

	return nil
}

/* RAJA-TODO
func (b *Buffer) OnCodecChange(fn func(webrtc.RTPCodecParameters)) {
	b.Lock()
	b.onCodecChange = fn
	b.Unlock()
}
*/

// Write adds an RTP Packet, ordering is not guaranteed, newer packets may arrive later
//
//go:noinline
func (b *Buffer) Write(pkt []byte) (n int, err error) {
	var rtpPacket rtp.Packet
	err = rtpPacket.Unmarshal(pkt)
	if err != nil {
		return
	}

	b.Lock()
	if b.BufferBase.IsClosed() {
		b.Unlock()
		err = io.EOF
		return
	}

	now := mono.UnixNano()
	if b.twcc != nil && b.twccExtID != 0 {
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

	if !b.isBound {
		packet := make([]byte, len(pkt))
		copy(packet, pkt)

		if len(b.pPackets) == 0 {
			b.logger.Debugw("received first packet")
		}

		startIdx := 0
		overflow := len(b.pPackets) - max(b.maxVideoPkts, b.maxAudioPkts)
		if overflow > 0 {
			startIdx = overflow
		}
		b.pPackets = append(b.pPackets[startIdx:], pendingPacket{
			packet:      packet,
			arrivalTime: now,
		})

		b.BufferBase.NotifyRead()
		b.Unlock()
		return
	}

	b.calc(pkt, &rtpPacket, now, false)
	b.BufferBase.NotifyRead()
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

func (b *Buffer) writeRTX(rtxPkt *rtp.Packet, arrivalTime int64) {
	b.Lock()
	defer b.Unlock()
	if !b.isBound {
		return
	}

	if rtxPkt.PayloadType != b.rtxPayloadType {
		b.logger.Debugw("unexpected rtx payload type", "expected", b.rtxPayloadType, "actual", rtxPkt.PayloadType)
		return
	}

	if b.rtxPktBuf == nil {
		b.rtxPktBuf = make([]byte, bucket.RTPMaxPktSize)
	}

	if len(rtxPkt.Payload) < 2 {
		b.logger.Warnw("rtx payload too short", nil, "size", len(rtxPkt.Payload))
		return
	}

	repairedPkt := *rtxPkt
	repairedPkt.PayloadType = b.payloadType
	repairedPkt.SequenceNumber = binary.BigEndian.Uint16(rtxPkt.Payload[:2])
	repairedPkt.SSRC = b.mediaSSRC
	repairedPkt.Payload = rtxPkt.Payload[2:]
	n, err := repairedPkt.MarshalTo(b.rtxPktBuf)
	if err != nil {
		b.logger.Errorw("could not marshal repaired packet", err, "ssrc", b.mediaSSRC, "sn", repairedPkt.SequenceNumber)
		return
	}

	b.calc(b.rtxPktBuf[:n], &repairedPkt, arrivalTime, false)
	b.BufferBase.NotifyRead()
}

func (b *Buffer) Read(buff []byte) (n int, err error) {
	b.Lock()
	for {
		if b.BufferBase.IsClosed() {
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
		b.BufferBase.WaitRead()
	}
}

// RAJA-TODO: move to BufferBase?
func (b *Buffer) Close() error {
	/* RAJA-TODO
	b.closeOnce.Do(func() {
		b.BufferBase.Close()

		b.BufferBase.StopKeyFrameSeeder()

		b.RLock()
		rtpStats := b.rtpStats
		b.BufferBase.NotifyRead()
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

		go b.flushExtPackets()
	})
	*/
	stats, err := b.BufferBase.CloseWithReason("close")
	if err != nil {
		return err
	}

	if stats != nil {
		if cb := b.getOnFinalRtpStats(); cb != nil {
			cb(stats)
		}
	}

	if cb := b.getOnClose(); cb != nil {
		cb()
	}

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

func (b *Buffer) sendPLI() {
	pli := []rtcp.Packet{
		&rtcp.PictureLossIndication{SenderSSRC: b.mediaSSRC, MediaSSRC: b.mediaSSRC},
	}

	if cb := b.getOnRtcpFeedback(); cb != nil {
		cb(pli)
	}
}

func (b *Buffer) calc(rawPkt []byte, rtpPacket *rtp.Packet, arrivalTime int64, isBuffered bool) {
	rtpPacket, flowState, err := b.BufferBase.HandleIncomingPacketLocked(
		rawPkt,
		rtpPacket,
		arrivalTime,
		isBuffered,
		nil,
	)
	if err != nil {
		return
	}

	b.BufferBase.UpdateNACKStateLocked(rtpPacket.SequenceNumber, flowState)
	b.doNACKs()

	b.doReports(arrivalTime)

	/* RAJA-REMOVE
	if rtpPacket == nil {
		rtpPacket = &rtp.Packet{}
		if err := rtpPacket.Unmarshal(rawPkt); err != nil {
			b.logger.Errorw("could not unmarshal RTP packet", err)
			return
		}
	}

	b.BufferBase.ProcessAudioSsrcLevelHeaderExtensionLocked(rtpPacket, arrivalTime)

	isRestart := false
	flowState := b.updateStreamState(rtpPacket, arrivalTime)
	switch flowState.UnhandledReason {
	case rtpstats.RTPFlowUnhandledReasonNone:
	case rtpstats.RTPFlowUnhandledReasonRestart:
		if !b.enableStreamRestartDetection {
			return
		}

		b.BufferBase.StopKeyFrameSeeder()

		b.rtpStats.Stop()
		b.logger.Infow("stream restart - rtp stats", b.rtpStats)

		b.snRangeMap = utils.NewRangeMap[uint64, uint64](100)
		b.setupRTPStats(b.clockRate)
		b.bucket.ResyncOnNextPacket()
		if b.nacker != nil {
			b.nacker = nack.NewNACKQueue(nack.NackQueueParamsDefault)
		}
		b.flushExtPacketsLocked()

		flowState = b.updateStreamState(rtpPacket, arrivalTime)
		isRestart = true
	default:
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

	ep := b.getExtPacket(rtpPacket, arrivalTime, isBuffered, isRestart, flowState)
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
	*/
}

/* RAJA-TODO
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
		"oldMime", b.mime, "newMime", newCodec.MimeType,
	)
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

	b.BufferBase.StartKeyFrameSeeder()
}

func (b *Buffer) updateStreamState(p *rtp.Packet, arrivalTime int64) rtpstats.RTPFlowState {
	flowState := b.BufferBase.updateStreamState(p, arrivalTime)

	if b.nacker != nil {
		b.nacker.Remove(p.SequenceNumber)

		for lost := flowState.LossStartInclusive; lost != flowState.LossEndExclusive; lost++ {
			b.nacker.Push(uint16(lost))
		}
	}

	return flowState
}
*/

func (b *Buffer) doNACKs() {
	if r := b.buildNACKPacket(); r != nil {
		if cb := b.onRtcpFeedback; cb != nil {
			cb(r)
		}
	}
}

func (b *Buffer) buildNACKPacket() []rtcp.Packet {
	if nacks := b.BufferBase.GetNACKPairsLocked(); len(nacks) > 0 {
		pkts := []rtcp.Packet{&rtcp.TransportLayerNack{
			SenderSSRC: b.mediaSSRC,
			MediaSSRC:  b.mediaSSRC,
			Nacks:      nacks,
		}}
		return pkts
	}
	return nil
}

func (b *Buffer) doReports(arrivalTime int64) {
	if arrivalTime-b.lastReportAt < rtcpReceiverReportDelta {
		return
	}
	b.lastReportAt = arrivalTime

	// RTCP reports
	pkts := b.getRTCP()
	if pkts != nil {
		if cb := b.onRtcpFeedback; cb != nil {
			cb(pkts)
		}
	}
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

func (b *Buffer) buildReceptionReport() *rtcp.ReceptionReport {
	proxyLoss := b.lastFractionLostToReport
	if b.codecType == webrtc.RTPCodecTypeAudio && !b.enableAudioLossProxying {
		proxyLoss = 0
	}

	return b.BufferBase.GetRtcpReceptionReportLocked(proxyLoss)
}

func (b *Buffer) SetLastFractionLostReport(lost uint8) {
	b.Lock()
	defer b.Unlock()

	b.lastFractionLostToReport = lost
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

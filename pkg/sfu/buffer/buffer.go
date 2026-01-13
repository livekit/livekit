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

	twcc      *twcc.Responder
	twccExtID uint8

	enableAudioLossProxying  bool
	lastFractionLostToReport uint8 // Last fraction lost from subscribers, should report to publisher; Audio only

	lastPacketRead int

	// callbacks
	onClose         func()
	onRtcpFeedback  func([]rtcp.Packet)
	onFinalRtpStats func(*livekit.RTPStats)
	onNotifyRTX     func(uint32, uint32, string)

	primaryBufferForRTX *Buffer
	rtxPktBuf           []byte
}

func NewBuffer(ssrc uint32, maxVideoPkts, maxAudioPkts int) *Buffer {
	b := &Buffer{}
	b.BufferBase = NewBufferBase(BufferBaseParams{
		SSRC:               ssrc,
		MaxVideoPkts:       maxVideoPkts,
		MaxAudioPkts:       maxAudioPkts,
		LoggerComponents:   []string{sutils.ComponentPub, sutils.ComponentSFU},
		SendPLI:            b.sendPLI,
		IsReportingEnabled: true,
	})
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

func (b *Buffer) Bind(params webrtc.RTPParameters, codec webrtc.RTPCodecCapability, bitrates int) error {
	b.Lock()
	defer b.Unlock()
	if b.isBound {
		return nil
	}

	if err := b.BufferBase.BindLocked(params, codec, bitrates); err != nil {
		return err
	}

	b.lastReportAt = mono.UnixNano()

	if len(b.pPackets) != 0 {
		b.logger.Debugw("releasing queued packets on bind", "count", len(b.pPackets))
	}
	for _, pp := range b.pPackets {
		b.calc(pp.packet, nil, pp.arrivalTime, true, false)
	}
	b.pPackets = nil

	b.isBound = true

	return nil
}

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
		overflow := len(b.pPackets) - max(b.BufferBase.MaxVideoPkts(), b.BufferBase.MaxAudioPkts())
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

	b.calc(pkt, &rtpPacket, now, false, false)
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

func (b *Buffer) NotifyRTX(ssrc uint32, repairSSRC uint32, rsid string) {
	if onNotifyRTX := b.getOnNotifyRTX(); onNotifyRTX != nil {
		onNotifyRTX(ssrc, repairSSRC, rsid)
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
	repairedPkt.SSRC = b.BufferBase.SSRC()
	repairedPkt.Payload = rtxPkt.Payload[2:]
	n, err := repairedPkt.MarshalTo(b.rtxPktBuf)
	if err != nil {
		b.logger.Errorw("could not marshal repaired packet", err, "ssrc", b.BufferBase.SSRC(), "sn", repairedPkt.SequenceNumber)
		return
	}

	b.calc(b.rtxPktBuf[:n], &repairedPkt, arrivalTime, false, true)
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

func (b *Buffer) Close() error {
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
	ssrc := b.BufferBase.SSRC()
	if ssrc == 0 {
		return
	}

	b.logger.Debugw("send pli", "mediaSSRC", ssrc)
	pli := []rtcp.Packet{
		&rtcp.PictureLossIndication{
			SenderSSRC: ssrc,
			MediaSSRC:  ssrc,
		},
	}

	if cb := b.getOnRtcpFeedback(); cb != nil {
		cb(pli)
	}
}

func (b *Buffer) calc(rawPkt []byte, rtpPacket *rtp.Packet, arrivalTime int64, isBuffered bool, isRTX bool) {
	b.BufferBase.HandleIncomingPacketLocked(
		rawPkt,
		rtpPacket,
		arrivalTime,
		isBuffered,
		isRTX,
		nil,
		0,
	)

	b.doNACKs()

	b.doReports(arrivalTime)
}

func (b *Buffer) doNACKs() {
	if r := b.buildNACKPacket(); r != nil {
		if cb := b.onRtcpFeedback; cb != nil {
			cb(r)
		}
	}
}

func (b *Buffer) buildNACKPacket() []rtcp.Packet {
	if nacks := b.BufferBase.GetNACKPairsLocked(); len(nacks) > 0 {
		ssrc := b.BufferBase.SSRC()
		pkts := []rtcp.Packet{&rtcp.TransportLayerNack{
			SenderSSRC: ssrc,
			MediaSSRC:  ssrc,
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
			SSRC:    b.BufferBase.SSRC(),
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

func (b *Buffer) OnNotifyRTX(fn func(ssrc uint32, repairSSRC uint32, rsid string)) {
	b.Lock()
	b.onNotifyRTX = fn
	b.Unlock()
}

func (b *Buffer) getOnNotifyRTX() func(ssrc uint32, repairSSRC uint32, rsid string) {
	b.RLock()
	defer b.RUnlock()

	return b.onNotifyRTX
}

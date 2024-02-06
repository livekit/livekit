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
	"strings"
	"sync"
	"time"

	"github.com/gammazero/deque"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"
	"go.uber.org/atomic"

	"github.com/livekit/livekit-server/pkg/sfu/audio"
	dd "github.com/livekit/livekit-server/pkg/sfu/dependencydescriptor"
	"github.com/livekit/livekit-server/pkg/sfu/utils"
	sutils "github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/mediatransportutil"
	"github.com/livekit/mediatransportutil/pkg/bucket"
	"github.com/livekit/mediatransportutil/pkg/nack"
	"github.com/livekit/mediatransportutil/pkg/twcc"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

const (
	ReportDelta = time.Second
)

type pendingPacket struct {
	arrivalTime time.Time
	packet      []byte
}

type ExtPacket struct {
	VideoLayer
	Arrival              time.Time
	ExtSequenceNumber    uint64
	ExtTimestamp         uint64
	Packet               *rtp.Packet
	Payload              interface{}
	KeyFrame             bool
	RawPacket            []byte
	DependencyDescriptor *ExtDependencyDescriptor
}

// Buffer contains all packets
type Buffer struct {
	sync.RWMutex
	bucket        *bucket.Bucket
	nacker        *nack.NackQueue
	videoPool     *sync.Pool
	audioPool     *sync.Pool
	codecType     webrtc.RTPCodecType
	extPackets    deque.Deque[*ExtPacket]
	pPackets      []pendingPacket
	closeOnce     sync.Once
	mediaSSRC     uint32
	clockRate     uint32
	lastReport    time.Time
	twccExt       uint8
	audioLevelExt uint8
	bound         bool
	closed        atomic.Bool
	mime          string

	snRangeMap *utils.RangeMap[uint64, uint64]

	latestTSForAudioLevelInitialized bool
	latestTSForAudioLevel            uint32

	twcc             *twcc.Responder
	audioLevelParams audio.AudioLevelParams
	audioLevel       *audio.AudioLevel

	lastPacketRead int

	pliThrottle int64

	rtpStats             *RTPStatsReceiver
	rrSnapshotId         uint32
	deltaStatsSnapshotId uint32

	lastFractionLostToReport uint8 // Last fraction lost from subscribers, should report to publisher; Audio only

	// callbacks
	onClose            func()
	onRtcpFeedback     func([]rtcp.Packet)
	onRtcpSenderReport func()
	onFpsChanged       func()
	onFinalRtpStats    func(*livekit.RTPStats)

	// logger
	logger logger.Logger

	// dependency descriptor
	ddExt    uint8
	ddParser *DependencyDescriptorParser

	paused              bool
	frameRateCalculator [DefaultMaxLayerSpatial + 1]FrameRateCalculator
	frameRateCalculated bool

	packetNotFoundCount   atomic.Uint32
	packetTooOldCount     atomic.Uint32
	extPacketTooMuchCount atomic.Uint32
}

// NewBuffer constructs a new Buffer
func NewBuffer(ssrc uint32, vp, ap *sync.Pool) *Buffer {
	l := logger.GetLogger() // will be reset with correct context via SetLogger
	b := &Buffer{
		mediaSSRC:   ssrc,
		videoPool:   vp,
		audioPool:   ap,
		snRangeMap:  utils.NewRangeMap[uint64, uint64](100),
		pliThrottle: int64(500 * time.Millisecond),
		logger:      l.WithComponent(sutils.ComponentPub).WithComponent(sutils.ComponentSFU),
	}
	b.extPackets.SetMinCapacity(7)
	return b
}

func (b *Buffer) SetLogger(logger logger.Logger) {
	b.Lock()
	defer b.Unlock()

	b.logger = logger.WithComponent(sutils.ComponentSFU)
	if b.rtpStats != nil {
		b.rtpStats.SetLogger(b.logger)
	}
}

func (b *Buffer) SetPaused(paused bool) {
	b.Lock()
	defer b.Unlock()

	b.paused = paused
}

func (b *Buffer) SetTWCC(twcc *twcc.Responder) {
	b.Lock()
	defer b.Unlock()

	b.twcc = twcc
}

func (b *Buffer) SetAudioLevelParams(audioLevelParams audio.AudioLevelParams) {
	b.Lock()
	defer b.Unlock()

	b.audioLevelParams = audioLevelParams
}

func (b *Buffer) Bind(params webrtc.RTPParameters, codec webrtc.RTPCodecCapability) {
	b.Lock()
	defer b.Unlock()
	if b.bound {
		return
	}

	b.rtpStats = NewRTPStatsReceiver(RTPStatsParams{
		ClockRate: codec.ClockRate,
		Logger:    b.logger,
	})
	b.rrSnapshotId = b.rtpStats.NewSnapshotId()
	b.deltaStatsSnapshotId = b.rtpStats.NewSnapshotId()

	b.clockRate = codec.ClockRate
	b.lastReport = time.Now()
	b.mime = strings.ToLower(codec.MimeType)

	for _, ext := range params.HeaderExtensions {
		switch ext.URI {
		case dd.ExtensionURI:
			b.ddExt = uint8(ext.ID)
			frc := NewFrameRateCalculatorDD(b.clockRate, b.logger)
			for i := range b.frameRateCalculator {
				b.frameRateCalculator[i] = frc.GetFrameRateCalculatorForSpatial(int32(i))
			}
			b.ddParser = NewDependencyDescriptorParser(b.ddExt, b.logger, func(spatial, temporal int32) {
				frc.SetMaxLayer(spatial, temporal)
			})

		case sdp.AudioLevelURI:
			b.audioLevelExt = uint8(ext.ID)
			b.audioLevel = audio.NewAudioLevel(b.audioLevelParams)
		}
	}

	switch {
	case strings.HasPrefix(b.mime, "audio/"):
		b.codecType = webrtc.RTPCodecTypeAudio
		b.bucket = bucket.NewBucket(b.audioPool.Get().(*[]byte))
	case strings.HasPrefix(b.mime, "video/"):
		b.codecType = webrtc.RTPCodecTypeVideo
		b.bucket = bucket.NewBucket(b.videoPool.Get().(*[]byte))
		if b.frameRateCalculator[0] == nil {
			if strings.EqualFold(codec.MimeType, webrtc.MimeTypeVP8) {
				b.frameRateCalculator[0] = NewFrameRateCalculatorVP8(b.clockRate, b.logger)
			}

			if strings.EqualFold(codec.MimeType, webrtc.MimeTypeVP9) {
				frc := NewFrameRateCalculatorVP9(b.clockRate, b.logger)
				for i := range b.frameRateCalculator {
					b.frameRateCalculator[i] = frc.GetFrameRateCalculatorForSpatial(int32(i))
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
		case webrtc.TypeRTCPFBTransportCC:
			if b.codecType == webrtc.RTPCodecTypeVideo {
				b.logger.Debugw("Setting feedback", "type", webrtc.TypeRTCPFBTransportCC)
				for _, ext := range params.HeaderExtensions {
					if ext.URI == sdp.TransportCCURI {
						b.twccExt = uint8(ext.ID)
						break
					}
				}
			}
		case webrtc.TypeRTCPFBNACK:
			// pion use a single mediaengine to manage negotiated codecs of peerconnection, that means we can't have different
			// codec settings at track level for same codec type, so enable nack for all audio receivers but don't create nack queue
			// for red codec.
			if strings.EqualFold(b.mime, "audio/red") {
				break
			}
			b.logger.Debugw("Setting feedback", "type", webrtc.TypeRTCPFBNACK)
			b.nacker = nack.NewNACKQueue(nack.NackQueueParamsDefault)
		}
	}

	for _, pp := range b.pPackets {
		b.calc(pp.packet, pp.arrivalTime)
	}
	b.pPackets = nil
	b.bound = true
}

// Write adds an RTP Packet, ordering is not guaranteed, newer packets may arrive later
func (b *Buffer) Write(pkt []byte) (n int, err error) {
	b.Lock()
	defer b.Unlock()

	if b.closed.Load() {
		err = io.EOF
		return
	}

	if !b.bound {
		packet := make([]byte, len(pkt))
		copy(packet, pkt)
		b.pPackets = append(b.pPackets, pendingPacket{
			packet:      packet,
			arrivalTime: time.Now(),
		})
		return
	}

	b.calc(pkt, time.Now())
	return
}

func (b *Buffer) Read(buff []byte) (n int, err error) {
	for {
		if b.closed.Load() {
			err = io.EOF
			return
		}
		b.Lock()
		if b.pPackets != nil && len(b.pPackets) > b.lastPacketRead {
			if len(buff) < len(b.pPackets[b.lastPacketRead].packet) {
				err = bucket.ErrBufferTooSmall
				b.Unlock()
				return
			}
			n = len(b.pPackets[b.lastPacketRead].packet)
			copy(buff, b.pPackets[b.lastPacketRead].packet)
			b.lastPacketRead++
			b.Unlock()
			return
		}
		b.Unlock()
		time.Sleep(25 * time.Millisecond)
	}
}

func (b *Buffer) ReadExtended(buf []byte) (*ExtPacket, error) {
	for {
		if b.closed.Load() {
			return nil, io.EOF
		}
		b.Lock()
		if b.extPackets.Len() > 0 {
			ep := b.extPackets.PopFront()
			ep = b.patchExtPacket(ep, buf)
			if ep == nil {
				b.Unlock()
				continue
			}

			b.Unlock()
			return ep, nil
		}
		b.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
}

func (b *Buffer) Close() error {
	b.Lock()
	defer b.Unlock()

	b.closeOnce.Do(func() {
		if b.bucket != nil && b.codecType == webrtc.RTPCodecTypeVideo {
			b.videoPool.Put(b.bucket.Src())
		}
		if b.bucket != nil && b.codecType == webrtc.RTPCodecTypeAudio {
			b.audioPool.Put(b.bucket.Src())
		}

		b.closed.Store(true)

		if b.rtpStats != nil {
			b.rtpStats.Stop()
			b.logger.Debugw("rtp stats",
				"direction", "upstream",
				"stats", b.rtpStats,
			)
			if b.onFinalRtpStats != nil {
				b.onFinalRtpStats(b.rtpStats.ToProto())
			}
		}

		if b.onClose != nil {
			b.onClose()
		}
	})
	return nil
}

func (b *Buffer) OnClose(fn func()) {
	b.onClose = fn
}

func (b *Buffer) SetPLIThrottle(duration int64) {
	b.Lock()
	defer b.Unlock()

	b.pliThrottle = duration
}

func (b *Buffer) SendPLI(force bool) {
	b.RLock()
	if (b.rtpStats == nil || b.rtpStats.TimeSinceLastPli() < b.pliThrottle) && !force {
		b.RUnlock()
		return
	}

	b.rtpStats.UpdatePliAndTime(1)
	b.RUnlock()

	b.logger.Debugw("send pli", "ssrc", b.mediaSSRC, "force", force)
	pli := []rtcp.Packet{
		&rtcp.PictureLossIndication{SenderSSRC: b.mediaSSRC, MediaSSRC: b.mediaSSRC},
	}

	if b.onRtcpFeedback != nil {
		b.onRtcpFeedback(pli)
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

func (b *Buffer) calc(pkt []byte, arrivalTime time.Time) {
	defer func() {
		b.doNACKs()

		b.doReports(arrivalTime)
	}()

	var rtpPacket rtp.Packet
	if err := rtpPacket.Unmarshal(pkt); err != nil {
		b.logger.Errorw("could not unmarshal RTP packet", err)
		return
	}

	// process header extensions always as padding packets could be used for probing
	b.processHeaderExtensions(&rtpPacket, arrivalTime)

	flowState := b.updateStreamState(&rtpPacket, arrivalTime)
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
				b.logger.Errorw("could not exclude range", err, "sn", rtpPacket.SequenceNumber, "esn", flowState.ExtSequenceNumber)
			}
		}
		// TODO-VP9-DEBUG-REMOVE-START
		snAdjustment, err := b.snRangeMap.GetValue(flowState.ExtSequenceNumber)
		b.logger.Debugw("dropping padding packet", "sn", rtpPacket.SequenceNumber, "osn", flowState.ExtSequenceNumber, "msn", flowState.ExtSequenceNumber-snAdjustment, "error", err)
		// TODO-VP9-DEBUG-REMOVE-END
		return
	}

	// add to RTX buffer using sequence number after accounting for dropped padding only packets
	snAdjustment, err := b.snRangeMap.GetValue(flowState.ExtSequenceNumber)
	if err != nil {
		b.logger.Errorw("could not get sequence number adjustment", err, "sn", flowState.ExtSequenceNumber, "payloadSize", len(rtpPacket.Payload))
		return
	}
	flowState.ExtSequenceNumber -= snAdjustment
	rtpPacket.Header.SequenceNumber = uint16(flowState.ExtSequenceNumber)
	_, err = b.bucket.AddPacketWithSequenceNumber(pkt, rtpPacket.Header.SequenceNumber)
	if err != nil {
		if errors.Is(err, bucket.ErrPacketTooOld) {
			packetTooOldCount := b.packetTooOldCount.Inc()
			if (packetTooOldCount-1)%100 == 0 {
				b.logger.Warnw("could not add packet to bucket", err, "count", packetTooOldCount)
			}
		} else if err != bucket.ErrRTXPacket {
			b.logger.Warnw("could not add packet to bucket", err)
		}
		return
	}

	ep := b.getExtPacket(&rtpPacket, arrivalTime, flowState)
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
	n, err := b.getPacket(buf, ep.Packet.SequenceNumber)
	if err != nil {
		packetNotFoundCount := b.packetNotFoundCount.Inc()
		if packetNotFoundCount%20 == 0 {
			b.logger.Warnw("could not get packet from bucket", err, "sn", ep.Packet.SequenceNumber, "headSN", b.bucket.HeadSequenceNumber(), "count", packetNotFoundCount)
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

func (b *Buffer) updateStreamState(p *rtp.Packet, arrivalTime time.Time) RTPFlowState {
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

		if flowState.HasLoss {
			for lost := flowState.LossStartInclusive; lost != flowState.LossEndExclusive; lost++ {
				b.nacker.Push(uint16(lost))
			}
		}
	}

	return flowState
}

func (b *Buffer) processHeaderExtensions(p *rtp.Packet, arrivalTime time.Time) {
	// submit to TWCC even if it is a padding only packet. Clients use padding only packets as probes
	// for bandwidth estimation
	if b.twcc != nil && b.twccExt != 0 {
		if ext := p.GetExtension(b.twccExt); ext != nil {
			b.twcc.Push(binary.BigEndian.Uint16(ext[0:2]), arrivalTime.UnixNano(), p.Marker)
		}
	}

	if b.audioLevelExt != 0 {
		if !b.latestTSForAudioLevelInitialized {
			b.latestTSForAudioLevelInitialized = true
			b.latestTSForAudioLevel = p.Timestamp
		}
		if e := p.GetExtension(b.audioLevelExt); e != nil {
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

func (b *Buffer) getExtPacket(rtpPacket *rtp.Packet, arrivalTime time.Time, flowState RTPFlowState) *ExtPacket {
	ep := &ExtPacket{
		Arrival:           arrivalTime,
		ExtSequenceNumber: flowState.ExtSequenceNumber,
		ExtTimestamp:      flowState.ExtTimestamp,
		Packet:            rtpPacket,
		VideoLayer: VideoLayer{
			Spatial:  InvalidLayerSpatial,
			Temporal: InvalidLayerTemporal,
		},
	}

	if len(rtpPacket.Payload) == 0 {
		// padding only packet, nothing else to do
		return ep
	}

	ep.Temporal = 0
	if b.ddParser != nil {
		ddVal, videoLayer, err := b.ddParser.Parse(ep.Packet)
		if err != nil {
			return nil
		} else if ddVal != nil {
			ep.DependencyDescriptor = ddVal
			ep.VideoLayer = videoLayer
			// DD-TODO : notify active decode target change if changed.
		}
	}
	switch b.mime {
	case "video/vp8":
		vp8Packet := VP8{}
		if err := vp8Packet.Unmarshal(rtpPacket.Payload); err != nil {
			b.logger.Warnw("could not unmarshal VP8 packet", err)
			return nil
		}
		ep.KeyFrame = vp8Packet.IsKeyFrame
		if ep.DependencyDescriptor == nil {
			ep.Temporal = int32(vp8Packet.TID)
		} else {
			// vp8 with DependencyDescriptor enabled, use the TID from the descriptor
			vp8Packet.TID = uint8(ep.Temporal)
			ep.Spatial = InvalidLayerSpatial // vp8 don't have spatial scalability, reset to invalid
		}
		ep.Payload = vp8Packet
	case "video/vp9":
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
		}
		ep.KeyFrame = IsVP9KeyFrame(rtpPacket.Payload)
	case "video/h264":
		ep.KeyFrame = IsH264KeyFrame(rtpPacket.Payload)
	case "video/av1":
		ep.KeyFrame = IsAV1KeyFrame(rtpPacket.Payload)
	}

	if ep.KeyFrame {
		if b.rtpStats != nil {
			b.rtpStats.UpdateKeyFrame(1)
		}
	}

	return ep
}

func (b *Buffer) doNACKs() {
	if b.nacker == nil {
		return
	}

	if r, numSeqNumsNacked := b.buildNACKPacket(); r != nil {
		if b.onRtcpFeedback != nil {
			b.onRtcpFeedback(r)
		}
		if b.rtpStats != nil {
			b.rtpStats.UpdateNack(uint32(numSeqNumsNacked))
		}
	}
}

func (b *Buffer) doReports(arrivalTime time.Time) {
	if time.Since(b.lastReport) < ReportDelta {
		return
	}

	b.lastReport = arrivalTime

	// RTCP reports
	pkts := b.getRTCP()
	if pkts != nil && b.onRtcpFeedback != nil {
		b.onRtcpFeedback(pkts)
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

	return b.rtpStats.GetRtcpReceptionReport(b.mediaSSRC, b.lastFractionLostToReport, b.rrSnapshotId)
}

func (b *Buffer) SetSenderReportData(rtpTime uint32, ntpTime uint64) {
	b.RLock()
	srData := &RTCPSenderReportData{
		RTPTimestamp: rtpTime,
		NTPTimestamp: mediatransportutil.NtpTime(ntpTime),
		At:           time.Now(),
	}

	if b.rtpStats != nil {
		b.rtpStats.SetRtcpSenderReportData(srData)
	}
	b.RUnlock()

	if b.onRtcpSenderReport != nil {
		b.onRtcpSenderReport()
	}
}

func (b *Buffer) GetSenderReportData() (*RTCPSenderReportData, *RTCPSenderReportData) {
	b.RLock()
	defer b.RUnlock()

	if b.rtpStats != nil {
		return b.rtpStats.GetRtcpSenderReportData()
	}

	return nil, nil
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

func (b *Buffer) GetPacket(buff []byte, sn uint16) (int, error) {
	b.Lock()
	defer b.Unlock()

	return b.getPacket(buff, sn)
}

func (b *Buffer) getPacket(buff []byte, sn uint16) (int, error) {
	if b.closed.Load() {
		return 0, io.EOF
	}
	return b.bucket.GetPacket(buff, sn)
}

func (b *Buffer) OnRtcpFeedback(fn func(fb []rtcp.Packet)) {
	b.onRtcpFeedback = fn
}

func (b *Buffer) OnRtcpSenderReport(fn func()) {
	b.onRtcpSenderReport = fn
}

func (b *Buffer) OnFinalRtpStats(fn func(*livekit.RTPStats)) {
	b.onFinalRtpStats = fn
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
		Layers: map[int32]*RTPDeltaInfo{
			0: deltaStats,
		},
	}
}

func (b *Buffer) GetAudioLevel() (float64, bool) {
	b.RLock()
	defer b.RUnlock()

	if b.audioLevel == nil {
		return 0, false
	}

	return b.audioLevel.GetLevel(time.Now())
}

func (b *Buffer) OnFpsChanged(f func()) {
	b.Lock()
	b.onFpsChanged = f
	b.Unlock()
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

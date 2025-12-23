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
	"github.com/livekit/mediatransportutil/pkg/bucket"
	"github.com/livekit/mediatransportutil/pkg/nack"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/mono"
)

var (
	ExtPacketFactory = &sync.Pool{
		New: func() any {
			return &ExtPacket{}
		},
	}
)

func ReleaseExtPacket(extPkt *ExtPacket) {
	if extPkt == nil {
		return
	}

	ReleaseExtDependencyDescriptor(extPkt.DependencyDescriptor)

	*extPkt = ExtPacket{}
	ExtPacketFactory.Put(extPkt)
}

// --------------------------------------

type ExtPacket struct {
	VideoLayer
	Arrival              int64
	ExtSequenceNumber    uint64
	ExtTimestamp         uint64
	Packet               *rtp.Packet
	Payload              any
	KeyFrame             bool
	RawPacket            []byte
	DependencyDescriptor *ExtDependencyDescriptor
	AbsCaptureTimeExt    *act.AbsCaptureTime
	IsOutOfOrder         bool
	IsBuffered           bool
	IsRestart            bool
}

// VideoSize represents video resolution
type VideoSize struct {
	Width  uint32
	Height uint32
}

type BufferProvider interface {
	SetLogger(lgr logger.Logger)
	SetAudioLevelParams(audioLevelParams audio.AudioLevelParams)
	SetStreamRestartDetection(enable bool)
	SetPLIThrottle(duration int64)
	SetRTT(rtt uint32)
	SetPaused(paused bool)
	GetPacket(buf []byte, esn uint64) (int, error)
	GetAudioLevel() (float64, bool)
	GetTemporalLayerFpsForSpatial(layer int32) []float32
	SendPLI(force bool)
	GetStats() *livekit.RTPStats
	GetDeltaStats() *StreamStatsWithLayers
	GetLastSenderReportTime() time.Time
	ReadExtended(buf []byte) (*ExtPacket, error)
	GetSenderReportData() *livekit.RTCPSenderReportState
	OnRtcpSenderReport(fn func())
	OnVideoSizeChanged(fn func([]VideoSize))
	StartKeyFrameSeeder()
	StopKeyFrameSeeder()
	CloseWithReason(reason string) (*livekit.RTPStats, error)
}

const (
	bucketCapCheckInterval = 1e9
)

type BufferBase struct {
	sync.RWMutex

	readCond *sync.Cond

	maxVideoPkts         int
	maxAudioPkts         int
	bucket               *bucket.Bucket[uint64, uint16]
	lastBucketCapCheckAt int64

	nacker              *nack.NackQueue
	isOOBNACK           bool // RAJA-TODO: set this properly from relay
	rtpStatsLite        *rtpstats.RTPStatsReceiverLite
	liteStatsSnapshotId uint32

	extPackets deque.Deque[*ExtPacket]

	codecType webrtc.RTPCodecType // RAJA-TODO? - maybe consolidate all this into codec webrtc.Codec
	closeOnce sync.Once           // RAJA-TODO use this in relayBuffer also?
	mediaSSRC uint32
	clockRate uint32 // RAJA-TODO - this needs to be populated properly
	mime      mime.MimeType

	// RAJA-REMOVE bound           bool

	rtpParameters  webrtc.RTPParameters
	payloadType    uint8
	rtxPayloadType uint8

	snRangeMap *utils.RangeMap[uint64, uint64]

	// RAJA-REMOVE twcc                         *twcc.Responder
	audioLevelParams                 audio.AudioLevelParams
	audioLevel                       *audio.AudioLevel
	audioLevelExtID                  uint8
	latestTSForAudioLevelInitialized bool
	latestTSForAudioLevel            uint32

	enableStreamRestartDetection bool

	pliThrottle int64
	sendPLI     func()

	isReportingEnabled   bool
	rtpStats             *rtpstats.RTPStatsReceiver
	ppsSnapshotId        uint32
	rrSnapshotId         uint32
	deltaStatsSnapshotId uint32

	// callbacks
	// RAJA-TODO onClose            func()
	onRtcpSenderReport func()
	onFpsChanged       func()
	onVideoSizeChanged func([]VideoSize)
	onCodecChange      func(webrtc.RTPCodecParameters)

	// video size tracking for multiple spatial layers
	currentVideoSize [DefaultMaxLayerSpatial + 1]VideoSize

	loggerComponents []string
	logger           logger.Logger

	// dependency descriptor
	ddExtID  uint8
	ddParser *DependencyDescriptorParser

	isPaused            bool
	frameRateCalculator [DefaultMaxLayerSpatial + 1]FrameRateCalculator
	frameRateCalculated bool

	packetNotFoundCount   atomic.Uint32
	packetTooOldCount     atomic.Uint32
	extPacketTooMuchCount atomic.Uint32

	absCaptureTimeExtID uint8

	keyFrameSeederGeneration atomic.Int32

	isClosed atomic.Bool
}

func NewBufferBase(
	ssrc uint32,
	maxVideoPkts int,
	maxAudioPkts int,
	loggerComponents []string,
	sendPLI func(),
	isReportingEnabled bool,
	isOOBNACK bool,
) *BufferBase {
	l := logger.GetLogger() // will be reset with correct context via SetLogger
	for _, component := range loggerComponents {
		l = l.WithComponent(component)
	}
	l = l.WithValues("ssrc", ssrc)

	b := &BufferBase{
		mediaSSRC:            ssrc,
		maxVideoPkts:         maxVideoPkts,
		maxAudioPkts:         maxAudioPkts,
		lastBucketCapCheckAt: mono.UnixNano(),
		snRangeMap:           utils.NewRangeMap[uint64, uint64](100),
		pliThrottle:          int64(500 * time.Millisecond),
		sendPLI:              sendPLI,
		isReportingEnabled:   isReportingEnabled,
		isOOBNACK:            isOOBNACK,
		loggerComponents:     loggerComponents,
		logger:               l,
	}
	b.readCond = sync.NewCond(&b.RWMutex)
	b.extPackets.SetBaseCap(128)
	return b
}

func (b *BufferBase) SetLogger(lgr logger.Logger) {
	b.Lock()
	defer b.Unlock()

	for _, component := range b.loggerComponents {
		lgr = lgr.WithComponent(component)
	}
	lgr = lgr.WithValues("ssrc", b.mediaSSRC)
	b.logger = lgr

	if b.rtpStats != nil {
		b.rtpStats.SetLogger(b.logger)
	}

	if b.rtpStatsLite != nil {
		b.rtpStatsLite.SetLogger(b.logger)
	}
}

func (b *BufferBase) BindLocked(params webrtc.RTPParameters, codec webrtc.RTPCodecCapability, bitrates int) error {
	b.logger.Debugw("binding track")
	if codec.ClockRate == 0 {
		b.logger.Warnw("invalid codec", nil, "params", params, "codec", codec, "bitrates", bitrates)
		return errInvalidCodec
	}

	b.setupRTPStats(codec.ClockRate)

	b.clockRate = codec.ClockRate
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
			// pion uses a single mediaengine to manage negotiated codecs of peerconnection, that means we can't have different
			// codec settings at track level for same codec type, so enable nack for all audio receivers but don't create nack queue
			// for red codec.
			if b.mime == mime.MimeTypeRED {
				break
			}
			b.logger.Debugw("Setting feedback", "type", webrtc.TypeRTCPFBNACK)
			b.nacker = nack.NewNACKQueue(nack.NackQueueParamsDefault)
		}
	}

	b.StartKeyFrameSeeder()

	return nil
}

func (b *BufferBase) CloseWithReason(reason string) (stats *livekit.RTPStats, err error) {
	b.closeOnce.Do(func() {
		b.isClosed.Store(true)

		b.StopKeyFrameSeeder()

		b.RLock()
		rtpStats := b.rtpStats
		rtpStatsLite := b.rtpStatsLite
		b.readCond.Broadcast()
		b.RUnlock()

		if rtpStats != nil {
			rtpStats.Stop()
		}
		if rtpStatsLite != nil {
			rtpStatsLite.Stop()
		}

		b.logger.Debugw(
			"rtp stats",
			"direction", "upstream",
			"stats", rtpStats,
			"statsLite", rtpStatsLite,
			"reason", reason,
		)
		stats = rtpStats.ToProto()

		go b.flushExtPackets()
	})
	return
}

func (b *BufferBase) IsClosed() bool {
	return b.isClosed.Load()
}

func (b *BufferBase) IsReceiving() bool {
	b.Lock()
	defer b.Unlock()

	if b.rtpStats != nil {
		return b.rtpStats.IsActive()
	}

	return true
}

// RAJA-TODO - set this properly in relay buffer too
func (b *BufferBase) SetPaused(paused bool) {
	b.Lock()
	defer b.Unlock()

	b.isPaused = paused
}

func (b *BufferBase) SetAudioLevelParams(audioLevelParams audio.AudioLevelParams) {
	b.Lock()
	defer b.Unlock()

	b.audioLevelParams = audioLevelParams
}

func (b *BufferBase) SetStreamRestartDetection(enable bool) {
	b.Lock()
	defer b.Unlock()

	b.enableStreamRestartDetection = enable
}

func (b *BufferBase) setupRTPStats(clockRate uint32) {
	b.rtpStats = rtpstats.NewRTPStatsReceiver(rtpstats.RTPStatsParams{
		ClockRate: clockRate,
		Logger:    b.logger,
	})
	b.ppsSnapshotId = b.rtpStats.NewSnapshotId()
	if b.isReportingEnabled {
		b.rrSnapshotId = b.rtpStats.NewSnapshotId()
		b.deltaStatsSnapshotId = b.rtpStats.NewSnapshotId()
	}

	if b.isOOBNACK {
		b.rtpStatsLite = rtpstats.NewRTPStatsReceiverLite(rtpstats.RTPStatsParams{
			ClockRate: clockRate,
			Logger:    b.logger,
		})
		b.liteStatsSnapshotId = b.rtpStatsLite.NewSnapshotLiteId()
	}
}

func (b *BufferBase) createDDParserAndFrameRateCalculator() {
	if mime.IsMimeTypeSVCCapable(b.mime) || b.mime == mime.MimeTypeVP8 {
		frc := NewFrameRateCalculatorDD(b.clockRate, b.logger)
		for i := range b.frameRateCalculator {
			b.frameRateCalculator[i] = frc.GetFrameRateCalculatorForSpatial(int32(i))
		}
		b.ddParser = NewDependencyDescriptorParser(
			b.ddExtID,
			b.logger,
			func(spatial, temporal int32) {
				frc.SetMaxLayer(spatial, temporal)
			},
			false,
		)
	}
}

func (b *BufferBase) createFrameRateCalculator() {
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

func (b *BufferBase) ReadExtended(buf []byte) (*ExtPacket, error) {
	b.Lock()
	for {
		if b.isClosed.Load() {
			b.Unlock()
			return nil, io.EOF
		}

		if b.extPackets.Len() > 0 {
			ep := b.extPackets.PopFront()
			patched := b.patchExtPacket(ep, buf)
			if patched == nil {
				ReleaseExtPacket(ep)
				continue
			}

			b.Unlock()
			return patched, nil
		}

		b.readCond.Wait()
	}
}

func (b *BufferBase) SetPLIThrottle(duration int64) {
	b.Lock()
	defer b.Unlock()

	b.pliThrottle = duration
}

func (b *BufferBase) SendPLI(force bool) {
	b.RLock()
	rtpStats := b.rtpStats
	pliThrottle := b.pliThrottle
	b.RUnlock()

	if (rtpStats == nil && !force) || !rtpStats.CheckAndUpdatePli(pliThrottle, force) {
		return
	}

	if b.sendPLI != nil {
		b.sendPLI()
	}
}

func (b *BufferBase) SetRTT(rtt uint32) {
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

func (b *BufferBase) WaitRead() {
	b.readCond.Wait()
}

func (b *BufferBase) NotifyRead() {
	b.readCond.Broadcast()
}

func (b *BufferBase) HandleIncomingPacketLocked(
	rawPkt []byte,
	rtpPacket *rtp.Packet,
	arrivalTime int64,
	isBuffered bool,
	skippedSeqs []uint16,
) (*rtp.Packet, rtpstats.RTPFlowState, *ExtPacket, error) {
	if rtpPacket == nil {
		rtpPacket = &rtp.Packet{}
		if err := rtpPacket.Unmarshal(rawPkt); err != nil {
			b.logger.Errorw("could not unmarshal RTP packet", err)
			return nil, rtpstats.RTPFlowState{}, err
		}
	}

	b.processAudioSsrcLevelHeaderExtension(rtpPacket, arrivalTime)

	if len(skippedSeqs) > 0 {
		skippedRtpPkt := rtp.Packet{
			Header: rtpPacket.Header,
		}
		skippedRtpPkt.Marker = false
		// Use the current highest timestamp to prevent the case of old sequence number and newer timestamp.
		// It is possible that the skipped packet is older. An example sequence
		//   - Packet 10, skipped 6, 7, 9 -> Packet 8 is unknown at this point
		//   - Packet 11, skipped 8 -> this would cause sequence number be older, but using timestamp from Packet 11 will make time stamp diff +ve
		skippedRtpPkt.Timestamp = b.rtpStats.HighestTimestamp()
		for _, sn := range skippedSeqs {
			skippedRtpPkt.SequenceNumber = sn
			flowState := b.rtpStats.Update(
				arrivalTime,
				skippedRtpPkt.Header.SequenceNumber,
				skippedRtpPkt.Header.Timestamp,
				skippedRtpPkt.Header.Marker,
				skippedRtpPkt.Header.MarshalSize(),
				len(skippedRtpPkt.Payload),
				int(skippedRtpPkt.PaddingSize),
			)
			if flowState.UnhandledReason == rtpstats.RTPFlowUnhandledReasonNone && !flowState.IsOutOfOrder {
				if err := b.snRangeMap.ExcludeRange(flowState.ExtSequenceNumber, flowState.ExtSequenceNumber+1); err != nil {
					b.logger.Errorw(
						"could not exclude range", err,
						// RAJA-TODO "spatialLayer", tr.Layer,
						"sequenceNumber", sn,
						"extSequenceNumber", flowState.ExtSequenceNumber,
						"rtpStats", b.rtpStats,
						"rtpStatsLite", b.rtpStatsLite,
						"snRangeMap", b.snRangeMap,
						"skipped", skippedSeqs,
					)
				}
			}
		}
	}

	isRestart := false
	flowState := b.rtpStats.Update(
		arrivalTime,
		rtpPacket.Header.SequenceNumber,
		rtpPacket.Header.Timestamp,
		rtpPacket.Header.Marker,
		rtpPacket.Header.MarshalSize(),
		len(rtpPacket.Payload),
		int(rtpPacket.PaddingSize),
	)
	switch flowState.UnhandledReason {
	case rtpstats.RTPFlowUnhandledReasonNone:
	case rtpstats.RTPFlowUnhandledReasonRestart:
		if !b.enableStreamRestartDetection {
			return rtpPacket, flowState, fmt.Errorf("unhandled reason: %s", flowState.UnhandledReason.String())
		}

		b.StopKeyFrameSeeder()

		b.rtpStats.Stop()
		b.logger.Infow("stream restart - rtp stats", b.rtpStats)

		b.snRangeMap = utils.NewRangeMap[uint64, uint64](100)
		b.setupRTPStats(b.clockRate)
		b.bucket.ResyncOnNextPacket()
		if b.nacker != nil {
			b.nacker = nack.NewNACKQueue(nack.NackQueueParamsDefault)
		}
		b.flushExtPacketsLocked()

		flowState = b.rtpStats.Update(
			arrivalTime,
			rtpPacket.Header.SequenceNumber,
			rtpPacket.Header.Timestamp,
			rtpPacket.Header.Marker,
			rtpPacket.Header.MarshalSize(),
			len(rtpPacket.Payload),
			int(rtpPacket.PaddingSize),
		)
		isRestart = true
	default:
		return rtpPacket, flowState, fmt.Errorf("unhandled reason: %s", flowState.UnhandledReason.String())
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
		return rtpPacket, flowState, errors.New("padding only packet")
	}

	// RAJA-TODO: ensure this does not get triggered in relay path
	if !flowState.IsOutOfOrder && rtpPacket.PayloadType != b.payloadType && b.codecType == webrtc.RTPCodecTypeVideo {
		b.handleCodecChange(rtpPacket.PayloadType)
	}

	// add to RTX buffer using sequence number after accounting for dropped padding only packets
	snAdjustment, err := b.snRangeMap.GetValue(flowState.ExtSequenceNumber)
	if err != nil {
		b.logger.Errorw(
			"could not get sequence number adjustment", err,
			// RAJA-TODO "spatialLayer", tr.Layer,
			"sequenceNumber", rtpPacket.SequenceNumber,
			"extSequenceNumber", flowState.ExtSequenceNumber,
			"timestamp", rtpPacket.Timestamp,
			"extTimestamp", flowState.ExtTimestamp,
			"payloadSize", len(rtpPacket.Payload),
			"paddingSize", rtpPacket.PaddingSize,
			"rtpStats", b.rtpStats,
			"rtpStatsLite", b.rtpStatsLite,
			"snRangeMap", b.snRangeMap,
		)
		return rtpPacket, flowState, err
	}

	flowState.ExtSequenceNumber -= snAdjustment
	rtpPacket.Header.SequenceNumber = uint16(flowState.ExtSequenceNumber)
	if _, err = b.bucket.AddPacketWithSequenceNumber(rawPkt, flowState.ExtSequenceNumber); err != nil {
		if !flowState.IsDuplicate {
			if errors.Is(err, bucket.ErrPacketTooOld) {
				packetTooOldCount := b.packetTooOldCount.Inc()
				if (packetTooOldCount-1)%100 == 0 {
					b.logger.Warnw(
						"could not add packet to bucket", err,
						"count", packetTooOldCount,
						// RAJA-TODO "spatialLayer", tr.Layer,
						"flowState", &flowState,
						"snAdjustment", snAdjustment,
						"incomingSequenceNumber", flowState.ExtSequenceNumber+snAdjustment,
						"rtpStats", b.rtpStats,
						"rtpStatsLite", b.rtpStatsLite,
						"snRangeMap", b.snRangeMap,
						"skipped", skippedSeqs,
					)
				}
			} else if err != bucket.ErrRTXPacket {
				b.logger.Warnw(
					"could not add packet to bucket", err,
					// RAJA-TODO "spatialLayer", tr.Layer,
					"flowState", &flowState,
					"snAdjustment", snAdjustment,
					"incomingSequenceNumber", flowState.ExtSequenceNumber+snAdjustment,
					"rtpStats", b.rtpStats,
					"rtpStatsLite", b.rtpStatsLite,
					"snRangeMap", b.snRangeMap,
					"skipped", skippedSeqs,
				)
			}
		}
		return rtpPacket, flowState, err
	}

	ep := b.getExtPacket(rtpPacket, arrivalTime, isBuffered, isRestart, flowState)
	if ep == nil {
		return rtpPacket, flowState, errors.New("could not get ext packet")
	}
	b.extPackets.PushBack(ep)
	b.readCond.Broadcast()

	if b.extPackets.Len() > b.bucket.Capacity() {
		if (b.extPacketTooMuchCount.Inc()-1)%100 == 0 {
			b.logger.Warnw("too much ext packets", nil, "count", b.extPackets.Len())
		}
	}

	b.maybeGrowBucket(arrivalTime)

	return rtpPacket, flowState, nil
}

func (b *BufferBase) UpdateNACKStateLocked(sequenceNumber uint16, flowState rtpstats.RTPFlowState) {
	if b.nacker == nil {
		return
	}

	b.nacker.Remove(sequenceNumber)

	for lost := flowState.LossStartInclusive; lost != flowState.LossEndExclusive; lost++ {
		b.nacker.Push(uint16(lost))
	}
}

func (b *BufferBase) UpdateOOBNACKStateLocked(sequenceNumber uint16, arrivalTime int64, size int) {
	if b.nacker != nil || !b.isOOBNACK {
		return
	}

	fsLite := b.rtpStatsLite.Update(arrivalTime, size, sequenceNumber)
	if !fsLite.IsNotHandled {
		b.nacker.Remove(sequenceNumber)

		for lost := fsLite.LossStartInclusive; lost != fsLite.LossEndExclusive; lost++ {
			b.nacker.Push(uint16(lost))
		}
	}
}

func (b *BufferBase) processAudioSsrcLevelHeaderExtension(p *rtp.Packet, arrivalTime int64) {
	if b.audioLevelExtID == 0 {
		return
	}

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

func (b *BufferBase) handleCodecChange(newPT uint8) {
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

	b.StartKeyFrameSeeder()
}

/* RAJA-TODO  - relayBuffer uses rtpStatsLite for NACKs, need some way to differentiate */
/* RAJA-REMOVE
func (b *BufferBase) updateStreamState(p *rtp.Packet, arrivalTime int64) rtpstats.RTPFlowState {
	return b.rtpStats.Update(
		arrivalTime,
		p.Header.SequenceNumber,
		p.Header.Timestamp,
		p.Header.Marker,
		p.Header.MarshalSize(),
		len(p.Payload),
		int(p.PaddingSize),
	)
}
*/

/* RAJA-TODO - this one split up as AddPacket and processVideoPkt in relayBuffer */
func (b *BufferBase) getExtPacket(
	rtpPacket *rtp.Packet,
	arrivalTime int64,
	isBuffered bool,
	isRestart bool,
	flowState rtpstats.RTPFlowState,
) *ExtPacket {
	ep := ExtPacketFactory.Get().(*ExtPacket)
	*ep = ExtPacket{
		Arrival:           arrivalTime,
		ExtSequenceNumber: flowState.ExtSequenceNumber,
		ExtTimestamp:      flowState.ExtTimestamp,
		Packet:            rtpPacket,
		VideoLayer: VideoLayer{
			Spatial:  InvalidLayerSpatial,
			Temporal: InvalidLayerTemporal,
		},
		IsOutOfOrder: flowState.IsOutOfOrder,
		IsBuffered:   isBuffered,
		IsRestart:    isRestart,
	}

	if len(ep.Packet.Payload) == 0 {
		// padding only packet, nothing else to do
		return ep
	}

	if err := b.processVideoPacket(ep); err != nil {
		ReleaseExtPacket(ep)
		return nil
	}

	if b.absCaptureTimeExtID != 0 {
		extData := rtpPacket.GetExtension(b.absCaptureTimeExtID)

		var actExt act.AbsCaptureTime
		if err := actExt.Unmarshal(extData); err == nil {
			ep.AbsCaptureTimeExt = &actExt
		}
	}

	return ep
}

func (b *BufferBase) processVideoPacket(ep *ExtPacket) error {
	if b.codecType != webrtc.RTPCodecTypeVideo {
		return nil
	}

	ep.Temporal = 0
	var videoSize []VideoSize
	if b.ddParser != nil {
		ddVal, videoLayer, err := b.ddParser.Parse(ep.Packet)
		if err != nil {
			if errors.Is(err, ErrDDExtentionNotFound) {
				if b.mime == mime.MimeTypeVP8 || b.mime == mime.MimeTypeVP9 {
					b.logger.Infow("dd extension not found, disable dd parser")
					b.ddParser = nil
					b.createFrameRateCalculator()
				}
			} else {
				return err
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
		if err := vp8Packet.Unmarshal(ep.Packet.Payload); err != nil {
			b.logger.Warnw("could not unmarshal VP8 packet", err)
			return err
		}
		ep.KeyFrame = vp8Packet.IsKeyFrame
		if ep.DependencyDescriptor == nil {
			ep.Temporal = int32(vp8Packet.TID)

			if ep.KeyFrame {
				if sz := ExtractVP8VideoSize(&vp8Packet, ep.Packet.Payload); sz.Width > 0 && sz.Height > 0 {
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
			_, err := vp9Packet.Unmarshal(ep.Packet.Payload)
			if err != nil {
				b.logger.Warnw("could not unmarshal VP9 packet", err)
				return err
			}
			ep.VideoLayer = VideoLayer{
				Spatial:  int32(vp9Packet.SID),
				Temporal: int32(vp9Packet.TID),
			}
			ep.Payload = vp9Packet
			ep.KeyFrame = IsVP9KeyFrame(&vp9Packet, ep.Packet.Payload)

			if ep.KeyFrame {
				for i := 0; i < len(vp9Packet.Width); i++ {
					videoSize = append(videoSize, VideoSize{
						Width:  uint32(vp9Packet.Width[i]),
						Height: uint32(vp9Packet.Height[i]),
					})
				}
			}
		} else {
			ep.KeyFrame = IsVP9KeyFrame(nil, ep.Packet.Payload)
		}

	case mime.MimeTypeH264:
		ep.KeyFrame = IsH264KeyFrame(ep.Packet.Payload)
		ep.Spatial = InvalidLayerSpatial // h.264 don't have spatial scalability, reset to invalid

		// Check H264 key frame video size
		if ep.KeyFrame {
			if sz := ExtractH264VideoSize(ep.Packet.Payload); sz.Width > 0 && sz.Height > 0 {
				videoSize = append(videoSize, sz)
			}
		}

	case mime.MimeTypeAV1:
		ep.KeyFrame = IsAV1KeyFrame(ep.Packet.Payload)

	case mime.MimeTypeH265:
		ep.KeyFrame = IsH265KeyFrame(ep.Packet.Payload)
		if ep.DependencyDescriptor == nil {
			if len(ep.Packet.Payload) < 2 {
				b.logger.Warnw("invalid H265 packet", nil, "payloadLen", len(ep.Packet.Payload))
				return errors.New("invalid H265 packet")
			}
			ep.VideoLayer = VideoLayer{
				Temporal: int32(ep.Packet.Payload[1]&0x07) - 1,
			}
			ep.Spatial = InvalidLayerSpatial

			if ep.KeyFrame {
				if sz := ExtractH265VideoSize(ep.Packet.Payload); sz.Width > 0 && sz.Height > 0 {
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

	if len(videoSize) > 0 {
		b.checkVideoSizeChange(videoSize)
	}

	b.doFpsCalc(ep)

	return nil
}

func (b *BufferBase) patchExtPacket(ep *ExtPacket, buf []byte) *ExtPacket {
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
				"rtpStatsLite", b.rtpStatsLite,
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

func (b *BufferBase) flushExtPackets() {
	b.Lock()
	defer b.Unlock()
	b.flushExtPacketsLocked()
}

func (b *BufferBase) flushExtPacketsLocked() {
	for b.extPackets.Len() > 0 {
		ep := b.extPackets.PopFront()
		ReleaseExtPacket(ep)
	}
	b.extPackets.Clear()
}

func (b *BufferBase) maybeGrowBucket(now int64) {
	if now-b.lastBucketCapCheckAt < bucketCapCheckInterval {
		return
	}
	b.lastBucketCapCheckAt = now

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
				b.logger.Infow(
					"grow bucket",
					"from", oldCap,
					"to", cap,
					"pps", pps,
					"deltaInfo", deltaInfo,
					"rtpStats", b.rtpStats,
				)
			}
		}
	}

	/* RAJA-TODO - have to somehow slow this in for relay
		if r.packetCounter != nil {
		if deltaInfoLite := r.rtpStatsLite.DeltaInfoLite(r.liteStatsSnapshotId); deltaInfoLite != nil {
			r.packetCounter.AddLoss(deltaInfoLite.PacketsLost, deltaInfoLite.Packets)
			r.packetCounter.AddOutOfOrder(deltaInfoLite.PacketsOutOfOrder, deltaInfoLite.Packets)
		}
	}
	*/
}

func (b *BufferBase) doFpsCalc(ep *ExtPacket) {
	// RAJA-TODO: b.isPaused in different for relay buffer, check if it is okay
	if b.isPaused || b.frameRateCalculated || len(ep.Packet.Payload) == 0 {
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

func (b *BufferBase) SetSenderReportData(srData *livekit.RTCPSenderReportState) {
	/* RAJA-TODO - there is no callback in relay because packets comes in via receiver
	need to think about making a callback or keep implementation out of base? */
	srData.At = mono.UnixNano()
	b.RLock()
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

func (b *BufferBase) GetSenderReportData() *livekit.RTCPSenderReportState {
	b.RLock()
	defer b.RUnlock()

	if b.rtpStats != nil {
		return b.rtpStats.GetRtcpSenderReportData()
	}

	return nil
}

/* RAJA-TODO - relayBuffer checks for stopped buffer - should add here also */
func (b *BufferBase) GetPacket(buff []byte, esn uint64) (int, error) {
	b.Lock()
	defer b.Unlock()

	return b.getPacket(buff, esn)
}

func (b *BufferBase) getPacket(buff []byte, esn uint64) (int, error) {
	if b.isClosed.Load() {
		return 0, io.EOF
	}
	return b.bucket.GetPacket(buff, esn)
}

func (b *BufferBase) GetStats() *livekit.RTPStats {
	b.RLock()
	defer b.RUnlock()

	if b.rtpStats == nil {
		return nil
	}

	return b.rtpStats.ToProto()
}

func (b *BufferBase) GetDeltaStats() *StreamStatsWithLayers {
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

func (b *BufferBase) GetLastSenderReportTime() time.Time {
	b.RLock()
	defer b.RUnlock()

	if b.rtpStats == nil {
		return time.Time{}
	}

	return b.rtpStats.LastSenderReportTime()
}

func (b *BufferBase) GetAudioLevel() (float64, bool) {
	b.RLock()
	defer b.RUnlock()

	if b.audioLevel == nil {
		return 0, false
	}

	return b.audioLevel.GetLevel(mono.UnixNano())
}

func (b *BufferBase) OnRtcpSenderReport(fn func()) {
	b.Lock()
	b.onRtcpSenderReport = fn
	b.Unlock()
}

func (b *BufferBase) getOnRtcpSenderReport() func() {
	b.RLock()
	defer b.RUnlock()

	return b.onRtcpSenderReport
}

func (b *BufferBase) OnFpsChanged(f func()) {
	b.Lock()
	b.onFpsChanged = f
	b.Unlock()
}

func (b *BufferBase) OnVideoSizeChanged(fn func([]VideoSize)) {
	b.Lock()
	b.onVideoSizeChanged = fn
	b.Unlock()
}

func (b *Buffer) OnCodecChange(fn func(webrtc.RTPCodecParameters)) {
	b.Lock()
	b.onCodecChange = fn
	b.Unlock()
}

// checkVideoSizeChange checks if video size has changed for a specific spatial layer and fires callback
func (b *BufferBase) checkVideoSizeChange(videoSizes []VideoSize) {
	if len(videoSizes) > len(b.currentVideoSize) {
		b.logger.Warnw(
			"video size index out of range", nil,
			"newSize", videoSizes,
			"currentVideoSize", b.currentVideoSize,
		)
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

func (b *BufferBase) GetTemporalLayerFpsForSpatial(layer int32) []float32 {
	b.RLock()
	defer b.RUnlock()

	if int(layer) >= len(b.frameRateCalculator) {
		return nil
	}

	if fc := b.frameRateCalculator[layer]; fc != nil {
		return fc.GetFrameRate()
	}
	return nil
}

func (b *BufferBase) StartKeyFrameSeeder() {
	if b.codecType == webrtc.RTPCodecTypeVideo {
		go b.seedKeyFrame(b.keyFrameSeederGeneration.Inc())
	}
}

func (b *BufferBase) StopKeyFrameSeeder() {
	b.keyFrameSeederGeneration.Inc()
}

func (b *BufferBase) seedKeyFrame(keyFrameSeederGeneration int32) {
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
	if rtpStats == nil {
		b.logger.Debugw("cannot do key frame seeding without stats")
		return
	}
	initialCount, _ = rtpStats.KeyFrame()

	for {
		if b.isClosed.Load() || b.keyFrameSeederGeneration.Load() != keyFrameSeederGeneration {
			b.logger.Debugw("stopping key frame seeder: stopped")
			return
		}

		select {
		case <-timer.C:
			b.logger.Debugw("stopping key frame seeder: timeout")
			return

		case <-ticker.C:
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

func (b *BufferBase) GetNACKPairsLocked() []rtcp.NackPair {
	if b.nacker == nil {
		return nil
	}

	pairs, numSeqNumsNacked := b.nacker.Pairs()
	if !b.isOOBNACK {
		if b.rtpStats != nil {
			b.rtpStats.UpdateNack(uint32(numSeqNumsNacked))
		}
	} else {
		if b.rtpStatsLite != nil {
			b.rtpStatsLite.UpdateNack(uint32(numSeqNumsNacked))
		}
	}

	return pairs
}

func (b *BufferBase) GetRtcpReceptionReportLocked(proxyLoss uint8) *rtcp.ReceptionReport {
	if b.rtpStats == nil {
		return nil
	}

	return b.rtpStats.GetRtcpReceptionReport(b.mediaSSRC, proxyLoss, b.rrSnapshotId)
}

// ---------------------------------------------------------------

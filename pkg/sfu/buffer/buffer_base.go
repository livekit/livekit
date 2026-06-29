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
	act "github.com/livekit/livekit-server/pkg/sfu/rtpextension/abscapturetime"
	dd "github.com/livekit/livekit-server/pkg/sfu/rtpextension/dependencydescriptor"
	"github.com/livekit/livekit-server/pkg/sfu/rtpstats"
	"github.com/livekit/livekit-server/pkg/sfu/utils"
	"github.com/livekit/mediatransportutil/pkg/bucket"
	"github.com/livekit/mediatransportutil/pkg/codec"
	"github.com/livekit/mediatransportutil/pkg/nack"
	"github.com/livekit/protocol/codecs/mime"
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
	IsKeyFrame           bool
	RawPacket            []byte
	DependencyDescriptor *ExtDependencyDescriptor
	AbsCaptureTimeExt    *act.AbsCaptureTime
	IsOutOfOrder         bool
	IsBuffered           bool
}

type BufferProvider interface {
	SetLogger(lgr logger.Logger)
	SetAudioLevelConfig(audioLevelConfig audio.AudioLevelConfig)
	SetStreamRestartDetection(enable bool)
	SetPLIThrottle(duration int64)
	SetRTT(rtt uint32)
	SetPaused(paused bool)

	SendPLI(force bool)

	ReadExtended(buf []byte) (*ExtPacket, error)
	GetPacket(buf []byte, esn uint64) (int, error)

	EnableVideoFrameCache(maxDuration time.Duration)
	GetVideoFrameCache() ([]*ExtPacket, bool)
	GetPacketsAfter(afterESN uint64) ([]*ExtPacket, bool)

	GetAudioLevel() (float64, bool)
	GetTemporalLayerFpsForSpatial(layer int32) []float32
	GetStats() *livekit.RTPStats
	GetDeltaStats() *StreamStatsWithLayers
	GetDeltaStatsLite() *rtpstats.RTPDeltaInfoLite
	GetLastSenderReportTime() time.Time
	GetNACKPairs() []rtcp.NackPair

	SetSenderReportData(srData *livekit.RTCPSenderReportState)
	GetSenderReportData() *livekit.RTCPSenderReportState

	OnRtcpSenderReport(fn func())
	OnFpsChanged(fn func())
	OnVideoSizeChanged(fn func([]codec.VideoSize))
	OnCodecChange(fn func(webrtc.RTPCodecParameters))
	OnStreamRestart(fn func(string))

	StartKeyFrameSeeder()
	StopKeyFrameSeeder()

	HandleIncomingPacket(
		rawPkt []byte,
		rtpPacket *rtp.Packet,
		arrivalTime int64,
		isBuffered bool,
		isRTX bool,
		skippedSeqs []uint16,
		oobSequenceNumber uint16,
	) (uint64, error)

	MarkForRestartStream(reason string)
	RestartStream(reason string)

	CloseWithReason(reason string) (*livekit.RTPStats, error)
}

const (
	bucketCapCheckInterval = 1e9
)

type BufferBaseParams struct {
	SSRC                uint32
	MaxVideoPkts        int
	MaxAudioPkts        int
	LoggerComponents    []string
	SendPLI             func()
	IsReportingEnabled  bool
	IsOOBSequenceNumber bool
	IsDDRestartEnabled  bool
}

type BufferBase struct {
	sync.RWMutex

	params BufferBaseParams

	readCond *sync.Cond

	bucket               *bucket.Bucket[uint64, uint16]
	lastBucketCapCheckAt int64

	nacker              nack.NackQueueInterface
	rtpStatsLite        *rtpstats.RTPStatsReceiverLite
	liteStatsSnapshotId uint32

	extPackets deque.Deque[*ExtPacket]

	codecType webrtc.RTPCodecType
	closeOnce sync.Once
	clockRate uint32
	mime      mime.MimeType

	rtpParameters  webrtc.RTPParameters
	payloadType    uint8
	rtxPayloadType uint8

	snRangeMap *utils.RangeMap[uint64, uint64]

	audioLevelConfig audio.AudioLevelConfig
	audioLevel       *audio.AudioLevel
	audioLevelExtID  uint8

	enableStreamRestartDetection bool

	pliThrottle int64

	rtpStats             *rtpstats.RTPStatsReceiver
	ppsSnapshotId        uint32
	rrSnapshotId         uint32
	deltaStatsSnapshotId uint32

	// callbacks
	onRtcpSenderReport func()
	onFpsChanged       func()
	onVideoSizeChanged func([]codec.VideoSize)
	onCodecChange      func(webrtc.RTPCodecParameters)
	onStreamRestart    func(string)

	// video size tracking for multiple spatial layers
	currentVideoSize [DefaultMaxLayerSpatial + 1]codec.VideoSize

	logger logger.Logger

	// dependency descriptor
	ddExtID  uint8
	ddParser *DependencyDescriptorParser

	// video frame cache: mark the most recent key frame so the current group-of-pictures can be read
	// back from the retransmit bucket (see EnableVideoFrameCache / GetVideoFrameCache). No packets are copied here - only
	// the key-frame boundary is tracked.
	videoFrameCacheEnabled     bool
	videoFrameCacheMaxDuration time.Duration // optional bound on key-frame interval (0 = bucket-bounded only)
	videoFrameCacheHasKeyFrame bool
	videoFrameCacheKeyFrameESN uint64 // ext sequence number of the current video frame cache group's first key-frame packet
	videoFrameCacheKeyFrameETS uint64 // ext timestamp of the current video frame cache group's key frame
	videoFrameCacheLatestTS    uint64 // maximum ext timestamp seen in the current video frame cache group (resets on a new key frame)

	isPaused            bool
	frameRateCalculator [DefaultMaxLayerSpatial + 1]FrameRateCalculator
	frameRateCalculated bool

	packetNotFoundCount   atomic.Uint32
	packetTooOldCount     atomic.Uint32
	extPacketTooMuchCount atomic.Uint32

	absCaptureTimeExtID uint8

	keyFrameSeederGeneration atomic.Int32

	isRestartPending bool

	isClosed atomic.Bool
}

func NewBufferBase(params BufferBaseParams) *BufferBase {
	l := logger.GetLogger() // will be reset with correct context via SetLogger
	for _, component := range params.LoggerComponents {
		l = l.WithComponent(component)
	}
	l = l.WithValues("ssrc", params.SSRC)

	b := &BufferBase{
		params:               params,
		lastBucketCapCheckAt: mono.UnixNano(),
		snRangeMap:           utils.NewRangeMap[uint64, uint64](100),
		pliThrottle:          int64(500 * time.Millisecond),
		logger:               l,
	}
	b.readCond = sync.NewCond(&b.RWMutex)
	b.extPackets.SetBaseCap(128)
	return b
}

func (b *BufferBase) SSRC() uint32 {
	return b.params.SSRC
}

func (b *BufferBase) MaxVideoPkts() int {
	return b.params.MaxVideoPkts
}

func (b *BufferBase) MaxAudioPkts() int {
	return b.params.MaxAudioPkts
}

func (b *BufferBase) SetLogger(lgr logger.Logger) {
	b.Lock()
	defer b.Unlock()

	for _, component := range b.params.LoggerComponents {
		lgr = lgr.WithComponent(component)
	}
	lgr = lgr.WithValues("ssrc", b.params.SSRC)
	b.logger = lgr

	if b.rtpStats != nil {
		b.rtpStats.SetLogger(b.logger)
	}

	if b.rtpStatsLite != nil {
		b.rtpStatsLite.SetLogger(b.logger)
	}
}

func (b *BufferBase) Bind(rtpParameters webrtc.RTPParameters, codec webrtc.RTPCodecCapability, bitrate int) error {
	b.logger.Debugw("binding track")

	b.Lock()
	defer b.Unlock()

	return b.BindLocked(rtpParameters, codec, bitrate)
}

func (b *BufferBase) BindLocked(rtpParameters webrtc.RTPParameters, codec webrtc.RTPCodecCapability, bitrate int) error {
	b.logger.Debugw("binding track")
	if codec.ClockRate == 0 {
		b.logger.Warnw("invalid codec", nil, "rtpParameters", rtpParameters, "codec", codec, "bitrate", bitrate)
		return errInvalidCodec
	}

	b.setupRTPStats(codec.ClockRate)

	b.clockRate = codec.ClockRate
	b.mime = mime.NormalizeMimeType(codec.MimeType)
	b.rtpParameters = rtpParameters
	for _, codecParameter := range rtpParameters.Codecs {
		if mime.IsMimeTypeStringEqual(codecParameter.MimeType, codec.MimeType) {
			b.payloadType = uint8(codecParameter.PayloadType)
			break
		}
	}

	if b.payloadType == 0 && !mime.IsMimeTypeStringEqual(codec.MimeType, webrtc.MimeTypePCMU) {
		b.logger.Warnw(
			"could not find payload type for codec", nil,
			"codec", codec.MimeType,
			"rtpParameters", rtpParameters,
		)
		b.payloadType = uint8(rtpParameters.Codecs[0].PayloadType)
	}

	// find RTX payload type
	for _, codec := range rtpParameters.Codecs {
		if mime.IsMimeTypeStringRTX(codec.MimeType) && strings.Contains(codec.SDPFmtpLine, fmt.Sprintf("apt=%d", b.payloadType)) {
			b.rtxPayloadType = uint8(codec.PayloadType)
			break
		}
	}

	for _, ext := range rtpParameters.HeaderExtensions {
		switch ext.URI {
		case dd.ExtensionURI:
			if b.ddExtID != 0 {
				b.logger.Warnw(
					"multiple dependency descriptor extensions found", nil,
					"id", ext.ID,
					"previous", b.ddExtID,
				)
				continue
			}
			b.ddExtID = uint8(ext.ID)
			b.createDDParserAndFrameRateCalculator()

		case sdp.AudioLevelURI:
			b.audioLevelExtID = uint8(ext.ID)
			b.audioLevel = audio.NewAudioLevel(audio.AudioLevelParams{
				ClockRate: b.clockRate,
			})
			b.audioLevel.SetConfig(b.audioLevelConfig)

		case act.AbsCaptureTimeURI:
			b.absCaptureTimeExtID = uint8(ext.ID)
		}
	}

	switch {
	case mime.IsMimeTypeAudio(b.mime):
		b.codecType = webrtc.RTPCodecTypeAudio
		b.bucket = bucket.NewBucket[uint64, uint16](
			InitPacketBufferSizeAudio,
			bucket.RTPMaxPktSize,
			bucket.RTPSeqNumOffset,
		)

	case mime.IsMimeTypeVideo(b.mime):
		b.codecType = webrtc.RTPCodecTypeVideo
		b.bucket = bucket.NewBucket[uint64, uint16](
			InitPacketBufferSizeVideo,
			bucket.RTPMaxPktSize,
			bucket.RTPSeqNumOffset,
		)

		if b.frameRateCalculator[0] == nil {
			b.createFrameRateCalculator()
		}

		if bitrate > 0 {
			pps := bitrate / 8 / 1200
			for pps > b.bucket.Capacity() {
				if b.bucket.Grow() >= b.params.MaxVideoPkts {
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
			if b.nacker == nil {
				b.nacker = nack.NewNACKQueue(nack.NackQueueParamsDefault)
			}
		}
	}

	if b.nacker == nil && b.params.IsOOBSequenceNumber {
		b.nacker = nack.NewNACKQueue(nack.NackQueueParamsDefault)
	}

	b.StartKeyFrameSeeder()

	return nil
}

func (b *BufferBase) CloseWithReason(reason string) (stats *livekit.RTPStats, err error) {
	b.closeOnce.Do(func() {
		b.isClosed.Store(true)

		b.StopKeyFrameSeeder()

		b.Lock()
		stats, _ = b.stopRTPStats(reason)
		b.readCond.Broadcast()
		b.Unlock()

		go b.flushExtPackets()
	})
	return
}

func (b *BufferBase) IsClosed() bool {
	return b.isClosed.Load()
}

func (b *BufferBase) SetPaused(paused bool) {
	b.Lock()
	defer b.Unlock()

	b.isPaused = paused
}

func (b *BufferBase) SetAudioLevelConfig(audioLevelConfig audio.AudioLevelConfig) {
	b.Lock()
	defer b.Unlock()

	b.audioLevelConfig = audioLevelConfig
	if b.audioLevel != nil {
		b.audioLevel.SetConfig(b.audioLevelConfig)
	}
}

func (b *BufferBase) SetStreamRestartDetection(enable bool) {
	b.Lock()
	defer b.Unlock()

	b.enableStreamRestartDetection = enable
}

func (b *BufferBase) setupRTPStats(clockRate uint32) {
	b.rtpStats = rtpstats.NewRTPStatsReceiver(rtpstats.RTPStatsParams{})
	b.rtpStats.SetLogger(b.logger)
	b.rtpStats.SetClockRate(clockRate)

	b.ppsSnapshotId = b.rtpStats.NewSnapshotId()
	if b.params.IsReportingEnabled {
		b.rrSnapshotId = b.rtpStats.NewSnapshotId()
		b.deltaStatsSnapshotId = b.rtpStats.NewSnapshotId()
	}

	b.setupRTPStatsLite(clockRate)
}

func (b *BufferBase) setupRTPStatsLite(clockRate uint32) {
	if b.params.IsOOBSequenceNumber {
		b.rtpStatsLite = rtpstats.NewRTPStatsReceiverLite(rtpstats.RTPStatsParams{})
		b.rtpStatsLite.SetLogger(b.logger)
		b.rtpStatsLite.SetClockRate(clockRate)

		b.liteStatsSnapshotId = b.rtpStatsLite.NewSnapshotLiteId()
	}
}

func (b *BufferBase) stopRTPStats(reason string) (stats *livekit.RTPStats, statsLite *livekit.RTPStats) {
	if b.rtpStats != nil {
		b.rtpStats.Stop()
		stats = b.rtpStats.ToProto()

		b.logger.Debugw(
			"rtp stats",
			"direction", "upstream",
			"stats", b.rtpStats,
			"reason", reason,
		)
	}

	statsLite = b.stopRTPStatsLite(reason)
	return
}

func (b *BufferBase) stopRTPStatsLite(reason string) (statsLite *livekit.RTPStats) {
	if b.rtpStatsLite != nil {
		b.rtpStatsLite.Stop()
		statsLite = b.rtpStatsLite.ToProto()

		b.logger.Debugw(
			"rtp stats lite",
			"direction", "upstream",
			"statsLite", b.rtpStatsLite,
			"reason", reason,
		)
	}
	return
}

func (b *BufferBase) RestartOOBSequenceNumber(reason string) {
	b.Lock()
	defer b.Unlock()

	b.stopRTPStatsLite(reason)
	b.setupRTPStatsLite(b.clockRate)

	if b.nacker != nil {
		b.nacker = nack.NewNACKQueue(nack.NackQueueParamsDefault)
	}
}

func (b *BufferBase) MarkForRestartStream(reason string) {
	b.logger.Debugw("marking for stream restart", "reason", reason)

	b.Lock()
	defer b.Unlock()

	b.isRestartPending = true
	b.readCond.Broadcast()
}

func (b *BufferBase) RestartStream(reason string) {
	b.logger.Debugw("stream restart", "reason", reason)

	b.Lock()
	defer b.Unlock()

	b.restartStreamLocked(reason, false)
	b.readCond.Broadcast()
}

func (b *BufferBase) restartStreamLocked(reason string, isDetected bool) {
	b.logger.Infow("stream restart", "reason", reason)

	// stop
	b.StopKeyFrameSeeder()
	b.stopRTPStats("stream-restart")
	b.flushExtPacketsLocked()
	// the marked video frame cache group references the pre-restart sequence-number base / evicted bucket contents
	b.videoFrameCacheHasKeyFrame = false

	// restart
	b.snRangeMap = utils.NewRangeMap[uint64, uint64](100)
	b.setupRTPStats(b.clockRate)

	b.bucket.ResyncOnNextPacket()
	b.lastBucketCapCheckAt = mono.UnixNano()

	if b.nacker != nil {
		b.nacker = nack.NewNACKQueue(nack.NackQueueParamsDefault)
	}

	if b.audioLevel != nil {
		b.audioLevel = audio.NewAudioLevel(audio.AudioLevelParams{
			ClockRate: b.clockRate,
		})
		b.audioLevel.SetConfig(b.audioLevelConfig)
	}

	if b.ddExtID != 0 {
		b.createDDParserAndFrameRateCalculator()
	}

	b.frameRateCalculated = false
	if b.frameRateCalculator[0] == nil {
		b.createFrameRateCalculator()
	}

	b.StartKeyFrameSeeder()

	if isDetected {
		b.isRestartPending = true

		if f := b.onStreamRestart; f != nil {
			go f(reason)
		}
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
			b.params.IsDDRestartEnabled,
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

		if b.isRestartPending {
			b.isRestartPending = false
			b.Unlock()
			return nil, nil
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

// EnableVideoFrameCache turns on video frame cache tracking for this (video) buffer: the most recent key frame is marked
// so the current group-of-pictures can be read back from the retransmit bucket via GetVideoFrameCache. No
// packets are copied - only the key-frame boundary is tracked. maxDuration bounds the served
// key-frame interval AND drives the retransmit bucket to retain that much history (see
// maybeGrowBucket), so the key frame is not evicted before it can be read; <= 0 keeps the default
// ~1s retransmit window (and bounds the video frame cache group to it). No-op for audio buffers.
func (b *BufferBase) EnableVideoFrameCache(maxDuration time.Duration) {
	b.Lock()
	defer b.Unlock()

	b.videoFrameCacheEnabled = true
	b.videoFrameCacheMaxDuration = maxDuration
	b.logger.Debugw("video frame cache enabled on buffer", "maxDuration", maxDuration)
}

// markVideoFrameCacheLocked records the key-frame boundary of the current video frame cache group and tracks its span. Caller holds
// the lock.
func (b *BufferBase) markVideoFrameCacheLocked(ep *ExtPacket) {
	if ep == nil || ep.Packet == nil || len(ep.Packet.Payload) == 0 {
		return
	}
	if ep.IsKeyFrame && (!b.videoFrameCacheHasKeyFrame || ep.ExtTimestamp != b.videoFrameCacheKeyFrameETS) {
		// a new key frame can be used to decoders to reset; remember its first packet's sequence number and reset the
		// span to the key frame so a stale packet from the previous video frame cache group cannot stretch it
		b.videoFrameCacheKeyFrameESN = ep.ExtSequenceNumber
		b.videoFrameCacheKeyFrameETS = ep.ExtTimestamp
		b.videoFrameCacheLatestTS = ep.ExtTimestamp
		b.videoFrameCacheHasKeyFrame = true
		b.logger.Debugw("video frame cache: marked key frame", "keyFrameSN", b.videoFrameCacheKeyFrameESN, "keyFrameTS", b.videoFrameCacheKeyFrameETS)
		return
	}
	// track the maximum timestamp seen in the current video frame cache group (not the last-written one) so an
	// out-of-order, older packet arriving last cannot shrink the measured span and let GetVideoFrameCache serve
	// more than videoFrameCacheMaxDuration. The head packet's timestamp is always <= videoFrameCacheLatestTS, so the
	// duration gate in GetVideoFrameCache strictly bounds the served video frame cache group.
	if ep.ExtTimestamp > b.videoFrameCacheLatestTS {
		b.videoFrameCacheLatestTS = ep.ExtTimestamp
	}
}

// GetVideoFrameCache reads the packets of the current group-of-pictures (from the most recent key frame up to
// the latest packet) back from the retransmit bucket, so a newly attached down track can be
// bootstrapped without requesting a fresh key frame (PLI). Returns (nil, false) when the cache is
// disabled, no key frame has been marked, the key-frame interval exceeds the configured bound, or
// the key frame is no longer retained in the bucket (the video frame cache group length is ultimately bounded by the
// bucket capacity). Lost packets within the video frame cache group are skipped.
//
// The packets are returned as ExtPackets reconstructed from the bucket bytes so they can be replayed
// through the normal forward path (WriteRTP): ExtSequenceNumber comes from the bucket key and
// ExtTimestamp / IsKeyFrame are derived from the marked key frame. The dependency descriptor is not
// reconstructed (SVC replay is not supported here). The returned packets are self-contained copies.
func (b *BufferBase) GetVideoFrameCache() ([]*ExtPacket, bool) {
	b.Lock()
	defer b.Unlock()

	if !b.videoFrameCacheEnabled || !b.videoFrameCacheHasKeyFrame || b.bucket == nil {
		b.logger.Debugw(
			"video frame cache miss: not ready",
			"videoFrameCacheEnabled", b.videoFrameCacheEnabled,
			"videoFrameCacheHasKeyFrame", b.videoFrameCacheHasKeyFrame,
			"hasBucket", b.bucket != nil,
		)
		return nil, false
	}

	if b.videoFrameCacheMaxDuration > 0 && b.clockRate > 0 {
		maxTicks := uint64(b.videoFrameCacheMaxDuration.Seconds() * float64(b.clockRate))
		if b.videoFrameCacheLatestTS > b.videoFrameCacheKeyFrameETS+maxTicks {
			// key-frame interval longer than the bound - too old to serve a complete replay
			b.logger.Debugw(
				"video frame cache miss: key-frame interval exceeds bound",
				"keyFrameTS", b.videoFrameCacheKeyFrameETS,
				"latestTS", b.videoFrameCacheLatestTS,
				"spanTicks", b.videoFrameCacheLatestTS-b.videoFrameCacheKeyFrameETS,
				"maxTicks", maxTicks,
			)
			return nil, false
		}
	}

	headESN := uint64(b.bucket.HeadSequenceNumber())
	if headESN < b.videoFrameCacheKeyFrameESN {
		b.logger.Debugw("video frame cache miss: head behind key frame", "headESN", headESN, "keyFrameSN", b.videoFrameCacheKeyFrameESN)
		return nil, false
	}

	pkts := b.reconstructPacketsLocked(b.videoFrameCacheKeyFrameESN, headESN)
	// the key frame itself must be present (its first packet), otherwise the video frame cache group cannot be served
	if len(pkts) == 0 || pkts[0].ExtSequenceNumber != b.videoFrameCacheKeyFrameESN {
		var firstSN uint64
		if len(pkts) > 0 {
			firstSN = pkts[0].ExtSequenceNumber
		}
		b.logger.Debugw(
			"video frame cache miss: key frame evicted from bucket",
			"keyFrameSN", b.videoFrameCacheKeyFrameESN,
			"headESN", headESN,
			"bucketCapacity", b.bucket.Capacity(),
			"reconstructed", len(pkts),
			"firstSN", firstSN,
		)
		return nil, false
	}
	return pkts, true
}

// GetPacketsAfter reads the packets newer than afterESN (exclusive) up to the current head from the
// retransmit bucket, reconstructed as ExtPackets like GetVideoFrameCache. It is used to catch a video frame cache replay up to
// the live forwarding point. Returns (nil, false) when the cache is disabled or nothing newer is
// retained.
func (b *BufferBase) GetPacketsAfter(afterESN uint64) ([]*ExtPacket, bool) {
	b.Lock()
	defer b.Unlock()

	if !b.videoFrameCacheEnabled || !b.videoFrameCacheHasKeyFrame || b.bucket == nil {
		return nil, false
	}
	headESN := uint64(b.bucket.HeadSequenceNumber())
	if headESN <= afterESN {
		return nil, false
	}
	pkts := b.reconstructPacketsLocked(afterESN+1, headESN)
	if len(pkts) == 0 {
		return nil, false
	}
	return pkts, true
}

// reconstructPacketsLocked builds self-contained ExtPackets for the sequence-number range
// [fromSN, headESN] from the retransmit bucket. Lost packets are skipped. ExtTimestamp is
// reconstructed relative to the marked key frame (a video frame cache group spans well under one 32-bit timestamp wrap)
// and IsKeyFrame flags packets at the key-frame timestamp. The dependency descriptor is not
// reconstructed. Caller holds the lock.
func (b *BufferBase) reconstructPacketsLocked(fromSN, headESN uint64) []*ExtPacket {
	keyFrameRTPTS := uint32(b.videoFrameCacheKeyFrameETS)
	var pkts []*ExtPacket
	buf := make([]byte, bucket.RTPMaxPktSize)
	for sn := fromSN; sn <= headESN; sn++ {
		n, err := b.bucket.GetPacket(buf, sn)
		if err != nil {
			continue // lost packet, skip
		}
		// copy out of the reused read buffer so the parsed packet is self-contained
		raw := make([]byte, n)
		copy(raw, buf[:n])
		p := &rtp.Packet{}
		if err := p.Unmarshal(raw); err != nil {
			continue
		}

		extTS := b.videoFrameCacheKeyFrameETS + uint64(p.Timestamp-keyFrameRTPTS)
		pkts = append(pkts, &ExtPacket{
			VideoLayer:        VideoLayer{Spatial: InvalidLayerSpatial, Temporal: InvalidLayerTemporal},
			Arrival:           mono.UnixNano(),
			ExtSequenceNumber: sn,
			ExtTimestamp:      extTS,
			Packet:            p,
			IsKeyFrame:        extTS == b.videoFrameCacheKeyFrameETS,
			RawPacket:         raw,
		})
	}
	return pkts
}

func (b *BufferBase) SetPLIThrottle(duration int64) {
	b.Lock()
	defer b.Unlock()

	b.pliThrottle = duration
}

func (b *BufferBase) SendPLI(force bool) {
	b.RLock()
	if b.codecType != webrtc.RTPCodecTypeVideo {
		b.RUnlock()
		return
	}

	rtpStats := b.rtpStats
	pliThrottle := b.pliThrottle
	b.RUnlock()

	if rtpStats == nil || !rtpStats.CheckAndUpdatePli(pliThrottle, force) {
		return
	}

	if b.params.SendPLI != nil {
		b.params.SendPLI()
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

func (b *BufferBase) HandleIncomingPacket(
	rawPkt []byte,
	rtpPacket *rtp.Packet,
	arrivalTime int64,
	isBuffered bool,
	isRTX bool,
	skippedSeqs []uint16,
	oobSequenceNumber uint16,
) (uint64, error) {
	b.Lock()
	defer b.Unlock()

	if b.isClosed.Load() {
		return 0, io.EOF
	}

	return b.HandleIncomingPacketLocked(
		rawPkt,
		rtpPacket,
		arrivalTime,
		isBuffered,
		isRTX,
		skippedSeqs,
		oobSequenceNumber,
	)
}

func (b *BufferBase) HandleIncomingPacketLocked(
	rawPkt []byte,
	rtpPacket *rtp.Packet,
	arrivalTime int64,
	isBuffered bool,
	isRTX bool,
	skippedSeqs []uint16,
	oobSequenceNumber uint16,
) (uint64, error) {
	if rtpPacket == nil {
		rtpPacket = &rtp.Packet{}
		if err := rtpPacket.Unmarshal(rawPkt); err != nil {
			b.logger.Errorw("could not unmarshal RTP packet", err)
			return 0, err
		}
	}

	b.processAudioSsrcLevelHeaderExtension(rtpPacket, arrivalTime)

	if len(skippedSeqs) > 0 {
		// Use the current highest timestamp to prevent the case of old sequence number and newer timestamp.
		// It is possible that the skipped packet is older. An example sequence
		//   - Packet 10, skipped 6, 7, 9 -> Packet 8 is unknown at this point
		//   - Packet 11, skipped 8 -> this would cause sequence number to be older, but using timestamp from Packet 11 will make time stamp diff +ve
		ts := b.rtpStats.HighestTimestamp()
		for _, sn := range skippedSeqs {
			flowState := b.rtpStats.Update(
				arrivalTime,
				sn,
				ts,
				false, // no marker
				0,     // no header for skipped packet, so 0 size
				0,     // no payload
				0,     // no padding
			)
			if flowState.UnhandledReason == rtpstats.RTPFlowUnhandledReasonNone && !flowState.IsOutOfOrder {
				if err := b.snRangeMap.ExcludeRange(flowState.ExtSequenceNumber, flowState.ExtSequenceNumber+1); err != nil {
					b.logger.Errorw(
						"could not exclude range", err,
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

	// do not start on an RTX packet
	if isRTX && !b.rtpStats.IsActive() {
		return 0, errors.New("cannot start on rtx packet")
	}

	flowState := b.rtpStats.Update(
		arrivalTime,
		rtpPacket.Header.SequenceNumber,
		rtpPacket.Header.Timestamp,
		rtpPacket.Header.Marker,
		rtpPacket.Header.MarshalSize(),
		len(rtpPacket.Payload),
		int(rtpPacket.Header.PaddingSize),
	)
	switch flowState.UnhandledReason {
	case rtpstats.RTPFlowUnhandledReasonNone:
	case rtpstats.RTPFlowUnhandledReasonRestart:
		if !b.enableStreamRestartDetection {
			return 0, fmt.Errorf("unhandled reason: %s", flowState.UnhandledReason.String())
		}

		b.restartStreamLocked("discontinuity", true)

		flowState = b.rtpStats.Update(
			arrivalTime,
			rtpPacket.Header.SequenceNumber,
			rtpPacket.Header.Timestamp,
			rtpPacket.Header.Marker,
			rtpPacket.Header.MarshalSize(),
			len(rtpPacket.Payload),
			int(rtpPacket.Header.PaddingSize),
		)
	default:
		return 0, fmt.Errorf("unhandled reason: %s", flowState.UnhandledReason.String())
	}

	if b.params.IsOOBSequenceNumber {
		b.updateOOBNACKState(oobSequenceNumber, arrivalTime, len(rawPkt))
	} else {
		b.updateNACKState(rtpPacket.SequenceNumber, flowState)
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
		return 0, errors.New("padding only packet")
	}

	if !flowState.IsOutOfOrder && rtpPacket.PayloadType != b.payloadType && b.codecType == webrtc.RTPCodecTypeVideo {
		b.logger.Infow("possible codec change", "oldPT", b.payloadType, "receivedPT", rtpPacket.PayloadType)
		b.handleCodecChange(rtpPacket.PayloadType)
	}

	// add to RTX buffer using sequence number after accounting for dropped padding only packets
	snAdjustment, err := b.snRangeMap.GetValue(flowState.ExtSequenceNumber)
	if err != nil {
		b.logger.Errorw(
			"could not get sequence number adjustment", err,
			"sequenceNumber", rtpPacket.SequenceNumber,
			"extSequenceNumber", flowState.ExtSequenceNumber,
			"timestamp", rtpPacket.Timestamp,
			"extTimestamp", flowState.ExtTimestamp,
			"payloadSize", len(rtpPacket.Payload),
			"paddingSize", rtpPacket.Header.PaddingSize,
			"rtpStats", b.rtpStats,
			"rtpStatsLite", b.rtpStatsLite,
			"snRangeMap", b.snRangeMap,
		)
		return 0, err
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
		return 0, err
	}

	ep := b.getExtPacket(rtpPacket, arrivalTime, isBuffered, flowState)
	if ep == nil {
		return 0, errors.New("could not get ext packet")
	}
	b.extPackets.PushBack(ep)
	if b.videoFrameCacheEnabled && b.codecType == webrtc.RTPCodecTypeVideo {
		b.markVideoFrameCacheLocked(ep)
	}
	b.readCond.Broadcast()

	if b.extPackets.Len() > b.bucket.Capacity() {
		if (b.extPacketTooMuchCount.Inc()-1)%100 == 0 {
			b.logger.Warnw("too much ext packets", nil, "count", b.extPackets.Len())
		}
	}

	b.maybeGrowBucket(arrivalTime)

	return ep.ExtSequenceNumber, nil
}

func (b *BufferBase) updateNACKState(sequenceNumber uint16, flowState rtpstats.RTPFlowState) {
	if b.nacker == nil {
		return
	}

	b.nacker.Remove(sequenceNumber)

	for lost := flowState.LossStartInclusive; lost != flowState.LossEndExclusive; lost++ {
		b.nacker.Push(uint16(lost))
	}
}

func (b *BufferBase) updateOOBNACKState(sequenceNumber uint16, arrivalTime int64, size int) {
	if b.nacker == nil || !b.params.IsOOBSequenceNumber {
		return
	}

	fsLite := b.rtpStatsLite.Update(arrivalTime, size, sequenceNumber)
	if fsLite.IsNotHandled {
		return
	}

	b.nacker.Remove(sequenceNumber)

	for lost := fsLite.LossStartInclusive; lost != fsLite.LossEndExclusive; lost++ {
		b.nacker.Push(uint16(lost))
	}
}

func (b *BufferBase) processAudioSsrcLevelHeaderExtension(p *rtp.Packet, arrivalTime int64) {
	if b.audioLevelExtID == 0 {
		return
	}

	if e := p.GetExtension(b.audioLevelExtID); e != nil {
		ext := rtp.AudioLevelExtension{}
		if err := ext.Unmarshal(e); err == nil {
			b.audioLevel.ObserveWithRTPTimestamp(ext.Level, p.Timestamp, arrivalTime)
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
		b.logger.Errorw(
			"could not find codec for new payload type", nil,
			"pt", newPT,
			"rtpParameters", b.rtpParameters,
		)
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

	if f := b.onCodecChange; f != nil {
		go f(newCodec)
	}
}

func (b *BufferBase) getExtPacket(
	rtpPacket *rtp.Packet,
	arrivalTime int64,
	isBuffered bool,
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
	var videoSize []codec.VideoSize
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
		vp8Packet := codec.VP8{}
		if err := vp8Packet.Unmarshal(ep.Packet.Payload); err != nil {
			b.logger.Warnw("could not unmarshal VP8 packet", err)
			return err
		}
		ep.IsKeyFrame = vp8Packet.IsKeyFrame
		if ep.DependencyDescriptor == nil {
			ep.Temporal = int32(vp8Packet.TID)

			if ep.IsKeyFrame {
				if sz := codec.ExtractVP8VideoSize(&vp8Packet, ep.Packet.Payload); sz.Width > 0 && sz.Height > 0 {
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
			ep.IsKeyFrame = codec.IsVP9KeyFrame(&vp9Packet, ep.Packet.Payload)

			if ep.IsKeyFrame {
				for i := 0; i < len(vp9Packet.Width); i++ {
					videoSize = append(videoSize, codec.VideoSize{
						Width:  uint32(vp9Packet.Width[i]),
						Height: uint32(vp9Packet.Height[i]),
					})
				}
			}
		} else {
			ep.IsKeyFrame = codec.IsVP9KeyFrame(nil, ep.Packet.Payload)
		}

	case mime.MimeTypeH264:
		ep.IsKeyFrame = codec.IsH264KeyFrame(ep.Packet.Payload)
		ep.Spatial = InvalidLayerSpatial // h.264 don't have spatial scalability, reset to invalid

		// Check H264 key frame video size
		if ep.IsKeyFrame {
			if sz := codec.ExtractH264VideoSize(ep.Packet.Payload); sz.Width > 0 && sz.Height > 0 {
				videoSize = append(videoSize, sz)
			}
		}

	case mime.MimeTypeAV1:
		ep.IsKeyFrame = codec.IsAV1KeyFrame(ep.Packet.Payload)

	case mime.MimeTypeH265:
		ep.IsKeyFrame = codec.IsH265KeyFrame(ep.Packet.Payload)
		if ep.DependencyDescriptor == nil {
			if len(ep.Packet.Payload) < 2 {
				b.logger.Warnw("invalid H265 packet", nil, "payloadLen", len(ep.Packet.Payload))
				return errors.New("invalid H265 packet")
			}
			ep.VideoLayer = VideoLayer{
				Temporal: int32(ep.Packet.Payload[1]&0x07) - 1,
			}
			ep.Spatial = InvalidLayerSpatial

			if ep.IsKeyFrame {
				if sz := codec.ExtractH265VideoSize(ep.Packet.Payload); sz.Width > 0 && sz.Height > 0 {
					videoSize = append(videoSize, sz)
				}
			}
		}
	}

	if ep.IsKeyFrame {
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
	n, err := b.getPacketLocked(buf, ep.ExtSequenceNumber)
	if err != nil {
		packetNotFoundCount := b.packetNotFoundCount.Inc()
		if (packetNotFoundCount-1)%20 == 0 {
			b.logger.Warnw(
				"could not get packet from bucket", err,
				"sn", ep.Packet.SequenceNumber,
				"headESN", b.bucket.HeadSequenceNumber(),
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

// bucketGrowTarget computes how many packets the retransmit bucket should retain (targetPkts) and
// the cap that allows growing to it (effectiveMaxPkts), given the measured packets-per-second.
//
// Normally the target is ~1s of packets (the NACK / retransmit window), bounded by maxPkts. When
// videoFrameCacheSizing is set (the video frame cache is enabled with a positive duration), the bucket must retain
// the whole video frame cache group duration plus ~0.5s margin so the key frame is not evicted before it is at most
// videoFrameCacheMaxDuration old; the cap is raised to fit since the default maxPkts is only ~1s worth.
func bucketGrowTarget(pps, maxPkts int, videoFrameCacheSizing bool, videoFrameCacheMaxDuration time.Duration) (targetPkts, effectiveMaxPkts int) {
	targetPkts = pps
	effectiveMaxPkts = maxPkts
	if videoFrameCacheSizing && videoFrameCacheMaxDuration > 0 {
		targetPkts = int(float64(pps)*videoFrameCacheMaxDuration.Seconds()) + pps/2
		if targetPkts > effectiveMaxPkts {
			effectiveMaxPkts = targetPkts
		}
	}
	return
}

func (b *BufferBase) maybeGrowBucket(now int64) {
	if now-b.lastBucketCapCheckAt < bucketCapCheckInterval {
		return
	}

	b.lastBucketCapCheckAt = now

	// check and allocate in a go routine, away from the forwarding path
	go func() {
		b.Lock()
		defer b.Unlock()

		cap := b.bucket.Capacity()
		maxPkts := b.params.MaxVideoPkts
		if b.codecType == webrtc.RTPCodecTypeAudio {
			maxPkts = b.params.MaxAudioPkts
		}

		// when the video frame cache is enabled the bucket must retain the whole configured video frame cache group
		// duration (the key frame must survive until GetVideoFrameCache reads it), which is more than the normal
		// ~1s retransmit window. In that case the target / cap are computed below from pps; otherwise
		// keep the original fast path.
		videoFrameCacheSizing := b.codecType == webrtc.RTPCodecTypeVideo && b.videoFrameCacheEnabled && b.videoFrameCacheMaxDuration > 0
		if cap >= maxPkts && !videoFrameCacheSizing {
			return
		}

		deltaInfo := b.rtpStats.DeltaInfo(b.ppsSnapshotId)
		if deltaInfo == nil {
			return
		}
		duration := deltaInfo.EndTime.Sub(deltaInfo.StartTime)
		if duration < 500*time.Millisecond {
			return
		}
		pps := int(time.Duration(deltaInfo.Packets) * time.Second / duration)

		targetPkts, maxPkts := bucketGrowTarget(pps, maxPkts, videoFrameCacheSizing, b.videoFrameCacheMaxDuration)
		if cap >= maxPkts {
			return
		}

		oldCap := cap
		for targetPkts > cap && cap < maxPkts {
			cap = b.bucket.Grow()
		}
		if cap > oldCap {
			b.logger.Infow(
				"grow bucket",
				"from", oldCap,
				"to", cap,
				"pps", pps,
				"targetPkts", targetPkts,
				"deltaInfo", deltaInfo,
				"rtpStats", b.rtpStats,
			)
		}
	}()
}

func (b *BufferBase) doFpsCalc(ep *ExtPacket) {
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

func (b *BufferBase) GetPacket(buff []byte, esn uint64) (int, error) {
	b.Lock()
	defer b.Unlock()

	return b.getPacketLocked(buff, esn)
}

func (b *BufferBase) getPacketLocked(buff []byte, esn uint64) (int, error) {
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

func (b *BufferBase) GetDeltaStatsLite() *rtpstats.RTPDeltaInfoLite {
	b.RLock()
	defer b.RUnlock()

	if b.rtpStatsLite == nil {
		return nil
	}

	return b.rtpStatsLite.DeltaInfoLite(b.liteStatsSnapshotId)
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

func (b *BufferBase) OnVideoSizeChanged(fn func([]codec.VideoSize)) {
	b.Lock()
	b.onVideoSizeChanged = fn
	b.Unlock()
}

func (b *BufferBase) OnCodecChange(fn func(webrtc.RTPCodecParameters)) {
	b.Lock()
	b.onCodecChange = fn
	b.Unlock()
}

func (b *BufferBase) OnStreamRestart(fn func(string)) {
	b.Lock()
	b.onStreamRestart = fn
	b.Unlock()
}

// checkVideoSizeChange checks if video size has changed for a specific spatial layer and fires callback
func (b *BufferBase) checkVideoSizeChange(videoSizes []codec.VideoSize) {
	if len(videoSizes) > len(b.currentVideoSize) {
		b.logger.Warnw(
			"video size index out of range", nil,
			"newSize", videoSizes,
			"currentVideoSize", b.currentVideoSize,
		)
		return
	}

	if len(videoSizes) < len(b.currentVideoSize) {
		videoSizes = append(videoSizes, make([]codec.VideoSize, len(b.currentVideoSize)-len(videoSizes))...)
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
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	initialCount := uint32(0)
	b.RLock()
	rtpStats := b.rtpStats
	lgr := b.logger
	b.RUnlock()
	lgr.Debugw("starting key frame seeder", "generation", keyFrameSeederGeneration)
	if rtpStats == nil {
		lgr.Debugw("cannot do key frame seeding without stats", "generation", keyFrameSeederGeneration)
		return
	}
	initialCount, _ = rtpStats.KeyFrame()

	for {
		if b.isClosed.Load() || b.keyFrameSeederGeneration.Load() != keyFrameSeederGeneration {
			lgr.Debugw(
				"stopping key frame seeder: stopped",
				"generation", keyFrameSeederGeneration,
				"currentGeneration", b.keyFrameSeederGeneration.Load(),
			)
			return
		}

		select {
		case <-timer.C:
			lgr.Debugw("stopping key frame seeder: timeout", "generation", keyFrameSeederGeneration)
			return

		case <-ticker.C:
			cnt, last := rtpStats.KeyFrame()
			if cnt > initialCount {
				lgr.Debugw(
					"stopping key frame seeder: received key frame",
					"generation", keyFrameSeederGeneration,
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

func (b *BufferBase) GetNACKPairs() []rtcp.NackPair {
	b.RLock()
	defer b.RUnlock()

	return b.GetNACKPairsLocked()
}

func (b *BufferBase) GetNACKPairsLocked() []rtcp.NackPair {
	if b.nacker == nil {
		return nil
	}

	pairs, numSeqNumsNacked := b.nacker.Pairs()
	if !b.params.IsOOBSequenceNumber {
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

	return b.rtpStats.GetRtcpReceptionReport(b.params.SSRC, proxyLoss, b.rrSnapshotId)
}

// ---------------------------------------------------------------

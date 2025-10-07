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

package sfu

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"go.uber.org/zap/zapcore"

	"github.com/livekit/mediatransportutil"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/utils/mono"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/codecmunger"
	"github.com/livekit/livekit-server/pkg/sfu/mime"
	dd "github.com/livekit/livekit-server/pkg/sfu/rtpextension/dependencydescriptor"
	"github.com/livekit/livekit-server/pkg/sfu/rtpstats"
	sfuutils "github.com/livekit/livekit-server/pkg/sfu/utils"
	"github.com/livekit/livekit-server/pkg/sfu/videolayerselector"
	"github.com/livekit/livekit-server/pkg/sfu/videolayerselector/temporallayerselector"
)

const (
	FlagPauseOnDowngrade  = true
	FlagFilterRTX         = false
	FlagFilterRTXLayers   = true
	TransitionCostSpatial = 10

	ResumeBehindThresholdSeconds      = float64(0.2)   // 200ms
	ResumeBehindHighThresholdSeconds  = float64(2.0)   // 2 seconds
	LayerSwitchBehindThresholdSeconds = float64(0.05)  // 50ms
	SwitchAheadThresholdSeconds       = float64(0.025) // 25ms
)

var (
	errSkipStartOnOutOfOrderPacket = errors.New("skip start on out-of-order packet")
	errSwitchPointTooFarBehind     = errors.New("switch point too far behind")
)

// -------------------------------------------------------------------

type VideoPauseReason int

const (
	VideoPauseReasonNone VideoPauseReason = iota
	VideoPauseReasonMuted
	VideoPauseReasonPubMuted
	VideoPauseReasonFeedDry
	VideoPauseReasonBandwidth
)

func (v VideoPauseReason) String() string {
	switch v {
	case VideoPauseReasonNone:
		return "NONE"
	case VideoPauseReasonMuted:
		return "MUTED"
	case VideoPauseReasonPubMuted:
		return "PUB_MUTED"
	case VideoPauseReasonFeedDry:
		return "FEED_DRY"
	case VideoPauseReasonBandwidth:
		return "BANDWIDTH"
	default:
		return fmt.Sprintf("%d", int(v))
	}
}

// -------------------------------------------------------------------

type VideoAllocation struct {
	PauseReason         VideoPauseReason
	IsDeficient         bool
	BandwidthRequested  int64
	BandwidthDelta      int64
	BandwidthNeeded     int64
	Bitrates            Bitrates
	TargetLayer         buffer.VideoLayer
	RequestLayerSpatial int32
	MaxLayer            buffer.VideoLayer
	DistanceToDesired   float64
}

func (v *VideoAllocation) String() string {
	return fmt.Sprintf("VideoAllocation{pause: %s, def: %+v, bwr: %d, del: %d, bwn: %d, rates: %+v, target: %s, req: %d, max: %s, dist: %0.2f}",
		v.PauseReason,
		v.IsDeficient,
		v.BandwidthRequested,
		v.BandwidthDelta,
		v.BandwidthNeeded,
		v.Bitrates,
		v.TargetLayer,
		v.RequestLayerSpatial,
		v.MaxLayer,
		v.DistanceToDesired,
	)
}

func (v *VideoAllocation) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if v == nil {
		return nil
	}

	e.AddString("PauseReason", v.PauseReason.String())
	e.AddBool("IsDeficient", v.IsDeficient)
	e.AddInt64("BandwidthRquested", v.BandwidthRequested)
	e.AddInt64("BandwidthDelta", v.BandwidthDelta)
	e.AddInt64("BandwidthNeeded", v.BandwidthNeeded)
	e.AddReflected("Bitrates", v.Bitrates)
	e.AddReflected("TargetLayer", v.TargetLayer)
	e.AddInt32("RequestLayerSpatial", v.RequestLayerSpatial)
	e.AddReflected("MaxLayer", v.MaxLayer)
	e.AddFloat64("DistanceToDesired", v.DistanceToDesired)
	return nil
}

var (
	VideoAllocationDefault = VideoAllocation{
		PauseReason:         VideoPauseReasonFeedDry, // start with no feed till feed is seen
		TargetLayer:         buffer.InvalidLayer,
		RequestLayerSpatial: buffer.InvalidLayerSpatial,
		MaxLayer:            buffer.InvalidLayer,
	}
)

// -------------------------------------------------------------------

type VideoAllocationProvisional struct {
	muted           bool
	pubMuted        bool
	maxSeenLayer    buffer.VideoLayer
	availableLayers []int32
	bitrates        Bitrates
	maxLayer        buffer.VideoLayer
	currentLayer    buffer.VideoLayer
	allocatedLayer  buffer.VideoLayer
}

// -------------------------------------------------------------------

type VideoTransition struct {
	From           buffer.VideoLayer
	To             buffer.VideoLayer
	BandwidthDelta int64
}

func (v *VideoTransition) String() string {
	return fmt.Sprintf("VideoTransition{from: %s, to: %s, del: %d}", v.From, v.To, v.BandwidthDelta)
}

func (v *VideoTransition) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if v == nil {
		return nil
	}

	e.AddReflected("From", v.From)
	e.AddReflected("To", v.To)
	e.AddInt64("BandwidthDelta", v.BandwidthDelta)
	return nil
}

// -------------------------------------------------------------------

type TranslationParams struct {
	shouldDrop         bool
	isResuming         bool
	isSwitching        bool
	rtp                TranslationParamsRTP
	ddBytes            []byte
	incomingHeaderSize int
	codecBytes         []byte
	marker             bool
}

// -------------------------------------------------------------------

type refInfo struct {
	senderReport    *livekit.RTCPSenderReportState
	tsOffset        uint64
	isTSOffsetValid bool
}

func (r refInfo) MarshalLogObject(e zapcore.ObjectEncoder) error {
	e.AddObject("senderReport", rtpstats.WrappedRTCPSenderReportStateLogger{
		RTCPSenderReportState: r.senderReport,
	})
	e.AddUint64("tsOffset", r.tsOffset)
	e.AddBool("isTSOffsetValid", r.isTSOffsetValid)
	return nil
}

// -------------------------------------------------------------------

type Forwarder struct {
	lock            sync.RWMutex
	mime            mime.MimeType
	clockRate       uint32
	kind            webrtc.RTPCodecType
	logger          logger.Logger
	skipReferenceTS bool
	rtpStats        *rtpstats.RTPStatsSender

	muted                 bool
	pubMuted              bool
	resumeBehindThreshold float64

	started                  bool
	preStartTime             time.Time
	extFirstTS               uint64
	lastSSRC                 uint32
	lastReferencePayloadType int8
	lastSwitchExtIncomingTS  uint64
	referenceLayerSpatial    int32
	dummyStartTSOffset       uint64
	refInfos                 [buffer.DefaultMaxLayerSpatial + 1]refInfo
	refVideoLayerMode        livekit.VideoLayer_Mode
	isDDAvailable            bool

	provisional *VideoAllocationProvisional

	lastAllocation VideoAllocation

	rtpMunger *RTPMunger

	vls videolayerselector.VideoLayerSelector

	codecMunger codecmunger.CodecMunger
}

func NewForwarder(
	kind webrtc.RTPCodecType,
	logger logger.Logger,
	skipReferenceTS bool,
	rtpStats *rtpstats.RTPStatsSender,
) *Forwarder {
	f := &Forwarder{
		mime:                     mime.MimeTypeUnknown,
		kind:                     kind,
		logger:                   logger,
		skipReferenceTS:          skipReferenceTS,
		rtpStats:                 rtpStats,
		referenceLayerSpatial:    buffer.InvalidLayerSpatial,
		lastAllocation:           VideoAllocationDefault,
		lastReferencePayloadType: -1,
		rtpMunger:                NewRTPMunger(logger),
		vls:                      videolayerselector.NewNull(logger),
		codecMunger:              codecmunger.NewNull(logger),
	}

	if f.kind == webrtc.RTPCodecTypeVideo {
		f.vls.SetMaxTemporal(buffer.DefaultMaxLayerTemporal)
	}
	return f
}

func (f *Forwarder) SetMaxPublishedLayer(maxPublishedLayer int32) bool {
	f.lock.Lock()
	defer f.lock.Unlock()

	existingMaxSeen := f.vls.GetMaxSeen()
	if maxPublishedLayer <= existingMaxSeen.Spatial {
		return false
	}

	f.vls.SetMaxSeenSpatial(maxPublishedLayer)
	f.logger.Debugw("setting max published layer", "layer", maxPublishedLayer)
	return true
}

func (f *Forwarder) SetMaxTemporalLayerSeen(maxTemporalLayerSeen int32) bool {
	f.lock.Lock()
	defer f.lock.Unlock()

	existingMaxSeen := f.vls.GetMaxSeen()
	if maxTemporalLayerSeen <= existingMaxSeen.Temporal {
		return false
	}

	f.vls.SetMaxSeenTemporal(maxTemporalLayerSeen)
	f.logger.Debugw("setting max temporal layer seen", "maxTemporalLayerSeen", maxTemporalLayerSeen)
	return true
}

func (f *Forwarder) DetermineCodec(codec webrtc.RTPCodecCapability, extensions []webrtc.RTPHeaderExtensionParameter, videoLayerMode livekit.VideoLayer_Mode) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if videoLayerMode == livekit.VideoLayer_ONE_SPATIAL_LAYER_PER_STREAM_INCOMPLETE_RTCP_SR {
		f.skipReferenceTS = true
	}

	toMimeType := mime.NormalizeMimeType(codec.MimeType)
	codecChanged := f.mime != mime.MimeTypeUnknown && f.mime != toMimeType
	if codecChanged {
		f.logger.Debugw("forwarder codec changed", "from", f.mime, "to", toMimeType)
	}
	f.mime = toMimeType
	f.clockRate = codec.ClockRate
	f.refVideoLayerMode = videoLayerMode

	ddAvailable := func(exts []webrtc.RTPHeaderExtensionParameter) bool {
		for _, ext := range exts {
			if ext.URI == dd.ExtensionURI {
				return true
			}
		}
		return false
	}

	switch f.mime {
	case mime.MimeTypeVP8:
		f.codecMunger = codecmunger.NewVP8FromNull(f.codecMunger, f.logger)
		if f.vls != nil {
			if vls := videolayerselector.NewSimulcastFromOther(f.vls); vls != nil {
				f.vls = vls
			} else {
				f.logger.Errorw("failed to create simulcast on codec change", nil)
			}
		} else {
			f.vls = videolayerselector.NewSimulcast(f.logger)
		}
		f.vls.SetTemporalLayerSelector(temporallayerselector.NewVP8(f.logger))

	case mime.MimeTypeH264, mime.MimeTypeH265:
		if f.vls != nil {
			if vls := videolayerselector.NewSimulcastFromOther(f.vls); vls != nil {
				f.vls = vls
			} else {
				f.logger.Errorw("failed to create simulcast on codec change", nil)
			}
		} else {
			f.vls = videolayerselector.NewSimulcast(f.logger)
		}

	case mime.MimeTypeVP9:
		if sfuutils.IsSimulcastMode(videoLayerMode) {
			if f.vls != nil {
				f.vls = videolayerselector.NewSimulcastFromOther(f.vls)
			} else {
				f.vls = videolayerselector.NewDependencyDescriptor(f.logger)
			}
		} else {
			f.isDDAvailable = ddAvailable(extensions)
			if f.isDDAvailable {
				if f.vls != nil {
					f.vls = videolayerselector.NewDependencyDescriptorFromOther(f.vls)
				} else {
					f.vls = videolayerselector.NewDependencyDescriptor(f.logger)
				}
			} else {
				if f.vls != nil {
					f.vls = videolayerselector.NewVP9FromOther(f.vls)
				} else {
					f.vls = videolayerselector.NewVP9(f.logger)
				}
			}
		}

	case mime.MimeTypeAV1:
		if sfuutils.IsSimulcastMode(videoLayerMode) {
			if f.vls != nil {
				f.vls = videolayerselector.NewSimulcastFromOther(f.vls)
			} else {
				f.vls = videolayerselector.NewSimulcast(f.logger)
			}
		} else {
			f.isDDAvailable = ddAvailable(extensions)
			if f.isDDAvailable {
				if f.vls != nil {
					f.vls = videolayerselector.NewDependencyDescriptorFromOther(f.vls)
				} else {
					f.vls = videolayerselector.NewDependencyDescriptor(f.logger)
				}
			} else {
				if f.vls != nil {
					f.vls = videolayerselector.NewSimulcastFromOther(f.vls)
				} else {
					f.vls = videolayerselector.NewSimulcast(f.logger)
				}
			}
		}
	}
}

func (f *Forwarder) GetState() *livekit.RTPForwarderState {
	f.lock.RLock()
	defer f.lock.RUnlock()

	if !f.started {
		return nil
	}

	state := &livekit.RTPForwarderState{
		Started:                   f.started,
		ReferenceLayerSpatial:     f.referenceLayerSpatial,
		ExtFirstTimestamp:         f.extFirstTS,
		DummyStartTimestampOffset: f.dummyStartTSOffset,
		RtpMunger:                 f.rtpMunger.GetState(),
	}
	if !f.preStartTime.IsZero() {
		state.PreStartTime = f.preStartTime.UnixNano()
	}

	codecMungerState := f.codecMunger.GetState()
	if vp8MungerState, ok := codecMungerState.(*livekit.VP8MungerState); ok {
		state.CodecMunger = &livekit.RTPForwarderState_Vp8Munger{
			Vp8Munger: vp8MungerState,
		}
	}

	state.SenderReportState = make([]*livekit.RTCPSenderReportState, len(f.refInfos))
	for layer, refInfo := range f.refInfos {
		state.SenderReportState[layer] = utils.CloneProto(refInfo.senderReport)
	}
	return state
}

func (f *Forwarder) SeedState(state *livekit.RTPForwarderState) {
	if state == nil || !state.Started {
		return
	}

	f.lock.Lock()
	defer f.lock.Unlock()

	for layer, rtcpSenderReportState := range state.SenderReportState {
		f.refInfos[layer] = refInfo{}
		if senderReport := utils.CloneProto(rtcpSenderReportState); senderReport != nil && senderReport.NtpTimestamp != 0 {
			f.refInfos[layer].senderReport = senderReport
		}
	}

	f.rtpMunger.SeedState(state.RtpMunger)
	f.codecMunger.SeedState(state.CodecMunger)

	f.started = true
	f.referenceLayerSpatial = state.ReferenceLayerSpatial
	if state.PreStartTime != 0 {
		f.preStartTime = time.Unix(0, state.PreStartTime)
	}
	f.extFirstTS = state.ExtFirstTimestamp
	f.dummyStartTSOffset = state.DummyStartTimestampOffset
}

func (f *Forwarder) Mute(muted bool, isSubscribeMutable bool) bool {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.muted == muted {
		return false
	}

	// Do not mute when paused due to bandwidth limitation.
	// There are two issues
	//   1. Muting means probing cannot happen on this track.
	//   2. Muting also triggers notification to publisher about layers this forwarder needs.
	//      If this forwarder does not need any layer, publisher could turn off all layers.
	// So, muting could lead to not being able to restart the track.
	// To avoid that, ignore mute when paused due to bandwidth limitations.
	//
	// NOTE: The above scenario refers to mute getting triggered due
	// to video stream visibility changes. When a stream is paused, it is possible
	// that the receiver hides the video tile triggering subscription mute.
	// The work around here to ignore mute does ignore an intentional mute.
	// It could result in some bandwidth consumed for stream without visibility in
	// the case of intentional mute.
	if muted && !isSubscribeMutable {
		f.logger.Debugw("ignoring forwarder mute, paused due to congestion")
		return false
	}

	f.logger.Debugw("setting forwarder mute", "muted", muted)
	f.muted = muted

	// resync when muted so that sequence numbers do not jump on unmute
	if muted {
		f.resyncLocked()
	}

	return true
}

func (f *Forwarder) IsMuted() bool {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.muted
}

func (f *Forwarder) PubMute(pubMuted bool) bool {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.pubMuted == pubMuted {
		return false
	}

	f.logger.Debugw("setting forwarder pub mute", "muted", pubMuted)
	f.pubMuted = pubMuted

	// resync when pub muted so that sequence numbers do not jump on unmute
	if pubMuted {
		f.resyncLocked()
	}
	return true
}

func (f *Forwarder) IsPubMuted() bool {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.pubMuted
}

func (f *Forwarder) IsAnyMuted() bool {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.muted || f.pubMuted
}

func (f *Forwarder) SetMaxSpatialLayer(spatialLayer int32) (bool, buffer.VideoLayer) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.kind == webrtc.RTPCodecTypeAudio {
		return false, buffer.InvalidLayer
	}

	existingMax := f.vls.GetMax()
	if spatialLayer == existingMax.Spatial {
		return false, existingMax
	}

	f.logger.Debugw("setting max spatial layer", "layer", spatialLayer)
	f.vls.SetMaxSpatial(spatialLayer)
	return true, f.vls.GetMax()
}

func (f *Forwarder) SetMaxTemporalLayer(temporalLayer int32) (bool, buffer.VideoLayer) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.kind == webrtc.RTPCodecTypeAudio {
		return false, buffer.InvalidLayer
	}

	existingMax := f.vls.GetMax()
	if temporalLayer == existingMax.Temporal {
		return false, existingMax
	}

	f.logger.Debugw("setting max temporal layer", "layer", temporalLayer)
	f.vls.SetMaxTemporal(temporalLayer)
	return true, f.vls.GetMax()
}

func (f *Forwarder) MaxLayer() buffer.VideoLayer {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.vls.GetMax()
}

func (f *Forwarder) CurrentLayer() buffer.VideoLayer {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.vls.GetCurrent()
}

func (f *Forwarder) TargetLayer() buffer.VideoLayer {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.vls.GetTarget()
}

func (f *Forwarder) GetMaxSubscribedSpatial() int32 {
	f.lock.RLock()
	defer f.lock.RUnlock()

	layer := buffer.InvalidLayerSpatial // covers muted case
	if !f.muted {
		layer = f.vls.GetMax().Spatial

		// If current is higher, mark the current layer as max subscribed layer
		// to prevent the current layer from stopping before forwarder switches
		// to the new and lower max layer,
		if layer < f.vls.GetCurrent().Spatial {
			layer = f.vls.GetCurrent().Spatial
		}

		// if reference layer is higher, hold there until an RTCP Sender Report from
		// publisher is available as that is used for reference time stamp between layers.
		if f.referenceLayerSpatial != buffer.InvalidLayerSpatial &&
			layer < f.referenceLayerSpatial &&
			f.refInfos[f.referenceLayerSpatial].senderReport == nil {
			layer = f.referenceLayerSpatial
		}
	}

	return layer
}

func (f *Forwarder) getRefLayer() (int32, int32) {
	if f.lastSSRC == 0 {
		return buffer.InvalidLayerSpatial, buffer.InvalidLayerSpatial
	}

	if f.kind == webrtc.RTPCodecTypeAudio {
		return 0, 0
	}

	currentLayerSpatial := f.vls.GetCurrent().Spatial
	if currentLayerSpatial < 0 || currentLayerSpatial > buffer.DefaultMaxLayerSpatial {
		return buffer.InvalidLayerSpatial, buffer.InvalidLayerSpatial
	}

	if f.refVideoLayerMode == livekit.VideoLayer_MULTIPLE_SPATIAL_LAYERS_PER_STREAM {
		return 0, currentLayerSpatial
	}

	return currentLayerSpatial, currentLayerSpatial
}

func (f *Forwarder) SetRefSenderReport(layer int32, srData *livekit.RTCPSenderReportState) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if layer >= 0 && int(layer) < len(f.refInfos) {
		if layer == f.referenceLayerSpatial && f.refInfos[layer].senderReport == nil {
			f.logger.Debugw("received RTCP sender report for reference layer spatial", "layer", layer)
		}
		f.refInfos[layer] = refInfo{srData, 0, false}

		// Mark validity of time stamp offset.
		//
		// It is possible to implement mute using pause/unpause
		// which can be implemented using replaceTrack(null)/replaceTrack(track).
		// In those cases, the RTP time stamp may not jump across
		// the mute/pause valley (for the time it is replaced with null track).
		// So, relying on a report that happened before unmute/unpause
		// could result in incorrect RTCP sender report on subscriber side.
		//
		// It could happen like this
		//   1. Normal operation: publisher sending sender reports and
		//      suscribers use reports from publisher to calculate and send
		//      RTCP sender report.
		//   2. Publisher pauses: there are no more reports.
		//   3. When paused, subscriber can still use the publisher side sender
		//      report to send reports. Although the time since last publisher
		//      sender report is increasing, the reports would still be correct
		//      as they referencing a previous (albeit older) correct report.
		//   4. Publisher unpauses after 20 seconds. But, it may not have advanced
		//      RTP Timestamp by that much. Let us say, it advances only by 5 seconds.
		//   5. When subscriber starts forwarding packets, it will calculate
		//      a new time stamp offset to adjust to the new time stamp of publisher.
		//   6. But, when that same offset is used on an old publisher sender report
		//      (i. e. a report from before the pause), the subscriber side sender
		//      reports jumps ahead in time by 15 seconds.
		//
		// So, mark valid for reports after last switch.
		refLayer, _ := f.getRefLayer()
		if layer == refLayer && srData.RtpTimestampExt >= f.lastSwitchExtIncomingTS {
			f.refInfos[layer].tsOffset = f.rtpMunger.GetTSOffset()
			f.refInfos[layer].isTSOffsetValid = true
		}
	}
}

func (f *Forwarder) GetSenderReportParams() (int32, bool, uint64, *livekit.RTCPSenderReportState) {
	f.lock.RLock()
	defer f.lock.RUnlock()

	refLayer, currentLayerSpatial := f.getRefLayer()
	if refLayer == buffer.InvalidLayerSpatial ||
		f.refInfos[refLayer].senderReport == nil ||
		!f.refInfos[refLayer].isTSOffsetValid {
		return buffer.InvalidLayerSpatial, false, 0, nil
	}

	return currentLayerSpatial, f.refVideoLayerMode == livekit.VideoLayer_MULTIPLE_SPATIAL_LAYERS_PER_STREAM, f.refInfos[refLayer].tsOffset, f.refInfos[refLayer].senderReport
}

func (f *Forwarder) isDeficientLocked() bool {
	return f.lastAllocation.IsDeficient
}

func (f *Forwarder) IsDeficient() bool {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.isDeficientLocked()
}

func (f *Forwarder) PauseReason() VideoPauseReason {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.lastAllocation.PauseReason
}

func (f *Forwarder) BandwidthRequested(brs Bitrates) int64 {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return getBandwidthNeeded(brs, f.vls.GetTarget(), f.lastAllocation.BandwidthRequested)
}

func (f *Forwarder) DistanceToDesired(availableLayers []int32, brs Bitrates) float64 {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return getDistanceToDesired(
		f.muted,
		f.pubMuted,
		f.vls.GetMaxSeen(),
		availableLayers,
		brs,
		f.vls.GetTarget(),
		f.vls.GetMax(),
	)
}

func (f *Forwarder) GetOptimalBandwidthNeeded(brs Bitrates) int64 {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return getOptimalBandwidthNeeded(f.muted, f.pubMuted, f.vls.GetMaxSeen().Spatial, brs, f.vls.GetMax())
}

func (f *Forwarder) AllocateOptimal(availableLayers []int32, brs Bitrates, allowOvershoot bool, hold bool) VideoAllocation {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.kind == webrtc.RTPCodecTypeAudio {
		return f.lastAllocation
	}

	maxLayer := f.vls.GetMax()
	maxSeenLayer := f.vls.GetMaxSeen()
	currentLayer := f.vls.GetCurrent()
	requestSpatial := f.vls.GetRequestSpatial()
	alloc := VideoAllocation{
		PauseReason:         VideoPauseReasonNone,
		Bitrates:            brs,
		TargetLayer:         buffer.InvalidLayer,
		RequestLayerSpatial: requestSpatial,
		MaxLayer:            maxLayer,
	}
	optimalBandwidthNeeded := getOptimalBandwidthNeeded(f.muted, f.pubMuted, maxSeenLayer.Spatial, brs, maxLayer)
	if optimalBandwidthNeeded == 0 {
		alloc.PauseReason = VideoPauseReasonFeedDry
	}
	alloc.BandwidthNeeded = optimalBandwidthNeeded

	getMaxTemporal := func() int32 {
		maxTemporal := maxLayer.Temporal
		if maxSeenLayer.Temporal != buffer.InvalidLayerTemporal && maxSeenLayer.Temporal < maxTemporal {
			maxTemporal = maxSeenLayer.Temporal
		}
		return maxTemporal
	}

	opportunisticAlloc := func() {
		// opportunistically latch on to anything
		maxSpatial := maxLayer.Spatial
		if allowOvershoot && f.vls.IsOvershootOkay() && maxSeenLayer.Spatial > maxSpatial {
			maxSpatial = maxSeenLayer.Spatial
		}

		alloc.TargetLayer = buffer.VideoLayer{
			Spatial:  min(maxSeenLayer.Spatial, maxSpatial),
			Temporal: getMaxTemporal(),
		}
	}

	switch {
	case !maxLayer.IsValid() || maxSeenLayer.Spatial == buffer.InvalidLayerSpatial:
		// nothing to do when max layers are not valid OR max published layer is invalid

	case f.muted:
		alloc.PauseReason = VideoPauseReasonMuted

	case f.pubMuted:
		alloc.PauseReason = VideoPauseReasonPubMuted

	default:
		// lots of different events could end up here
		//   1. Publisher side layer resuming/stopping
		//   2. Bitrate becoming available
		//   3. New max published spatial layer or max temporal layer seen
		//   4. Subscriber layer changes
		//
		// to handle all of the above
		//   1. Find highest that can be requested - takes into account available layers and overshoot.
		//      This should catch scenarios like layers resuming/stopping.
		//   2. If current is a valid layer, check against currently available layers and continue at current
		//      if possible. Else, choose the highest available layer as the next target.
		//   3. If current is not valid, set next target to be opportunistic.
		maxLayerSpatialLimit := min(maxLayer.Spatial, maxSeenLayer.Spatial)
		highestAvailableLayer := buffer.InvalidLayerSpatial
		lowestAvailableLayer := buffer.InvalidLayerSpatial
		requestLayerSpatial := buffer.InvalidLayerSpatial
		for _, al := range availableLayers {
			if al > requestLayerSpatial && al <= maxLayerSpatialLimit {
				requestLayerSpatial = al
			}
			if al > highestAvailableLayer {
				highestAvailableLayer = al
			}
			if lowestAvailableLayer == buffer.InvalidLayerSpatial || al < lowestAvailableLayer {
				lowestAvailableLayer = al
			}
		}
		if requestLayerSpatial == buffer.InvalidLayerSpatial && highestAvailableLayer != buffer.InvalidLayerSpatial && allowOvershoot && f.vls.IsOvershootOkay() {
			requestLayerSpatial = highestAvailableLayer
		}

		if currentLayer.IsValid() {
			if (requestLayerSpatial == requestSpatial && currentLayer.Spatial == requestSpatial) || requestLayerSpatial == buffer.InvalidLayerSpatial {
				// 1. current is locked to desired, stay there
				// OR
				// 2. feed may be dry, let it continue at current layer if valid.
				// covers the cases of
				//   1. mis-detection of layer stop - can continue streaming
				//   2. current layer resuming - can latch on when it starts
				alloc.TargetLayer = buffer.VideoLayer{
					Spatial:  currentLayer.Spatial,
					Temporal: getMaxTemporal(),
				}
			} else {
				// current layer has stopped, switch to lowest available if `hold`ing, else switch to highest available
				if hold {
					// if `hold` is requested, may be set due to early warning congestion
					// signal, in that case layers are not increased as increasing layers
					// will result in more load on the channel
					alloc.TargetLayer = buffer.VideoLayer{
						Spatial:  lowestAvailableLayer,
						Temporal: 0,
					}
				} else {
					alloc.TargetLayer = buffer.VideoLayer{
						Spatial:  requestLayerSpatial,
						Temporal: getMaxTemporal(),
					}
				}
			}
			alloc.RequestLayerSpatial = alloc.TargetLayer.Spatial
		} else {
			if hold {
				// allocate minimal to make the stream active while `hold`ing.
				if lowestAvailableLayer == buffer.InvalidLayerSpatial {
					alloc.TargetLayer = buffer.VideoLayer{
						Spatial:  0,
						Temporal: 0,
					}
				} else {
					alloc.TargetLayer = buffer.VideoLayer{
						Spatial:  lowestAvailableLayer,
						Temporal: 0,
					}
				}
				alloc.RequestLayerSpatial = alloc.TargetLayer.Spatial
			} else {
				// opportunistically latch on to anything
				opportunisticAlloc()
				if requestLayerSpatial == buffer.InvalidLayerSpatial {
					alloc.RequestLayerSpatial = maxLayerSpatialLimit
				} else {
					alloc.RequestLayerSpatial = requestLayerSpatial
				}
			}
		}
	}

	if !alloc.TargetLayer.IsValid() {
		alloc.TargetLayer = buffer.InvalidLayer
		alloc.RequestLayerSpatial = buffer.InvalidLayerSpatial
	}
	if alloc.TargetLayer.IsValid() {
		alloc.BandwidthRequested = getOptimalBandwidthNeeded(f.muted, f.pubMuted, maxSeenLayer.Spatial, brs, alloc.TargetLayer)
	}
	alloc.BandwidthDelta = alloc.BandwidthRequested - getBandwidthNeeded(brs, f.vls.GetTarget(), f.lastAllocation.BandwidthRequested)
	alloc.DistanceToDesired = getDistanceToDesired(
		f.muted,
		f.pubMuted,
		f.vls.GetMaxSeen(),
		availableLayers,
		brs,
		alloc.TargetLayer,
		f.vls.GetMax(),
	)

	return f.updateAllocation(alloc, "optimal")
}

func (f *Forwarder) ProvisionalAllocatePrepare(availableLayers []int32, bitrates Bitrates) {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.provisional = &VideoAllocationProvisional{
		allocatedLayer: buffer.InvalidLayer,
		muted:          f.muted,
		pubMuted:       f.pubMuted,
		maxSeenLayer:   f.vls.GetMaxSeen(),
		bitrates:       bitrates,
		maxLayer:       f.vls.GetMax(),
		currentLayer:   f.vls.GetCurrent(),
	}

	f.provisional.availableLayers = make([]int32, len(availableLayers))
	copy(f.provisional.availableLayers, availableLayers)
}

func (f *Forwarder) ProvisionalAllocateReset() {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.provisional.allocatedLayer = buffer.InvalidLayer
}

func (f *Forwarder) ProvisionalAllocate(availableChannelCapacity int64, layer buffer.VideoLayer, allowPause bool, allowOvershoot bool) (bool, int64) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.provisional.muted ||
		f.provisional.pubMuted ||
		f.provisional.maxSeenLayer.Spatial == buffer.InvalidLayerSpatial ||
		!f.provisional.maxLayer.IsValid() ||
		((!allowOvershoot || !f.vls.IsOvershootOkay()) && layer.GreaterThan(f.provisional.maxLayer)) {
		return false, 0
	}

	requiredBitrate := f.provisional.bitrates[layer.Spatial][layer.Temporal]
	if requiredBitrate == 0 {
		return false, 0
	}

	alreadyAllocatedBitrate := int64(0)
	if f.provisional.allocatedLayer.IsValid() {
		alreadyAllocatedBitrate = f.provisional.bitrates[f.provisional.allocatedLayer.Spatial][f.provisional.allocatedLayer.Temporal]
	}

	// a layer under maximum fits, take it
	if !layer.GreaterThan(f.provisional.maxLayer) && requiredBitrate <= (availableChannelCapacity+alreadyAllocatedBitrate) {
		f.provisional.allocatedLayer = layer
		return true, requiredBitrate - alreadyAllocatedBitrate
	}

	//
	// Given layer does not fit.
	//
	// Could be one of
	//  1. a layer below maximum that does not fit
	//  2. a layer above maximum which may or may not fit, but overshoot is allowed.
	// In any of those cases, take the lowest possible layer if pause is not allowed
	//
	if !allowPause && (!f.provisional.allocatedLayer.IsValid() || !layer.GreaterThan(f.provisional.allocatedLayer)) {
		f.provisional.allocatedLayer = layer
		return true, requiredBitrate - alreadyAllocatedBitrate
	}

	return false, 0
}

func (f *Forwarder) ProvisionalAllocateGetCooperativeTransition(allowOvershoot bool) (VideoTransition, []int32, Bitrates) {
	//
	// This is called when a track needs a change (could be mute/unmute, subscribed layers changed, published layers changed)
	// when channel is congested.
	//
	// The goal is to provide a co-operative transition. Co-operative stream allocation aims to keep all the streams active
	// as much as possible.
	//
	// When channel is congested, effecting a transition which will consume more bits will lead to more congestion.
	// So, this routine does the following
	//   1. When muting, it is not going to increase consumption.
	//   2. If the stream is currently active and the transition needs more bits (higher layers = more bits), do not make the up move.
	//      The higher layer requirement could be due to a new published layer becoming available or subscribed layers changing.
	//   3. If the new target layers are lower than current target, take the move down and save bits.
	//   4. If not currently streaming, find the minimum layers that can unpause the stream.
	//
	// To summarize, co-operative streaming means
	//   - Try to keep tracks streaming, i.e. no pauses at the expense of some streams not being at optimal layers
	//   - Do not make an upgrade as it could affect other tracks
	//
	f.lock.Lock()
	defer f.lock.Unlock()

	existingTargetLayer := f.vls.GetTarget()
	if f.provisional.muted || f.provisional.pubMuted {
		f.provisional.allocatedLayer = buffer.InvalidLayer
		return VideoTransition{
			From:           existingTargetLayer,
			To:             f.provisional.allocatedLayer,
			BandwidthDelta: -getBandwidthNeeded(f.provisional.bitrates, existingTargetLayer, f.lastAllocation.BandwidthRequested),
		}, f.provisional.availableLayers, f.provisional.bitrates
	}

	// check if we should preserve current target
	if existingTargetLayer.IsValid() {
		// what is the highest that is available
		maximalLayer := buffer.InvalidLayer
		maximalBandwidthRequired := int64(0)
		for s := f.provisional.maxLayer.Spatial; s >= 0; s-- {
			for t := f.provisional.maxLayer.Temporal; t >= 0; t-- {
				if f.provisional.bitrates[s][t] != 0 {
					maximalLayer = buffer.VideoLayer{Spatial: s, Temporal: t}
					maximalBandwidthRequired = f.provisional.bitrates[s][t]
					break
				}
			}

			if maximalBandwidthRequired != 0 {
				break
			}
		}

		if maximalLayer.IsValid() {
			if !existingTargetLayer.GreaterThan(maximalLayer) && f.provisional.bitrates[existingTargetLayer.Spatial][existingTargetLayer.Temporal] != 0 {
				// currently streaming and maybe wanting an upgrade (existingTargetLayer <= maximalLayer),
				// just preserve current target in the cooperative scheme of things
				f.provisional.allocatedLayer = existingTargetLayer
				return VideoTransition{
					From:           existingTargetLayer,
					To:             existingTargetLayer,
					BandwidthDelta: 0,
				}, f.provisional.availableLayers, f.provisional.bitrates
			}

			if existingTargetLayer.GreaterThan(maximalLayer) {
				// maximalLayer < existingTargetLayer, make the down move
				f.provisional.allocatedLayer = maximalLayer
				return VideoTransition{
					From:           existingTargetLayer,
					To:             maximalLayer,
					BandwidthDelta: maximalBandwidthRequired - getBandwidthNeeded(f.provisional.bitrates, existingTargetLayer, f.lastAllocation.BandwidthRequested),
				}, f.provisional.availableLayers, f.provisional.bitrates
			}
		}
	}

	findNextLayer := func(
		minSpatial, maxSpatial int32,
		minTemporal, maxTemporal int32,
	) (buffer.VideoLayer, int64) {
		layers := buffer.InvalidLayer
		bw := int64(0)
		for s := minSpatial; s <= maxSpatial; s++ {
			for t := minTemporal; t <= maxTemporal; t++ {
				if f.provisional.bitrates[s][t] != 0 {
					layers = buffer.VideoLayer{Spatial: s, Temporal: t}
					bw = f.provisional.bitrates[s][t]
					break
				}
			}

			if bw != 0 {
				break
			}
		}

		return layers, bw
	}

	targetLayer := buffer.InvalidLayer
	bandwidthRequired := int64(0)
	if !existingTargetLayer.IsValid() {
		// currently not streaming, find minimal
		// NOTE: a layer in feed could have paused and there could be other options than going back to minimal,
		// but the cooperative scheme knocks things back to minimal
		targetLayer, bandwidthRequired = findNextLayer(
			0, f.provisional.maxLayer.Spatial,
			0, f.provisional.maxLayer.Temporal,
		)

		// could not find a minimal layer, overshoot if allowed
		if bandwidthRequired == 0 && f.provisional.maxLayer.IsValid() && allowOvershoot && f.vls.IsOvershootOkay() {
			targetLayer, bandwidthRequired = findNextLayer(
				f.provisional.maxLayer.Spatial+1, buffer.DefaultMaxLayerSpatial,
				0, buffer.DefaultMaxLayerTemporal,
			)
		}
	}

	// if nothing available, just leave target at current to enable opportunistic forwarding in case current resumes
	if !targetLayer.IsValid() {
		targetLayer = f.provisional.currentLayer
		if targetLayer.IsValid() {
			bandwidthRequired = f.provisional.bitrates[targetLayer.Spatial][targetLayer.Temporal]
		}
	}

	f.provisional.allocatedLayer = targetLayer
	return VideoTransition{
		From:           f.vls.GetTarget(),
		To:             targetLayer,
		BandwidthDelta: bandwidthRequired - getBandwidthNeeded(f.provisional.bitrates, existingTargetLayer, f.lastAllocation.BandwidthRequested),
	}, f.provisional.availableLayers, f.provisional.bitrates
}

func (f *Forwarder) ProvisionalAllocateGetBestWeightedTransition() (VideoTransition, []int32, Bitrates) {
	//
	// This is called when a track needs a change (could be mute/unmute, subscribed layers changed, published layers changed)
	// when channel is congested. This is called on tracks other than the one needing the change. When the track
	// needing the change requires bits, this is called to check if this track can contribute some bits to the pool.
	//
	// The goal is to keep all tracks streaming as much as possible. So, the track that needs a change needs bandwidth to be unpaused.
	//
	// This tries to figure out how much this track can contribute back to the pool to enable the track that needs to be unpaused.
	//   1. Track muted OR feed dry - can contribute everything back in case it was using bandwidth.
	//   2. Look at all possible down transitions from current target and find the best offer.
	//      Best offer is calculated as bandwidth saved moving to a down layer divided by cost.
	//      Cost has two components
	//        a. Transition cost: Spatial layer switch is expensive due to key frame requirement, but temporal layer switch is free.
	//        b. Quality cost: The farther away from desired layers, the higher the quality cost.
	//
	f.lock.Lock()
	defer f.lock.Unlock()

	targetLayer := f.vls.GetTarget()
	if f.provisional.muted || f.provisional.pubMuted {
		f.provisional.allocatedLayer = buffer.InvalidLayer
		return VideoTransition{
			From:           targetLayer,
			To:             f.provisional.allocatedLayer,
			BandwidthDelta: 0 - getBandwidthNeeded(f.provisional.bitrates, targetLayer, f.lastAllocation.BandwidthRequested),
		}, f.provisional.availableLayers, f.provisional.bitrates
	}

	maxReachableLayerTemporal := buffer.InvalidLayerTemporal
	for t := f.provisional.maxLayer.Temporal; t >= 0; t-- {
		for s := f.provisional.maxLayer.Spatial; s >= 0; s-- {
			if f.provisional.bitrates[s][t] != 0 {
				maxReachableLayerTemporal = t
				break
			}
		}
		if maxReachableLayerTemporal != buffer.InvalidLayerTemporal {
			break
		}
	}

	if maxReachableLayerTemporal == buffer.InvalidLayerTemporal {
		// feed has gone dry, just leave target at current to enable opportunistic forwarding in case current resumes.
		// Note that this is giving back bits and opportunistic forwarding resuming might trigger congestion again,
		// but that should be handled by stream allocator.
		f.provisional.allocatedLayer = f.provisional.currentLayer
		return VideoTransition{
			From:           targetLayer,
			To:             f.provisional.allocatedLayer,
			BandwidthDelta: 0 - getBandwidthNeeded(f.provisional.bitrates, targetLayer, f.lastAllocation.BandwidthRequested),
		}, f.provisional.availableLayers, f.provisional.bitrates
	}

	// starting from minimum to target, find transition which gives the best
	// transition taking into account bits saved vs cost of such a transition
	existingBandwidthNeeded := getBandwidthNeeded(f.provisional.bitrates, targetLayer, f.lastAllocation.BandwidthRequested)
	bestLayer := buffer.InvalidLayer
	bestBandwidthDelta := int64(0)
	bestValue := float32(0)
	for s := int32(0); s <= targetLayer.Spatial; s++ {
		for t := int32(0); t <= targetLayer.Temporal; t++ {
			if s == targetLayer.Spatial && t == targetLayer.Temporal {
				break
			}

			bandwidthDelta := max(0, existingBandwidthNeeded-f.provisional.bitrates[s][t])

			transitionCost := int32(0)
			// SVC-TODO: SVC will need a different cost transition
			if targetLayer.Spatial != s {
				transitionCost = TransitionCostSpatial
			}

			qualityCost := (maxReachableLayerTemporal+1)*(targetLayer.Spatial-s) + (targetLayer.Temporal - t)

			value := float32(0)
			if (transitionCost + qualityCost) != 0 {
				value = float32(bandwidthDelta) / float32(transitionCost+qualityCost)
			}
			if value > bestValue || (value == bestValue && bandwidthDelta > bestBandwidthDelta) {
				bestValue = value
				bestBandwidthDelta = bandwidthDelta
				bestLayer = buffer.VideoLayer{Spatial: s, Temporal: t}
			}
		}
	}

	f.provisional.allocatedLayer = bestLayer
	return VideoTransition{
		From:           targetLayer,
		To:             bestLayer,
		BandwidthDelta: -bestBandwidthDelta,
	}, f.provisional.availableLayers, f.provisional.bitrates
}

func (f *Forwarder) ProvisionalAllocateCommit() VideoAllocation {
	f.lock.Lock()
	defer f.lock.Unlock()

	optimalBandwidthNeeded := getOptimalBandwidthNeeded(
		f.provisional.muted,
		f.provisional.pubMuted,
		f.provisional.maxSeenLayer.Spatial,
		f.provisional.bitrates,
		f.provisional.maxLayer,
	)
	alloc := VideoAllocation{
		BandwidthRequested:  0,
		BandwidthDelta:      0 - getBandwidthNeeded(f.provisional.bitrates, f.vls.GetTarget(), f.lastAllocation.BandwidthRequested),
		Bitrates:            f.provisional.bitrates,
		BandwidthNeeded:     optimalBandwidthNeeded,
		TargetLayer:         f.provisional.allocatedLayer,
		RequestLayerSpatial: f.provisional.allocatedLayer.Spatial,
		MaxLayer:            f.provisional.maxLayer,
		DistanceToDesired: getDistanceToDesired(
			f.provisional.muted,
			f.provisional.pubMuted,
			f.provisional.maxSeenLayer,
			f.provisional.availableLayers,
			f.provisional.bitrates,
			f.provisional.allocatedLayer,
			f.provisional.maxLayer,
		),
	}

	switch {
	case f.provisional.muted:
		alloc.PauseReason = VideoPauseReasonMuted

	case f.provisional.pubMuted:
		alloc.PauseReason = VideoPauseReasonPubMuted

	case optimalBandwidthNeeded == 0:
		if f.provisional.allocatedLayer.IsValid() {
			// overshoot
			alloc.BandwidthRequested = f.provisional.bitrates[f.provisional.allocatedLayer.Spatial][f.provisional.allocatedLayer.Temporal]
			alloc.BandwidthDelta = alloc.BandwidthRequested - getBandwidthNeeded(f.provisional.bitrates, f.vls.GetTarget(), f.lastAllocation.BandwidthRequested)
		} else {
			alloc.PauseReason = VideoPauseReasonFeedDry

			// leave target at current for opportunistic forwarding
			if f.provisional.currentLayer.IsValid() && f.provisional.currentLayer.Spatial <= f.provisional.maxLayer.Spatial {
				f.provisional.allocatedLayer = f.provisional.currentLayer
				alloc.TargetLayer = f.provisional.allocatedLayer
				alloc.RequestLayerSpatial = alloc.TargetLayer.Spatial
			}
		}

	default:
		if f.provisional.allocatedLayer.IsValid() {
			alloc.BandwidthRequested = f.provisional.bitrates[f.provisional.allocatedLayer.Spatial][f.provisional.allocatedLayer.Temporal]
		}
		alloc.BandwidthDelta = alloc.BandwidthRequested - getBandwidthNeeded(f.provisional.bitrates, f.vls.GetTarget(), f.lastAllocation.BandwidthRequested)

		if f.provisional.allocatedLayer.GreaterThan(f.provisional.maxLayer) ||
			alloc.BandwidthRequested >= getOptimalBandwidthNeeded(
				f.provisional.muted,
				f.provisional.pubMuted,
				f.provisional.maxSeenLayer.Spatial,
				f.provisional.bitrates,
				f.provisional.maxLayer,
			) {
			// could be greater than optimal if overshooting
			alloc.IsDeficient = false
		} else {
			alloc.IsDeficient = true
			if !f.provisional.allocatedLayer.IsValid() {
				alloc.PauseReason = VideoPauseReasonBandwidth
			}
		}
	}

	return f.updateAllocation(alloc, "cooperative")
}

func (f *Forwarder) AllocateNextHigher(availableChannelCapacity int64, availableLayers []int32, brs Bitrates, allowOvershoot bool) (VideoAllocation, bool) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.kind == webrtc.RTPCodecTypeAudio {
		return f.lastAllocation, false
	}

	// if not deficient, nothing to do
	if !f.isDeficientLocked() {
		return f.lastAllocation, false
	}

	maxLayer := f.vls.GetMax()
	maxSeenLayer := f.vls.GetMaxSeen()
	optimalBandwidthNeeded := getOptimalBandwidthNeeded(f.muted, f.pubMuted, maxSeenLayer.Spatial, brs, maxLayer)

	alreadyAllocated := int64(0)
	targetLayer := f.vls.GetTarget()
	if targetLayer.IsValid() {
		alreadyAllocated = brs[targetLayer.Spatial][targetLayer.Temporal]
	}

	doAllocation := func(
		minSpatial, maxSpatial int32,
		minTemporal, maxTemporal int32,
	) (bool, VideoAllocation, bool) {
		for s := minSpatial; s <= maxSpatial; s++ {
			for t := minTemporal; t <= maxTemporal; t++ {
				bandwidthRequested := brs[s][t]
				if bandwidthRequested == 0 {
					continue
				}

				if (!allowOvershoot || !f.vls.IsOvershootOkay()) && bandwidthRequested-alreadyAllocated > availableChannelCapacity {
					// next higher available layer does not fit, return
					return true, f.lastAllocation, false
				}

				newTargetLayer := buffer.VideoLayer{Spatial: s, Temporal: t}
				alloc := VideoAllocation{
					IsDeficient:         true,
					BandwidthRequested:  bandwidthRequested,
					BandwidthDelta:      bandwidthRequested - alreadyAllocated,
					BandwidthNeeded:     optimalBandwidthNeeded,
					Bitrates:            brs,
					TargetLayer:         newTargetLayer,
					RequestLayerSpatial: newTargetLayer.Spatial,
					MaxLayer:            maxLayer,
					DistanceToDesired: getDistanceToDesired(
						f.muted,
						f.pubMuted,
						maxSeenLayer,
						availableLayers,
						brs,
						newTargetLayer,
						maxLayer,
					),
				}
				if newTargetLayer.GreaterThan(maxLayer) || bandwidthRequested >= optimalBandwidthNeeded {
					alloc.IsDeficient = false
				}

				return true, f.updateAllocation(alloc, "next-higher"), true
			}
		}

		return false, VideoAllocation{}, false
	}

	done := false
	var allocation VideoAllocation
	boosted := false

	// try moving temporal layer up in currently streaming spatial layer
	if targetLayer.IsValid() {
		done, allocation, boosted = doAllocation(
			targetLayer.Spatial, targetLayer.Spatial,
			targetLayer.Temporal+1, maxLayer.Temporal,
		)
		if done {
			return allocation, boosted
		}
	}

	// try moving spatial layer up if temporal layer move up is not available
	done, allocation, boosted = doAllocation(
		targetLayer.Spatial+1, maxLayer.Spatial,
		0, maxLayer.Temporal,
	)
	if done {
		return allocation, boosted
	}

	if allowOvershoot && f.vls.IsOvershootOkay() && maxLayer.IsValid() {
		done, allocation, boosted = doAllocation(
			maxLayer.Spatial+1, buffer.DefaultMaxLayerSpatial,
			0, buffer.DefaultMaxLayerTemporal,
		)
		if done {
			return allocation, boosted
		}
	}

	return f.lastAllocation, false
}

func (f *Forwarder) GetNextHigherTransition(brs Bitrates, allowOvershoot bool) (VideoTransition, bool) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.kind == webrtc.RTPCodecTypeAudio {
		return VideoTransition{}, false
	}

	// if not deficient, nothing to do
	if !f.isDeficientLocked() {
		return VideoTransition{}, false
	}

	// if targets are still pending, don't increase
	targetLayer := f.vls.GetTarget()
	if targetLayer.IsValid() && targetLayer != f.vls.GetCurrent() {
		return VideoTransition{}, false
	}

	alreadyAllocated := int64(0)
	if targetLayer.IsValid() {
		alreadyAllocated = brs[targetLayer.Spatial][targetLayer.Temporal]
	}

	findNextHigher := func(
		minSpatial, maxSpatial int32,
		minTemporal, maxTemporal int32,
	) (bool, VideoTransition, bool) {
		for s := minSpatial; s <= maxSpatial; s++ {
			for t := minTemporal; t <= maxTemporal; t++ {
				bandwidthRequested := brs[s][t]
				// traverse till finding a layer requiring more bits.
				// NOTE: it possible that higher temporal layer of lower spatial layer
				//       could use more bits than lower temporal layer of higher spatial layer.
				if bandwidthRequested == 0 || bandwidthRequested < alreadyAllocated {
					continue
				}

				transition := VideoTransition{
					From:           targetLayer,
					To:             buffer.VideoLayer{Spatial: s, Temporal: t},
					BandwidthDelta: bandwidthRequested - alreadyAllocated,
				}

				return true, transition, true
			}
		}

		return false, VideoTransition{}, false
	}

	done := false
	var transition VideoTransition
	isAvailable := false

	// try moving temporal layer up in currently streaming spatial layer
	maxLayer := f.vls.GetMax()
	if targetLayer.IsValid() {
		done, transition, isAvailable = findNextHigher(
			targetLayer.Spatial, targetLayer.Spatial,
			targetLayer.Temporal+1, maxLayer.Temporal,
		)
		if done {
			return transition, isAvailable
		}
	}

	// try moving spatial layer up if temporal layer move up is not available
	done, transition, isAvailable = findNextHigher(
		targetLayer.Spatial+1, maxLayer.Spatial,
		0, maxLayer.Temporal,
	)
	if done {
		return transition, isAvailable
	}

	if allowOvershoot && f.vls.IsOvershootOkay() && maxLayer.IsValid() {
		done, transition, isAvailable = findNextHigher(
			maxLayer.Spatial+1, buffer.DefaultMaxLayerSpatial,
			0, buffer.DefaultMaxLayerTemporal,
		)
		if done {
			return transition, isAvailable
		}
	}

	return VideoTransition{}, false
}

func (f *Forwarder) Pause(availableLayers []int32, brs Bitrates) VideoAllocation {
	f.lock.Lock()
	defer f.lock.Unlock()

	maxLayer := f.vls.GetMax()
	maxSeenLayer := f.vls.GetMaxSeen()
	optimalBandwidthNeeded := getOptimalBandwidthNeeded(f.muted, f.pubMuted, maxSeenLayer.Spatial, brs, maxLayer)
	alloc := VideoAllocation{
		BandwidthRequested:  0,
		BandwidthDelta:      0 - getBandwidthNeeded(brs, f.vls.GetTarget(), f.lastAllocation.BandwidthRequested),
		Bitrates:            brs,
		BandwidthNeeded:     optimalBandwidthNeeded,
		TargetLayer:         buffer.InvalidLayer,
		RequestLayerSpatial: buffer.InvalidLayerSpatial,
		MaxLayer:            maxLayer,
		DistanceToDesired: getDistanceToDesired(
			f.muted,
			f.pubMuted,
			maxSeenLayer,
			availableLayers,
			brs,
			buffer.InvalidLayer,
			maxLayer,
		),
	}

	switch {
	case f.muted:
		alloc.PauseReason = VideoPauseReasonMuted

	case f.pubMuted:
		alloc.PauseReason = VideoPauseReasonPubMuted

	case optimalBandwidthNeeded == 0:
		alloc.PauseReason = VideoPauseReasonFeedDry

	default:
		// pausing due to lack of bandwidth
		alloc.IsDeficient = true
		alloc.PauseReason = VideoPauseReasonBandwidth
	}

	return f.updateAllocation(alloc, "pause")
}

func (f *Forwarder) updateAllocation(alloc VideoAllocation, reason string) VideoAllocation {
	// restrict target temporal to 0 if codec does not support temporal layers
	if alloc.TargetLayer.IsValid() && f.mime == mime.MimeTypeH264 {
		alloc.TargetLayer.Temporal = 0
	}

	if alloc.IsDeficient != f.lastAllocation.IsDeficient ||
		alloc.PauseReason != f.lastAllocation.PauseReason ||
		alloc.TargetLayer != f.lastAllocation.TargetLayer ||
		alloc.RequestLayerSpatial != f.lastAllocation.RequestLayerSpatial {
		f.logger.Debugw(
			fmt.Sprintf("stream allocation: %s", reason),
			"allocation", &alloc,
			"lastAllocation", &f.lastAllocation,
		)
	}
	f.lastAllocation = alloc

	f.setTargetLayer(f.lastAllocation.TargetLayer, f.lastAllocation.RequestLayerSpatial)
	if !f.vls.GetTarget().IsValid() {
		f.resyncLocked()
	}

	return f.lastAllocation
}

func (f *Forwarder) setTargetLayer(targetLayer buffer.VideoLayer, requestLayerSpatial int32) {
	f.vls.SetTarget(targetLayer)
	if targetLayer.IsValid() {
		f.vls.SetRequestSpatial(requestLayerSpatial)
	} else {
		f.vls.SetRequestSpatial(buffer.InvalidLayerSpatial)
	}
}

func (f *Forwarder) Resync() {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.resyncLocked()
}

func (f *Forwarder) resyncLocked() {
	f.vls.SetCurrent(buffer.InvalidLayer)
	f.lastSSRC = 0
	if f.pubMuted {
		f.resumeBehindThreshold = ResumeBehindThresholdSeconds
	}
}

func (f *Forwarder) CheckSync() (bool, int32) {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.vls.CheckSync()
}

func (f *Forwarder) Restart() {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.resyncLocked()
	f.setTargetLayer(buffer.InvalidLayer, buffer.InvalidLayerSpatial)
	f.referenceLayerSpatial = buffer.InvalidLayerSpatial
	f.lastReferencePayloadType = -1

	for layer := 0; layer < len(f.refInfos); layer++ {
		f.refInfos[layer] = refInfo{}
	}
	f.lastSwitchExtIncomingTS = 0
	f.refVideoLayerMode = livekit.VideoLayer_MODE_UNUSED
}

func (f *Forwarder) FilterRTX(nacks []uint16) (filtered []uint16, disallowedLayers [buffer.DefaultMaxLayerSpatial + 1]bool) {
	f.lock.RLock()
	defer f.lock.RUnlock()

	if !FlagFilterRTX {
		filtered = nacks
	} else {
		filtered = f.rtpMunger.FilterRTX(nacks)
	}

	//
	// Curb RTX when deficient for two cases
	//   1. Target layer is lower than current layer. When current hits target, a key frame should flush the decoder.
	//   2. Requested layer is higher than current. Current layer's key frame should have flushed encoder.
	//      Remote might ask for older layer because of its jitter buffer, but let it starve as channel is already congested.
	//
	// Without the curb, when congestion hits, RTX rate could be so high that it further congests the channel.
	//
	if FlagFilterRTXLayers {
		currentLayer := f.vls.GetCurrent()
		targetLayer := f.vls.GetTarget()
		for layer := int32(0); layer < buffer.DefaultMaxLayerSpatial+1; layer++ {
			if f.isDeficientLocked() && (targetLayer.Spatial < currentLayer.Spatial || layer > currentLayer.Spatial) {
				disallowedLayers[layer] = true
			}
		}
	}
	return
}

func (f *Forwarder) GetTranslationParams(extPkt *buffer.ExtPacket, layer int32) (TranslationParams, error) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.muted || f.pubMuted {
		return TranslationParams{
			shouldDrop: true,
		}, nil
	}

	switch f.kind {
	case webrtc.RTPCodecTypeAudio:
		return f.getTranslationParamsAudio(extPkt, layer)

	case webrtc.RTPCodecTypeVideo:
		return f.getTranslationParamsVideo(extPkt, layer)
	}

	return TranslationParams{
		shouldDrop: true,
	}, ErrUnknownKind
}

func (f *Forwarder) getRefLayerRTPTimestamp(ts uint32, refLayer, targetLayer int32) (uint32, error) {
	if refLayer < 0 || int(refLayer) > len(f.refInfos) || targetLayer < 0 || int(targetLayer) > len(f.refInfos) {
		return 0, fmt.Errorf("invalid layer(s), refLayer: %d, targetLayer: %d", refLayer, targetLayer)
	}

	if refLayer == targetLayer || f.refVideoLayerMode == livekit.VideoLayer_MULTIPLE_SPATIAL_LAYERS_PER_STREAM {
		return ts, nil
	}

	srRef := f.refInfos[refLayer].senderReport
	srTarget := f.refInfos[targetLayer].senderReport
	if srRef == nil || srRef.NtpTimestamp == 0 {
		return 0, fmt.Errorf("unavailable layer ref, refLayer: %d, targetLayer: %d", refLayer, targetLayer)
	}
	if srTarget == nil || srTarget.NtpTimestamp == 0 {
		return 0, fmt.Errorf("unavailable layer target, refLayer: %d, targetLayer: %d", refLayer, targetLayer)
	}

	ntpDiff := mediatransportutil.NtpTime(srRef.NtpTimestamp).Time().Sub(mediatransportutil.NtpTime(srTarget.NtpTimestamp).Time())
	rtpDiff := ntpDiff.Nanoseconds() * int64(f.clockRate) / 1e9

	// calculate other layer's time stamp at the same time as ref layer's NTP time
	normalizedOtherTS := srTarget.RtpTimestamp + uint32(rtpDiff)

	// now both layers' time stamp refer to the same NTP time and the diff is the offset between the layers
	offset := srRef.RtpTimestamp - normalizedOtherTS

	return ts + offset, nil
}

func (f *Forwarder) processSourceSwitch(extPkt *buffer.ExtPacket, layer int32) error {
	if !f.started {
		if extPkt.IsOutOfOrder {
			return errSkipStartOnOutOfOrderPacket
		}

		f.started = true
		f.referenceLayerSpatial = layer
		f.rtpMunger.SetLastSnTs(extPkt)
		f.codecMunger.SetLast(extPkt)
		f.logger.Debugw(
			"starting forwarding",
			"sequenceNumber", extPkt.Packet.SequenceNumber,
			"extSequenceNumber", extPkt.ExtSequenceNumber,
			"timestamp", extPkt.Packet.Timestamp,
			"extTimestamp", extPkt.ExtTimestamp,
			"layer", layer,
			"referenceLayerSpatial", f.referenceLayerSpatial,
		)
		return nil
	} else if f.referenceLayerSpatial == buffer.InvalidLayerSpatial {
		if extPkt.IsOutOfOrder {
			return errSkipStartOnOutOfOrderPacket
		}

		f.referenceLayerSpatial = layer
		f.codecMunger.SetLast(extPkt)
		f.logger.Debugw(
			"catch up forwarding",
			"sequenceNumber", extPkt.Packet.SequenceNumber,
			"extSequenceNumber", extPkt.ExtSequenceNumber,
			"timestamp", extPkt.Packet.Timestamp,
			"extTimestamp", extPkt.ExtTimestamp,
			"layer", layer,
			"referenceLayerSpatial", f.referenceLayerSpatial,
		)
	}

	logTransition := func(message string, extExpectedTS, extRefTS, extLastTS uint64, diffSeconds float64) {
		f.logger.Debugw(
			message,
			"layer", layer,
			"referenceLayerSpatial", f.referenceLayerSpatial,
			"extExpectedTS", extExpectedTS,
			"incomingTS", extPkt.Packet.Timestamp,
			"extIncomingTS", extPkt.ExtTimestamp,
			"extRefTS", extRefTS,
			"extLastTS", extLastTS,
			"diffSeconds", math.Abs(diffSeconds),
			"refInfos", logger.ObjectSlice(f.refInfos[:]),
			"lastSwitchExtIncomingTS", f.lastSwitchExtIncomingTS,
			"rtpStats", f.rtpStats,
		)
	}
	// TODO-REMOVE-AFTER-DATA-COLLECTION
	logTransitionInfo := func(message string, extExpectedTS, extRefTS, extLastTS uint64, diffSeconds float64) {
		f.logger.Infow(
			message,
			"layer", layer,
			"referenceLayerSpatial", f.referenceLayerSpatial,
			"extExpectedTS", extExpectedTS,
			"incomingTS", extPkt.Packet.Timestamp,
			"extIncomingTS", extPkt.ExtTimestamp,
			"extRefTS", extRefTS,
			"extLastTS", extLastTS,
			"diffSeconds", math.Abs(diffSeconds),
			"refInfos", logger.ObjectSlice(f.refInfos[:]),
			"lastSwitchExtIncomingTS", f.lastSwitchExtIncomingTS,
			"rtpStats", f.rtpStats,
		)
	}

	// Compute how much time passed between the previous forwarded packet
	// and the current incoming (to be forwarded) packet and calculate
	// timestamp offset on source change.
	//
	// There are three timestamps to consider here
	//   1. extLastTS -> timestamp of last sent packet
	//   2. extRefTS -> timestamp of this packet (after munging) calculated using feed's RTCP sender report
	//   3. extExpectedTS -> expected timestamp of this packet calculated based on elapsed time since first packet
	// Ideally, extRefTS and extExpectedTS should be very close and extLastTS should be before both of those.
	// But, cases like muting/unmuting, clock vagaries, pacing, etc. make them not satisfy those conditions always.
	rtpMungerState := f.rtpMunger.GetState()
	extLastTS := rtpMungerState.ExtLastTimestamp
	extExpectedTS := extLastTS
	extRefTS := extLastTS
	refTS := uint32(extRefTS)
	switchingAt := mono.Now()
	if !f.skipReferenceTS {
		var err error
		refTS, err = f.getRefLayerRTPTimestamp(extPkt.Packet.Timestamp, f.referenceLayerSpatial, layer)
		if err != nil {
			// error out if refTS is not available. It can happen when there is no sender report
			// for the layer being switched to. Can especially happen at the start of the track when layer switches are
			// potentially happening very quickly. Erroring out and waiting for a layer for which a sender report has been
			// received will calculate a better offset, but may result in initial adaptation to take a bit longer depending
			// on how often publisher/remote side sends RTCP sender report.
			f.logger.Debugw(
				"could not get ref layer timestamp",
				"referenceLayerSpatial", f.referenceLayerSpatial,
				"layer", layer,
				"error", err,
			)
			return err
		}
	}

	// adjust extRefTS to current packet's timestamp mapped to that of reference layer's
	extRefTS = (extRefTS & 0xFFFF_FFFF_0000_0000) + uint64(refTS) + f.dummyStartTSOffset
	lastTS := uint32(extLastTS)
	refTS = uint32(extRefTS)
	if (refTS-lastTS) < 1<<31 && refTS < lastTS {
		extRefTS += (1 << 32)
	}
	if (lastTS-refTS) < 1<<31 && lastTS < refTS && extRefTS >= 1<<32 {
		extRefTS -= (1 << 32)
	}

	if f.rtpStats != nil {
		tsExt, err := f.rtpStats.GetExpectedRTPTimestamp(switchingAt)
		if err == nil {
			extExpectedTS = tsExt
			if f.lastReferencePayloadType == -1 {
				f.dummyStartTSOffset = extExpectedTS - uint64(refTS)
				extRefTS = extExpectedTS
			}
		} else {
			if !f.preStartTime.IsZero() {
				timeSinceFirst := time.Since(f.preStartTime)
				rtpDiff := uint64(timeSinceFirst.Nanoseconds() * int64(f.clockRate) / 1e9)
				extExpectedTS = f.extFirstTS + rtpDiff
				if f.dummyStartTSOffset == 0 {
					f.dummyStartTSOffset = extExpectedTS - uint64(refTS)
					extRefTS = extExpectedTS
					f.logger.Infow(
						"calculating dummyStartTSOffset",
						"preStartTime", f.preStartTime,
						"extFirstTS", f.extFirstTS,
						"timeSinceFirst", timeSinceFirst,
						"rtpDiff", rtpDiff,
						"extRefTS", extRefTS,
						"incomingTS", extPkt.Packet.Timestamp,
						"referenceLayerSpatial", f.referenceLayerSpatial,
						"dummyStartTSOffset", f.dummyStartTSOffset,
					)
				}
			}
		}
	}

	bigJump := false
	var extNextTS uint64
	if f.lastSSRC == 0 {
		// If resuming (e. g. on unmute), keep next timestamp close to expected timestamp.
		//
		// Rationale:
		// Case 1: If mute is implemented via something like stopping a track and resuming it on unmute,
		// the RTP timestamp may not have jumped across mute valley. In this case, old timestamp
		// should not be used.
		//
		// Case 2: OTOH, something like pacing may be adding latency in the publisher path (even if
		// the timestamps incremented correctly across the mute valley). In this case, reference
		// timestamp should be used as things will catch up to real time when channel capacity
		// increases and pacer starts sending at faster rate.
		//
		// But, the challenge is distinguishing between the two cases. As a compromise, the difference
		// between extExpectedTS and extRefTS is thresholded. Difference below the threshold is treated as Case 2
		// and above as Case 1.
		//
		// In the event of extRefTS > extExpectedTS, use extRefTS.
		// Ideally, extRefTS should not be ahead of extExpectedTS, but extExpectedTS uses the first packet's
		// wall clock time. So, if the first packet experienced abmormal latency, it is possible
		// for extRefTS > extExpectedTS
		diffSeconds := float64(int64(extExpectedTS-extRefTS)) / float64(f.clockRate)
		if diffSeconds >= 0.0 {
			if f.resumeBehindThreshold > 0 && diffSeconds > f.resumeBehindThreshold {
				logTransitionInfo("resume, reference too far behind", extExpectedTS, extRefTS, extLastTS, diffSeconds)
				extNextTS = extExpectedTS
				bigJump = true
			} else if diffSeconds > ResumeBehindHighThresholdSeconds {
				// could be due to incoming time stamp lagging a lot, like an unpause of the track
				logTransitionInfo("resume, reference very far behind", extExpectedTS, extRefTS, extLastTS, diffSeconds)
				extNextTS = extExpectedTS
				bigJump = true
			} else {
				extNextTS = extRefTS
			}
		} else {
			if math.Abs(diffSeconds) > SwitchAheadThresholdSeconds {
				logTransition("resume, reference too far ahead", extExpectedTS, extRefTS, extLastTS, diffSeconds)
			}
			extNextTS = extRefTS
		}
		f.resumeBehindThreshold = 0.0
	} else {
		// switching between layers, check if extRefTS is too far behind the last sent
		diffSeconds := float64(int64(extRefTS-extLastTS)) / float64(f.clockRate)
		if diffSeconds < 0.0 {
			if math.Abs(diffSeconds) > LayerSwitchBehindThresholdSeconds {
				// this could be due to pacer trickling out this layer. Error out and wait for a more opportune time.
				// AVSYNC-TODO: Consider some forcing function to do the switch
				// (like "have waited for too long for layer switch, nothing available, switch to whatever is available" kind of condition).
				logTransition("layer switch, reference too far behind", extExpectedTS, extRefTS, extLastTS, diffSeconds)

				return errSwitchPointTooFarBehind
			}

			// use a nominal increase to ensure that timestamp is always moving forward
			logTransition("layer switch, reference is slightly behind", extExpectedTS, extRefTS, extLastTS, diffSeconds)
			extNextTS = extLastTS + 1
		} else {
			diffSeconds = float64(int64(extRefTS-extExpectedTS)) / float64(f.clockRate)
			if diffSeconds > SwitchAheadThresholdSeconds {
				logTransition("layer switch, reference too far ahead", extExpectedTS, extRefTS, extLastTS, diffSeconds)
			}

			extNextTS = extRefTS
		}
	}

	if int64(extNextTS-extLastTS) <= 0 {
		f.logger.Debugw("next timestamp is before last, adjusting", "extNextTS", extNextTS, "extLastTS", extLastTS)
		// nominal increase
		extNextTS = extLastTS + 1
	}
	if bigJump { // TODO-REMOVE-AFTER-DATA-COLLECTION
		f.logger.Infow(
			"next timestamp on switch",
			"switchingAt", switchingAt,
			"layer", layer,
			"extLastTS", extLastTS,
			"lastMarker", rtpMungerState.LastMarker,
			"extRefTS", extRefTS,
			"dummyStartTSOffset", f.dummyStartTSOffset,
			"referenceLayerSpatial", f.referenceLayerSpatial,
			"extExpectedTS", extExpectedTS,
			"extNextTS", extNextTS,
			"tsJump", extNextTS-extLastTS,
			"nextSN", rtpMungerState.ExtLastSequenceNumber+1,
			"extIncomingSN", extPkt.ExtSequenceNumber,
			"incomingTS", extPkt.Packet.Timestamp,
			"extIncomingTS", extPkt.ExtTimestamp,
			"rtpStats", f.rtpStats,
		)
	} else {
		f.logger.Debugw(
			"next timestamp on switch",
			"switchingAt", switchingAt,
			"layer", layer,
			"extLastTS", extLastTS,
			"lastMarker", rtpMungerState.LastMarker,
			"extRefTS", extRefTS,
			"dummyStartTSOffset", f.dummyStartTSOffset,
			"referenceLayerSpatial", f.referenceLayerSpatial,
			"extExpectedTS", extExpectedTS,
			"extNextTS", extNextTS,
			"tsJump", extNextTS-extLastTS,
			"nextSN", rtpMungerState.ExtLastSequenceNumber+1,
			"extIncomingSN", extPkt.ExtSequenceNumber,
			"extIncomingTS", extPkt.ExtTimestamp,
			"rtpStats", f.rtpStats,
		)
	}

	f.rtpMunger.UpdateSnTsOffsets(extPkt, 1, extNextTS-extLastTS)
	f.codecMunger.UpdateOffsets(extPkt)
	return nil
}

// should be called with lock held
func (f *Forwarder) getTranslationParamsCommon(extPkt *buffer.ExtPacket, layer int32, tp *TranslationParams) error {
	if f.lastSSRC != extPkt.Packet.SSRC {
		if err := f.processSourceSwitch(extPkt, layer); err != nil {
			f.logger.Debugw(
				"could not switch feed",
				"error", err,
				"layer", layer,
				"refInfos", logger.ObjectSlice(f.refInfos[:]),
				"lastSwitchExtIncomingTS", f.lastSwitchExtIncomingTS,
				"rtpStats", f.rtpStats,
				"currentLayer", f.vls.GetCurrent(),
				"targetLayer", f.vls.GetCurrent(),
				"maxLayer", f.vls.GetMax(),
			)
			tp.shouldDrop = true
			f.vls.Rollback()
			return nil
		}
		f.logger.Debugw(
			"switching feed",
			"fromSSRC", f.lastSSRC,
			"toSSRC", extPkt.Packet.SSRC,
			"fromPayloadType", f.lastReferencePayloadType,
			"toPayloadType", extPkt.Packet.PayloadType,
			"layer", layer,
			"refInfos", logger.ObjectSlice(f.refInfos[:]),
			"lastSwitchExtIncomingTS", f.lastSwitchExtIncomingTS,
			"currentLayer", f.vls.GetCurrent(),
			"targetLayer", f.vls.GetCurrent(),
			"maxLayer", f.vls.GetMax(),
		)
		f.lastSSRC = extPkt.Packet.SSRC
		f.lastReferencePayloadType = int8(extPkt.Packet.PayloadType)
		f.lastSwitchExtIncomingTS = extPkt.ExtTimestamp
	}

	tpRTP, err := f.rtpMunger.UpdateAndGetSnTs(extPkt, tp.marker)
	if err != nil {
		tp.shouldDrop = true
		if err == ErrPaddingOnlyPacket || err == ErrDuplicatePacket || err == ErrOutOfOrderSequenceNumberCacheMiss {
			return nil
		}
		return err
	}

	tp.rtp = tpRTP

	if len(extPkt.Packet.Payload) > 0 {
		return f.translateCodecHeader(extPkt, tp)
	}

	return nil
}

// should be called with lock held
func (f *Forwarder) getTranslationParamsAudio(extPkt *buffer.ExtPacket, layer int32) (TranslationParams, error) {
	tp := TranslationParams{}
	if err := f.getTranslationParamsCommon(extPkt, layer, &tp); err != nil {
		tp.shouldDrop = true
		return tp, err
	}
	return tp, nil
}

// should be called with lock held
func (f *Forwarder) getTranslationParamsVideo(extPkt *buffer.ExtPacket, layer int32) (TranslationParams, error) {
	tp := TranslationParams{}
	if !f.vls.GetTarget().IsValid() {
		// stream is paused by streamallocator
		tp.shouldDrop = true
		return tp, nil
	}

	result := f.vls.Select(extPkt, layer)
	if !result.IsSelected {
		if f.isDDAvailable && extPkt.DependencyDescriptor == nil {
			f.isDDAvailable = false
			switch f.mime {
			case mime.MimeTypeVP9:
				f.vls = videolayerselector.NewVP9FromOther(f.vls)
			case mime.MimeTypeAV1:
				f.vls = videolayerselector.NewSimulcastFromOther(f.vls)
			}
		}
		tp.shouldDrop = true
		if f.started && result.IsRelevant {
			// call to update highest incoming sequence number and other internal structures
			if tpRTP, err := f.rtpMunger.UpdateAndGetSnTs(extPkt, result.RTPMarker); err == nil {
				if tpRTP.snOrdering == SequenceNumberOrderingContiguous {
					f.rtpMunger.PacketDropped(extPkt)
				}
			}
		}
		return tp, nil
	}
	tp.isResuming = result.IsResuming
	tp.isSwitching = result.IsSwitching
	tp.ddBytes = result.DependencyDescriptorExtension
	tp.marker = result.RTPMarker

	err := f.getTranslationParamsCommon(extPkt, layer, &tp)
	if tp.shouldDrop {
		return tp, err
	}

	if FlagPauseOnDowngrade && f.isDeficientLocked() && f.vls.GetTarget().Spatial < f.vls.GetCurrent().Spatial {
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
		// Note that it is possible for client subscription layer restriction
		// to coincide with server restriction due to bandwidth limitation,
		// In the case of subscription change, higher should continue streaming
		// to ensure smooth transition.
		//
		// To differentiate between the two cases, drop only when in DEFICIENT state.
		//
		tp.shouldDrop = true
		return tp, nil
	}

	return tp, nil
}

func (f *Forwarder) translateCodecHeader(extPkt *buffer.ExtPacket, tp *TranslationParams) error {
	// codec specific forwarding check and any needed packet munging
	tl := f.vls.SelectTemporal(extPkt)
	inputSize, codecBytes, err := f.codecMunger.UpdateAndGet(
		extPkt,
		tp.rtp.snOrdering == SequenceNumberOrderingOutOfOrder,
		tp.rtp.snOrdering == SequenceNumberOrderingGap,
		tl,
	)
	if err != nil {
		tp.shouldDrop = true
		if err == codecmunger.ErrFilteredVP8TemporalLayer || err == codecmunger.ErrOutOfOrderVP8PictureIdCacheMiss {
			if err == codecmunger.ErrFilteredVP8TemporalLayer {
				// filtered temporal layer, update sequence number offset to prevent holes
				f.rtpMunger.PacketDropped(extPkt)
			}
			return nil
		}

		return err
	}
	tp.incomingHeaderSize = inputSize
	tp.codecBytes = codecBytes
	return nil
}

func (f *Forwarder) maybeStart() {
	if f.started {
		return
	}

	f.started = true
	f.preStartTime = time.Now()

	sequenceNumber := uint16(rand.Intn(1<<14)) + uint16(1<<15) // a random number in third quartile of sequence number space
	timestamp := uint32(rand.Intn(1<<30)) + uint32(1<<31)      // a random number in third quartile of timestamp space
	extPkt := &buffer.ExtPacket{
		Packet: &rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: sequenceNumber,
				Timestamp:      timestamp,
			},
		},
		ExtSequenceNumber: uint64(sequenceNumber),
		ExtTimestamp:      uint64(timestamp),
	}
	f.rtpMunger.SetLastSnTs(extPkt)

	f.extFirstTS = uint64(timestamp)
	f.logger.Infow(
		"starting with dummy forwarding",
		"sequenceNumber", extPkt.Packet.SequenceNumber,
		"timestamp", extPkt.Packet.Timestamp,
		"preStartTime", f.preStartTime,
	)
}

func (f *Forwarder) GetSnTsForPadding(num int, frameRate uint32, forceMarker bool) ([]SnTs, error) {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.maybeStart()

	// padding is used for probing. Padding packets should only
	// be at frame boundaries to ensure decoder sequencer does
	// not get out-of-sync. But, when a stream is paused,
	// force a frame marker as a restart of the stream will
	// start with a key frame which will reset the decoder.
	if !f.vls.GetTarget().IsValid() {
		forceMarker = true
	}
	return f.rtpMunger.UpdateAndGetPaddingSnTs(
		num,
		f.clockRate,
		frameRate,
		forceMarker,
		f.rtpMunger.GetState().ExtLastTimestamp,
	)
}

func (f *Forwarder) GetSnTsForBlankFrames(frameRate uint32, numPackets int) ([]SnTs, bool, error) {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.maybeStart()

	frameEndNeeded := !f.rtpMunger.IsOnFrameBoundary()
	if frameEndNeeded {
		numPackets++
	}

	extLastTS := f.rtpMunger.GetState().ExtLastTimestamp
	extExpectedTS := extLastTS
	if f.rtpStats != nil {
		tsExt, err := f.rtpStats.GetExpectedRTPTimestamp(mono.Now())
		if err == nil {
			extExpectedTS = tsExt
		}
	}
	if int64(extExpectedTS-extLastTS) <= 0 {
		extExpectedTS = extLastTS + 1
	}
	snts, err := f.rtpMunger.UpdateAndGetPaddingSnTs(
		numPackets,
		f.clockRate,
		frameRate,
		frameEndNeeded,
		extExpectedTS,
	)
	return snts, frameEndNeeded, err
}

func (f *Forwarder) GetPadding(frameEndNeeded bool) ([]byte, error) {
	f.lock.Lock()
	defer f.lock.Unlock()

	return f.codecMunger.UpdateAndGetPadding(!frameEndNeeded)
}

func (f *Forwarder) RTPMungerDebugInfo() map[string]interface{} {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.rtpMunger.DebugInfo()
}

// -----------------------------------------------------------------------------

func getOptimalBandwidthNeeded(muted bool, pubMuted bool, maxPublishedLayer int32, brs Bitrates, maxLayer buffer.VideoLayer) int64 {
	if muted || pubMuted || maxPublishedLayer == buffer.InvalidLayerSpatial {
		return 0
	}

	for i := maxLayer.Spatial; i >= 0; i-- {
		for j := maxLayer.Temporal; j >= 0; j-- {
			if brs[i][j] == 0 {
				continue
			}

			return brs[i][j]
		}
	}

	// could be 0 due to either
	//   1. publisher has stopped all layers ==> feed dry.
	//   2. stream tracker has declared all layers stopped, functionally same as above.
	//      But, listed differently as this could be a mis-detection.
	//   3. Bitrate measurement is pending.
	return 0
}

func getBandwidthNeeded(brs Bitrates, layer buffer.VideoLayer, fallback int64) int64 {
	if layer.IsValid() && brs[layer.Spatial][layer.Temporal] > 0 {
		return brs[layer.Spatial][layer.Temporal]
	}

	return fallback
}

func getDistanceToDesired(
	muted bool,
	pubMuted bool,
	maxSeenLayer buffer.VideoLayer,
	availableLayers []int32,
	brs Bitrates,
	targetLayer buffer.VideoLayer,
	maxLayer buffer.VideoLayer,
) float64 {
	if muted || pubMuted || !maxSeenLayer.IsValid() || !maxLayer.IsValid() {
		return 0.0
	}

	adjustedMaxLayer := maxLayer

	maxAvailableSpatial := buffer.InvalidLayerSpatial
	maxAvailableTemporal := buffer.InvalidLayerTemporal

	// max available spatial is min(subscribedMax, publishedMax, availableMax)
	// subscribedMax = subscriber requested max spatial layer
	// publishedMax = max spatial layer ever published
	// availableMax = based on bit rate measurement, available max spatial layer
done:
	for s := int32(len(brs)) - 1; s >= 0; s-- {
		for t := int32(len(brs[0])) - 1; t >= 0; t-- {
			if brs[s][t] != 0 {
				maxAvailableSpatial = s
				break done
			}
		}
	}

	// before bit rate measurement is available, stream tracker could declare layer seen, account for that
	for _, layer := range availableLayers {
		if layer > maxAvailableSpatial {
			maxAvailableSpatial = layer
			maxAvailableTemporal = maxSeenLayer.Temporal // till bit rate measurement is available, assume max seen as temporal
		}
	}

	if maxAvailableSpatial < adjustedMaxLayer.Spatial {
		adjustedMaxLayer.Spatial = maxAvailableSpatial
	}

	if maxSeenLayer.Spatial < adjustedMaxLayer.Spatial {
		adjustedMaxLayer.Spatial = maxSeenLayer.Spatial
	}

	// max available temporal is min(subscribedMax, temporalLayerSeenMax, availableMax)
	// subscribedMax = subscriber requested max temporal layer
	// temporalLayerSeenMax = max temporal layer ever published/seen
	// availableMax = based on bit rate measurement, available max temporal in the adjusted max spatial layer
	if adjustedMaxLayer.Spatial != buffer.InvalidLayerSpatial {
		for t := int32(len(brs[0])) - 1; t >= 0; t-- {
			if brs[adjustedMaxLayer.Spatial][t] != 0 {
				maxAvailableTemporal = t
				break
			}
		}
	}
	if maxAvailableTemporal < adjustedMaxLayer.Temporal {
		adjustedMaxLayer.Temporal = maxAvailableTemporal
	}

	if maxSeenLayer.Temporal < adjustedMaxLayer.Temporal {
		adjustedMaxLayer.Temporal = maxSeenLayer.Temporal
	}

	if !adjustedMaxLayer.IsValid() {
		adjustedMaxLayer = buffer.VideoLayer{Spatial: 0, Temporal: 0}
	}

	// adjust target layers if they are invalid, i. e. not streaming
	adjustedTargetLayer := targetLayer
	if !targetLayer.IsValid() {
		adjustedTargetLayer = buffer.VideoLayer{Spatial: 0, Temporal: 0}
	}

	distance :=
		((adjustedMaxLayer.Spatial - adjustedTargetLayer.Spatial) * (maxSeenLayer.Temporal + 1)) +
			(adjustedMaxLayer.Temporal - adjustedTargetLayer.Temporal)
	if !targetLayer.IsValid() {
		distance += (maxSeenLayer.Temporal + 1)
	}

	return float64(distance) / float64(maxSeenLayer.Temporal+1)
}

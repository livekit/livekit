package sfu

import (
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/codecmunger"
	dd "github.com/livekit/livekit-server/pkg/sfu/dependencydescriptor"
	"github.com/livekit/livekit-server/pkg/sfu/videolayerselector"
	"github.com/livekit/livekit-server/pkg/sfu/videolayerselector/temporallayerselector"
)

// Forwarder
const (
	FlagPauseOnDowngrade    = true
	FlagFilterRTX           = true
	TransitionCostSpatial   = 10
	ParkedLayerWaitDuration = 2 * time.Second
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

func (v VideoAllocation) String() string {
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
	Bitrates        Bitrates
	maxLayer        buffer.VideoLayer
	currentLayer    buffer.VideoLayer
	parkedLayer     buffer.VideoLayer
	allocatedLayer  buffer.VideoLayer
}

// -------------------------------------------------------------------

type VideoTransition struct {
	From           buffer.VideoLayer
	To             buffer.VideoLayer
	BandwidthDelta int64
}

func (v VideoTransition) String() string {
	return fmt.Sprintf("VideoTransition{from: %s, to: %s, del: %d}", v.From, v.To, v.BandwidthDelta)
}

// -------------------------------------------------------------------

type TranslationParams struct {
	shouldDrop                  bool
	isResuming                  bool
	isSwitchingToRequestSpatial bool
	isSwitchingToMaxSpatial     bool
	maxSpatialLayer             int32
	rtp                         *TranslationParamsRTP
	codecBytes                  []byte
	ddBytes                     []byte
	marker                      bool
}

// -------------------------------------------------------------------

type ForwarderState struct {
	Started      bool
	PreStartTime time.Time
	FirstTS      uint32
	RefTSOffset  uint32
	RTP          RTPMungerState
	Codec        interface{}
}

func (f ForwarderState) String() string {
	codecString := ""
	switch codecState := f.Codec.(type) {
	case codecmunger.VP8State:
		codecString = codecState.String()
	}
	return fmt.Sprintf("ForwarderState{started: %v, preStartTime: %s, firstTS: %d, refTSOffset: %d, rtp: %s, codec: %s}",
		f.Started,
		f.PreStartTime.String(),
		f.FirstTS,
		f.RefTSOffset,
		f.RTP.String(),
		codecString,
	)
}

// -------------------------------------------------------------------

type Forwarder struct {
	lock                          sync.RWMutex
	codec                         webrtc.RTPCodecCapability
	kind                          webrtc.RTPCodecType
	logger                        logger.Logger
	getReferenceLayerRTPTimestamp func(ts uint32, layer int32, referenceLayer int32) (uint32, error)
	getExpectedRTPTimestamp       func(at time.Time) (uint32, uint64, error)

	muted    bool
	pubMuted bool

	started               bool
	preStartTime          time.Time
	firstTS               uint32
	lastSSRC              uint32
	referenceLayerSpatial int32
	refTSOffset           uint32

	parkedLayerTimer *time.Timer

	provisional *VideoAllocationProvisional

	lastAllocation VideoAllocation

	rtpMunger *RTPMunger

	vls videolayerselector.VideoLayerSelector

	codecMunger codecmunger.CodecMunger

	onParkedLayerExpired func()
}

func NewForwarder(
	kind webrtc.RTPCodecType,
	logger logger.Logger,
	getReferenceLayerRTPTimestamp func(ts uint32, layer int32, referenceLayer int32) (uint32, error),
	getExpectedRTPTimestamp func(at time.Time) (uint32, uint64, error),
) *Forwarder {
	f := &Forwarder{
		kind:                          kind,
		logger:                        logger,
		getReferenceLayerRTPTimestamp: getReferenceLayerRTPTimestamp,
		getExpectedRTPTimestamp:       getExpectedRTPTimestamp,
		referenceLayerSpatial:         buffer.InvalidLayerSpatial,
		lastAllocation:                VideoAllocationDefault,
		rtpMunger:                     NewRTPMunger(logger),
		vls:                           videolayerselector.NewNull(logger),
		codecMunger:                   codecmunger.NewNull(logger),
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
	f.logger.Debugw("setting max published layer", "maxPublishedLayer", maxPublishedLayer)
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

func (f *Forwarder) OnParkedLayerExpired(fn func()) {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.onParkedLayerExpired = fn
}

func (f *Forwarder) getOnParkedLayerExpired() func() {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.onParkedLayerExpired
}

func (f *Forwarder) DetermineCodec(codec webrtc.RTPCodecCapability, extensions []webrtc.RTPHeaderExtensionParameter) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.codec.MimeType != "" {
		return
	}
	f.codec = codec

	switch strings.ToLower(codec.MimeType) {
	case "video/vp8":
		f.codecMunger = codecmunger.NewVP8FromNull(f.codecMunger, f.logger)
		if f.vls != nil {
			f.vls = videolayerselector.NewSimulcastFromNull(f.vls)
		} else {
			f.vls = videolayerselector.NewSimulcast(f.logger)
		}
		f.vls.SetTemporalLayerSelector(temporallayerselector.NewVP8(f.logger))
	case "video/h264":
		if f.vls != nil {
			f.vls = videolayerselector.NewSimulcastFromNull(f.vls)
		} else {
			f.vls = videolayerselector.NewSimulcast(f.logger)
		}
	case "video/vp9":
		isDDAvailable := false
	searchDone:
		for _, ext := range extensions {
			switch ext.URI {
			case dd.ExtensionUrl:
				isDDAvailable = true
				break searchDone
			}
		}
		if isDDAvailable {
			if f.vls != nil {
				f.vls = videolayerselector.NewDependencyDescriptorFromNull(f.vls)
			} else {
				f.vls = videolayerselector.NewDependencyDescriptor(f.logger)
			}
		} else {
			if f.vls != nil {
				f.vls = videolayerselector.NewVP9FromNull(f.vls)
			} else {
				f.vls = videolayerselector.NewVP9(f.logger)
			}
		}
	case "video/av1":
		// DD-TODO : we only enable dd layer selector for av1/vp9 now, in the future we can enable it for vp8 too
		if f.vls != nil {
			f.vls = videolayerselector.NewDependencyDescriptorFromNull(f.vls)
		} else {
			f.vls = videolayerselector.NewDependencyDescriptor(f.logger)
		}
	}
}

func (f *Forwarder) GetState() ForwarderState {
	f.lock.RLock()
	defer f.lock.RUnlock()

	if !f.started {
		return ForwarderState{}
	}

	return ForwarderState{
		Started:      f.started,
		PreStartTime: f.preStartTime,
		FirstTS:      f.firstTS,
		RefTSOffset:  f.refTSOffset,
		RTP:          f.rtpMunger.GetLast(),
		Codec:        f.codecMunger.GetState(),
	}
}

func (f *Forwarder) SeedState(state ForwarderState) {
	if !state.Started {
		return
	}

	f.lock.Lock()
	defer f.lock.Unlock()

	f.rtpMunger.SeedLast(state.RTP)
	f.codecMunger.SeedState(state.Codec)

	f.started = true
	f.preStartTime = state.PreStartTime
	f.firstTS = state.FirstTS
	f.refTSOffset = state.RefTSOffset
}

func (f *Forwarder) Mute(muted bool) (bool, buffer.VideoLayer) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.muted == muted {
		return false, f.vls.GetMax()
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
	if muted && f.isDeficientLocked() && f.lastAllocation.PauseReason == VideoPauseReasonBandwidth {
		f.logger.Infow("ignoring forwarder mute, paused due to congestion")
		return false, f.vls.GetMax()
	}

	f.logger.Debugw("setting forwarder mute", "muted", muted)
	f.muted = muted

	// resync when muted so that sequence numbers do not jump on unmute
	if muted {
		f.resyncLocked()
	}

	return true, f.vls.GetMax()
}

func (f *Forwarder) IsMuted() bool {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.muted
}

func (f *Forwarder) PubMute(pubMuted bool) (bool, buffer.VideoLayer) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.pubMuted == pubMuted {
		return false, f.vls.GetMax()
	}

	f.logger.Debugw("setting forwarder pub mute", "pubMuted", pubMuted)
	f.pubMuted = pubMuted

	if f.kind == webrtc.RTPCodecTypeAudio {
		// for audio resync when pub muted so that sequence numbers do not jump on unmute
		// audio stops forwarding during pub mute too
		if pubMuted {
			f.resyncLocked()
		}
	} else {
		// Do not resync on publisher mute as forwarding can continue on unmute using same layer.
		// On unmute, park current layers as streaming can continue without a key frame when publisher starts the stream.
		targetLayer := f.vls.GetTarget()
		if !pubMuted && targetLayer.IsValid() && f.vls.GetCurrent().Spatial == targetLayer.Spatial {
			f.setupParkedLayer(targetLayer)
			f.vls.SetCurrent(buffer.InvalidLayer)
		}
	}

	return true, f.vls.GetMax()
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

func (f *Forwarder) SetMaxSpatialLayer(spatialLayer int32) (bool, buffer.VideoLayer, buffer.VideoLayer) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.kind == webrtc.RTPCodecTypeAudio {
		return false, buffer.InvalidLayer, buffer.InvalidLayer
	}

	existingMax := f.vls.GetMax()
	if spatialLayer == existingMax.Spatial {
		return false, existingMax, f.vls.GetCurrent()
	}

	f.logger.Debugw("setting max spatial layer", "layer", spatialLayer)
	f.vls.SetMaxSpatial(spatialLayer)

	f.clearParkedLayer()

	return true, f.vls.GetMax(), f.vls.GetCurrent()
}

func (f *Forwarder) SetMaxTemporalLayer(temporalLayer int32) (bool, buffer.VideoLayer, buffer.VideoLayer) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.kind == webrtc.RTPCodecTypeAudio {
		return false, buffer.InvalidLayer, buffer.InvalidLayer
	}

	existingMax := f.vls.GetMax()
	if temporalLayer == existingMax.Temporal {
		return false, existingMax, f.vls.GetCurrent()
	}

	f.logger.Debugw("setting max temporal layer", "layer", temporalLayer)
	f.vls.SetMaxTemporal(temporalLayer)

	f.clearParkedLayer()

	return true, f.vls.GetMax(), f.vls.GetCurrent()
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

func (f *Forwarder) GetReferenceLayerSpatial() int32 {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.referenceLayerSpatial
}

func (f *Forwarder) isDeficientLocked() bool {
	return f.lastAllocation.IsDeficient
}

func (f *Forwarder) IsDeficient() bool {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.isDeficientLocked()
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

func (f *Forwarder) AllocateOptimal(availableLayers []int32, brs Bitrates, allowOvershoot bool) VideoAllocation {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.kind == webrtc.RTPCodecTypeAudio {
		return f.lastAllocation
	}

	maxLayer := f.vls.GetMax()
	maxSeenLayer := f.vls.GetMaxSeen()
	parkedLayer := f.vls.GetParked()
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
			Spatial:  int32(math.Min(float64(maxSeenLayer.Spatial), float64(maxSpatial))),
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
		// leave it at current layers for opportunistic resume
		alloc.TargetLayer = currentLayer
		alloc.RequestLayerSpatial = alloc.TargetLayer.Spatial

	case parkedLayer.IsValid():
		// if parked on a layer, let it continue
		alloc.TargetLayer = parkedLayer
		alloc.RequestLayerSpatial = alloc.TargetLayer.Spatial

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
		maxLayerSpatialLimit := int32(math.Min(float64(maxLayer.Spatial), float64(maxSeenLayer.Spatial)))
		highestAvailableLayer := buffer.InvalidLayerSpatial
		requestLayerSpatial := buffer.InvalidLayerSpatial
		for _, al := range availableLayers {
			if al > requestLayerSpatial && al <= maxLayerSpatialLimit {
				requestLayerSpatial = al
			}
			if al > highestAvailableLayer {
				highestAvailableLayer = al
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
				// current layer has stopped, switch to highest available
				alloc.TargetLayer = buffer.VideoLayer{
					Spatial:  requestLayerSpatial,
					Temporal: getMaxTemporal(),
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

	if !alloc.TargetLayer.IsValid() {
		alloc.TargetLayer = buffer.InvalidLayer
		alloc.RequestLayerSpatial = buffer.InvalidLayerSpatial
	}
	if alloc.TargetLayer.IsValid() {
		alloc.BandwidthRequested = optimalBandwidthNeeded
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

func (f *Forwarder) ProvisionalAllocatePrepare(availableLayers []int32, Bitrates Bitrates) {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.provisional = &VideoAllocationProvisional{
		allocatedLayer: buffer.InvalidLayer,
		muted:          f.muted,
		pubMuted:       f.pubMuted,
		maxSeenLayer:   f.vls.GetMaxSeen(),
		Bitrates:       Bitrates,
		maxLayer:       f.vls.GetMax(),
		currentLayer:   f.vls.GetCurrent(),
		parkedLayer:    f.vls.GetParked(),
	}

	f.provisional.availableLayers = make([]int32, len(availableLayers))
	copy(f.provisional.availableLayers, availableLayers)
}

func (f *Forwarder) ProvisionalAllocate(availableChannelCapacity int64, layer buffer.VideoLayer, allowPause bool, allowOvershoot bool) int64 {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.provisional.muted ||
		f.provisional.pubMuted ||
		f.provisional.maxSeenLayer.Spatial == buffer.InvalidLayerSpatial ||
		!f.provisional.maxLayer.IsValid() ||
		((!allowOvershoot || !f.vls.IsOvershootOkay()) && layer.GreaterThan(f.provisional.maxLayer)) {
		return 0
	}

	requiredBitrate := f.provisional.Bitrates[layer.Spatial][layer.Temporal]
	if requiredBitrate == 0 {
		return 0
	}

	alreadyAllocatedBitrate := int64(0)
	if f.provisional.allocatedLayer.IsValid() {
		alreadyAllocatedBitrate = f.provisional.Bitrates[f.provisional.allocatedLayer.Spatial][f.provisional.allocatedLayer.Temporal]
	}

	// a layer under maximum fits, take it
	if !layer.GreaterThan(f.provisional.maxLayer) && requiredBitrate <= (availableChannelCapacity+alreadyAllocatedBitrate) {
		f.provisional.allocatedLayer = layer
		return requiredBitrate - alreadyAllocatedBitrate
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
		return requiredBitrate - alreadyAllocatedBitrate
	}

	return 0
}

func (f *Forwarder) ProvisionalAllocateGetCooperativeTransition(allowOvershoot bool) VideoTransition {
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
		bandwidthRequired := int64(0)
		f.provisional.allocatedLayer = buffer.InvalidLayer
		if f.provisional.pubMuted {
			// leave it at current for opportunistic forwarding, there is still bandwidth saving with publisher mute
			f.provisional.allocatedLayer = f.provisional.currentLayer
			if f.provisional.allocatedLayer.IsValid() {
				bandwidthRequired = f.provisional.Bitrates[f.provisional.allocatedLayer.Spatial][f.provisional.allocatedLayer.Temporal]
			}
		}
		return VideoTransition{
			From:           f.vls.GetTarget(),
			To:             f.provisional.allocatedLayer,
			BandwidthDelta: bandwidthRequired - getBandwidthNeeded(f.provisional.Bitrates, existingTargetLayer, f.lastAllocation.BandwidthRequested),
		}
	}

	// check if we should preserve current target
	if existingTargetLayer.IsValid() {
		// what is the highest that is available
		maximalLayer := buffer.InvalidLayer
		maximalBandwidthRequired := int64(0)
		for s := f.provisional.maxLayer.Spatial; s >= 0; s-- {
			for t := f.provisional.maxLayer.Temporal; t >= 0; t-- {
				if f.provisional.Bitrates[s][t] != 0 {
					maximalLayer = buffer.VideoLayer{Spatial: s, Temporal: t}
					maximalBandwidthRequired = f.provisional.Bitrates[s][t]
					break
				}
			}

			if maximalBandwidthRequired != 0 {
				break
			}
		}

		if maximalLayer.IsValid() {
			if !existingTargetLayer.GreaterThan(maximalLayer) && f.provisional.Bitrates[existingTargetLayer.Spatial][existingTargetLayer.Temporal] != 0 {
				// currently streaming and maybe wanting an upgrade (existingTargetLayer <= maximalLayer),
				// just preserve current target in the cooperative scheme of things
				f.provisional.allocatedLayer = existingTargetLayer
				return VideoTransition{
					From:           existingTargetLayer,
					To:             existingTargetLayer,
					BandwidthDelta: 0,
				}
			}

			if existingTargetLayer.GreaterThan(maximalLayer) {
				// maximalLayer < existingTargetLayer, make the down move
				f.provisional.allocatedLayer = maximalLayer
				return VideoTransition{
					From:           existingTargetLayer,
					To:             maximalLayer,
					BandwidthDelta: maximalBandwidthRequired - getBandwidthNeeded(f.provisional.Bitrates, existingTargetLayer, f.lastAllocation.BandwidthRequested),
				}
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
				if f.provisional.Bitrates[s][t] != 0 {
					layers = buffer.VideoLayer{Spatial: s, Temporal: t}
					bw = f.provisional.Bitrates[s][t]
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
		if f.provisional.parkedLayer.IsValid() {
			targetLayer = f.provisional.parkedLayer
		} else {
			targetLayer = f.provisional.currentLayer
		}

		if targetLayer.IsValid() {
			bandwidthRequired = f.provisional.Bitrates[targetLayer.Spatial][targetLayer.Temporal]
		}
	}

	f.provisional.allocatedLayer = targetLayer
	return VideoTransition{
		From:           f.vls.GetTarget(),
		To:             targetLayer,
		BandwidthDelta: bandwidthRequired - getBandwidthNeeded(f.provisional.Bitrates, existingTargetLayer, f.lastAllocation.BandwidthRequested),
	}
}

func (f *Forwarder) ProvisionalAllocateGetBestWeightedTransition() VideoTransition {
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
		// if publisher muted, give up opportunistic resume and give back the bandwidth
		f.provisional.allocatedLayer = buffer.InvalidLayer
		return VideoTransition{
			From:           targetLayer,
			To:             f.provisional.allocatedLayer,
			BandwidthDelta: 0 - getBandwidthNeeded(f.provisional.Bitrates, targetLayer, f.lastAllocation.BandwidthRequested),
		}
	}

	maxReachableLayerTemporal := buffer.InvalidLayerTemporal
	for t := f.provisional.maxLayer.Temporal; t >= 0; t-- {
		for s := f.provisional.maxLayer.Spatial; s >= 0; s-- {
			if f.provisional.Bitrates[s][t] != 0 {
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
		if f.provisional.parkedLayer.IsValid() {
			f.provisional.allocatedLayer = f.provisional.parkedLayer
		} else {
			f.provisional.allocatedLayer = f.provisional.currentLayer
		}
		return VideoTransition{
			From:           targetLayer,
			To:             f.provisional.allocatedLayer,
			BandwidthDelta: 0 - getBandwidthNeeded(f.provisional.Bitrates, targetLayer, f.lastAllocation.BandwidthRequested),
		}
	}

	// starting from minimum to target, find transition which gives the best
	// transition taking into account bits saved vs cost of such a transition
	existingBandwidthNeeded := getBandwidthNeeded(f.provisional.Bitrates, targetLayer, f.lastAllocation.BandwidthRequested)
	bestLayer := buffer.InvalidLayer
	bestBandwidthDelta := int64(0)
	bestValue := float32(0)
	for s := int32(0); s <= targetLayer.Spatial; s++ {
		for t := int32(0); t <= targetLayer.Temporal; t++ {
			if s == targetLayer.Spatial && t == targetLayer.Temporal {
				break
			}

			bandwidthDelta := int64(math.Max(float64(0), float64(existingBandwidthNeeded-f.provisional.Bitrates[s][t])))

			transitionCost := int32(0)
			// LK-TODO: SVC will need a different cost transition
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
		BandwidthDelta: bestBandwidthDelta,
	}
}

func (f *Forwarder) ProvisionalAllocateCommit() VideoAllocation {
	f.lock.Lock()
	defer f.lock.Unlock()

	optimalBandwidthNeeded := getOptimalBandwidthNeeded(
		f.provisional.muted,
		f.provisional.pubMuted,
		f.provisional.maxSeenLayer.Spatial,
		f.provisional.Bitrates,
		f.provisional.maxLayer,
	)
	alloc := VideoAllocation{
		BandwidthRequested:  0,
		BandwidthDelta:      0 - getBandwidthNeeded(f.provisional.Bitrates, f.vls.GetTarget(), f.lastAllocation.BandwidthRequested),
		Bitrates:            f.provisional.Bitrates,
		BandwidthNeeded:     optimalBandwidthNeeded,
		TargetLayer:         f.provisional.allocatedLayer,
		RequestLayerSpatial: f.provisional.allocatedLayer.Spatial,
		MaxLayer:            f.provisional.maxLayer,
		DistanceToDesired: getDistanceToDesired(
			f.provisional.muted,
			f.provisional.pubMuted,
			f.provisional.maxSeenLayer,
			f.provisional.availableLayers,
			f.provisional.Bitrates,
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
			alloc.BandwidthRequested = f.provisional.Bitrates[f.provisional.allocatedLayer.Spatial][f.provisional.allocatedLayer.Temporal]
			alloc.BandwidthDelta = alloc.BandwidthRequested - getBandwidthNeeded(f.provisional.Bitrates, f.vls.GetTarget(), f.lastAllocation.BandwidthRequested)
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
			alloc.BandwidthRequested = f.provisional.Bitrates[f.provisional.allocatedLayer.Spatial][f.provisional.allocatedLayer.Temporal]
		}
		alloc.BandwidthDelta = alloc.BandwidthRequested - getBandwidthNeeded(f.provisional.Bitrates, f.vls.GetTarget(), f.lastAllocation.BandwidthRequested)

		if f.provisional.allocatedLayer.GreaterThan(f.provisional.maxLayer) ||
			alloc.BandwidthRequested >= getOptimalBandwidthNeeded(
				f.provisional.muted,
				f.provisional.pubMuted,
				f.provisional.maxSeenLayer.Spatial,
				f.provisional.Bitrates,
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

	f.clearParkedLayer()
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

	// if targets are still pending, don't increase
	targetLayer := f.vls.GetTarget()
	if targetLayer.IsValid() && targetLayer != f.vls.GetCurrent() {
		return f.lastAllocation, false
	}

	maxLayer := f.vls.GetMax()
	maxSeenLayer := f.vls.GetMaxSeen()
	optimalBandwidthNeeded := getOptimalBandwidthNeeded(f.muted, f.pubMuted, maxSeenLayer.Spatial, brs, maxLayer)

	alreadyAllocated := int64(0)
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
				if bandwidthRequested == 0 {
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

	f.clearParkedLayer()
	return f.updateAllocation(alloc, "pause")
}

func (f *Forwarder) updateAllocation(alloc VideoAllocation, reason string) VideoAllocation {
	// restrict target temporal to 0 if codec does not support temporal layers
	if alloc.TargetLayer.IsValid() && strings.ToLower(f.codec.MimeType) == "video/h264" {
		alloc.TargetLayer.Temporal = 0
	}

	if alloc.IsDeficient != f.lastAllocation.IsDeficient ||
		alloc.PauseReason != f.lastAllocation.PauseReason ||
		alloc.TargetLayer != f.lastAllocation.TargetLayer ||
		alloc.RequestLayerSpatial != f.lastAllocation.RequestLayerSpatial {
		if reason == "optimal" {
			f.logger.Debugw(fmt.Sprintf("stream allocation: %s", reason), "allocation", alloc)
		} else {
			f.logger.Infow(fmt.Sprintf("stream allocation: %s", reason), "allocation", alloc)
		}
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
	f.vls.SetRequestSpatial(requestLayerSpatial)
}

func (f *Forwarder) Resync() {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.resyncLocked()
}

func (f *Forwarder) resyncLocked() {
	f.vls.SetCurrent(buffer.InvalidLayer)
	f.lastSSRC = 0
	f.clearParkedLayer()
}

func (f *Forwarder) clearParkedLayer() {
	f.vls.SetParked(buffer.InvalidLayer)
	if f.parkedLayerTimer != nil {
		f.parkedLayerTimer.Stop()
		f.parkedLayerTimer = nil
	}
}

func (f *Forwarder) setupParkedLayer(parkedLayer buffer.VideoLayer) {
	f.clearParkedLayer()

	f.vls.SetParked(parkedLayer)
	f.parkedLayerTimer = time.AfterFunc(ParkedLayerWaitDuration, func() {
		f.lock.Lock()
		notify := f.vls.GetParked().IsValid()
		f.clearParkedLayer()
		f.lock.Unlock()

		if onParkedLayerExpired := f.getOnParkedLayerExpired(); onParkedLayerExpired != nil && notify {
			onParkedLayerExpired()
		}
	})
}

func (f *Forwarder) CheckSync() (locked bool, layer int32) {
	f.lock.RLock()
	defer f.lock.RUnlock()

	layer = f.vls.GetRequestSpatial()
	locked = layer == f.vls.GetCurrent().Spatial || f.vls.GetParked().IsValid()
	return
}

func (f *Forwarder) FilterRTX(nacks []uint16) (filtered []uint16, disallowedLayers [buffer.DefaultMaxLayerSpatial + 1]bool) {
	if !FlagFilterRTX {
		filtered = nacks
		return
	}

	f.lock.RLock()
	defer f.lock.RUnlock()

	filtered = f.rtpMunger.FilterRTX(nacks)

	//
	// Curb RTX when deficient for two cases
	//   1. Target layer is lower than current layer. When current hits target, a key frame should flush the decoder.
	//   2. Requested layer is higher than current. Current layer's key frame should have flushed encoder.
	//      Remote might ask for older layer because of its jitter buffer, but let it starve as channel is already congested.
	//
	// Without the curb, when congestion hits, RTX rate could be so high that it further congests the channel.
	//
	currentLayer := f.vls.GetCurrent()
	targetLayer := f.vls.GetTarget()
	for layer := int32(0); layer < buffer.DefaultMaxLayerSpatial+1; layer++ {
		if f.isDeficientLocked() && (targetLayer.Spatial < currentLayer.Spatial || layer > currentLayer.Spatial) {
			disallowedLayers[layer] = true
		}
	}

	return
}

func (f *Forwarder) GetTranslationParams(extPkt *buffer.ExtPacket, layer int32) (*TranslationParams, error) {
	f.lock.Lock()
	defer f.lock.Unlock()

	// Video: Do not drop on publisher mute to enable resume on publisher unmute without a key frame.
	if f.muted {
		return &TranslationParams{
			shouldDrop: true,
		}, nil
	}

	switch f.kind {
	case webrtc.RTPCodecTypeAudio:
		// Audio: Blank frames are injected on publisher mute to ensure decoder does not get stuck at a noise frame. So, do not forward.
		if f.pubMuted {
			return &TranslationParams{
				shouldDrop: true,
			}, nil
		}

		return f.getTranslationParamsAudio(extPkt, layer)
	case webrtc.RTPCodecTypeVideo:
		return f.getTranslationParamsVideo(extPkt, layer)
	}

	return nil, ErrUnknownKind
}

// should be called with lock held
func (f *Forwarder) getTranslationParamsCommon(extPkt *buffer.ExtPacket, layer int32, tp *TranslationParams) (*TranslationParams, error) {
	if f.lastSSRC != extPkt.Packet.SSRC {
		if !f.started {
			f.started = true
			f.referenceLayerSpatial = layer
			f.rtpMunger.SetLastSnTs(extPkt)
			f.codecMunger.SetLast(extPkt)
			f.logger.Infow(
				"starting forwarding",
				"sequenceNumber", extPkt.Packet.SequenceNumber,
				"timestamp", extPkt.Packet.Timestamp,
				"layer", layer,
				"referenceLayerSpatial", f.referenceLayerSpatial,
			)
		} else {
			if f.referenceLayerSpatial == buffer.InvalidLayerSpatial {
				// on a resume, reference layer may not be set, so only set when it is invalid
				f.referenceLayerSpatial = layer
			}

			// Compute how much time passed between the old RTP extPkt
			// and the current packet, and fix timestamp on source change
			//
			// There are three time stamps to consider here
			//   1. lastTS -> time stamp of last sent packet
			//   2. refTS -> time stamp of this packet (after munging) calculated using feed's RTCP sender report
			//   3. expectedTS -> time stamp of this packet (after munging) calculated using this stream's RTCP sender report
			// Ideally, refTS and expectedTS should be very close and lastTS should be before both of those.
			// But, cases like muting/unmuting, clock vagaries make them not satisfy those conditions always.
			//
			// There are 6 orderings to consider (considering only inequalities). Resolve them using following rules
			//   1. Timestamp has to move forward
			//   2. Keep next time stamp close to expected
			lastTS := f.rtpMunger.GetLast().LastTS
			refTS := lastTS
			expectedTS := lastTS
			minTS := ^uint64(0)
			switchingAt := time.Now()
			if f.getReferenceLayerRTPTimestamp != nil {
				ts, err := f.getReferenceLayerRTPTimestamp(extPkt.Packet.Timestamp, layer, f.referenceLayerSpatial)
				if err == nil {
					refTS = ts
				}
			}
			if f.getExpectedRTPTimestamp != nil {
				ts, min, err := f.getExpectedRTPTimestamp(switchingAt)
				if err == nil {
					expectedTS = ts
					minTS = min
				} else {
					rtpDiff := uint32(0)
					if !f.preStartTime.IsZero() && f.refTSOffset == 0 {
						timeSinceFirst := time.Since(f.preStartTime)
						rtpDiff = uint32(timeSinceFirst.Nanoseconds() * int64(f.codec.ClockRate) / 1e9)
						f.refTSOffset = f.firstTS + rtpDiff - refTS
						f.logger.Infow(
							"calculating refTSOffset",
							"preStartTime", f.preStartTime.String(),
							"firstTS", f.firstTS,
							"timeSinceFirst", timeSinceFirst,
							"rtpDiff", rtpDiff,
							"refTS", refTS,
							"refTSOffset", f.refTSOffset,
						)
					}
					expectedTS += rtpDiff
				}
			}
			refTS += f.refTSOffset
			nextTS, explain := getNextTimestamp(lastTS, refTS, expectedTS, minTS)
			f.logger.Infow(
				"next timestamp on switch",
				"switchingAt", switchingAt.String(),
				"layer", layer,
				"lastTS", lastTS,
				"refTS", refTS,
				"refTSOffset", f.refTSOffset,
				"referenceLayerSpatial", f.referenceLayerSpatial,
				"expectedTS", expectedTS,
				"minTS", minTS,
				"nextTS", nextTS,
				"jump", nextTS-lastTS,
				"explanation", explain,
			)

			f.rtpMunger.UpdateSnTsOffsets(extPkt, 1, nextTS-lastTS)
			f.codecMunger.UpdateOffsets(extPkt)
		}

		f.logger.Debugw("switching feed", "from", f.lastSSRC, "to", extPkt.Packet.SSRC)
		f.lastSSRC = extPkt.Packet.SSRC
	}

	if tp == nil {
		tp = &TranslationParams{}
	}
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
func (f *Forwarder) getTranslationParamsAudio(extPkt *buffer.ExtPacket, layer int32) (*TranslationParams, error) {
	return f.getTranslationParamsCommon(extPkt, layer, nil)
}

// should be called with lock held
func (f *Forwarder) getTranslationParamsVideo(extPkt *buffer.ExtPacket, layer int32) (*TranslationParams, error) {
	tp := &TranslationParams{}

	if !f.vls.GetTarget().IsValid() {
		// stream is paused by streamallocator
		tp.shouldDrop = true
		return tp, nil
	}

	result := f.vls.Select(extPkt, layer)
	if !result.IsSelected {
		tp.shouldDrop = true
		if f.started && result.IsRelevant {
			f.rtpMunger.UpdateAndGetSnTs(extPkt) // call to update highest incoming sequence number and other internal structures
			f.rtpMunger.PacketDropped(extPkt)
		}
		return tp, nil
	}
	tp.isResuming = result.IsResuming
	tp.isSwitchingToRequestSpatial = result.IsSwitchingToRequestSpatial
	tp.isSwitchingToMaxSpatial = result.IsSwitchingToMaxSpatial
	tp.maxSpatialLayer = result.MaxSpatialLayer
	tp.ddBytes = result.DependencyDescriptorExtension
	tp.marker = result.RTPMarker

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

	_, err := f.getTranslationParamsCommon(extPkt, layer, tp)
	if tp.shouldDrop || len(extPkt.Packet.Payload) == 0 {
		return tp, err
	}

	// codec specific forwarding check and any needed packet munging
	codecBytes, err := f.codecMunger.UpdateAndGet(
		extPkt,
		tp.rtp.snOrdering == SequenceNumberOrderingOutOfOrder,
		tp.rtp.snOrdering == SequenceNumberOrderingGap,
		f.vls.SelectTemporal(extPkt),
	)
	if err != nil {
		tp.rtp = nil
		tp.shouldDrop = true
		if err == codecmunger.ErrFilteredVP8TemporalLayer || err == codecmunger.ErrOutOfOrderVP8PictureIdCacheMiss {
			if err == codecmunger.ErrFilteredVP8TemporalLayer {
				// filtered temporal layer, update sequence number offset to prevent holes
				f.rtpMunger.PacketDropped(extPkt)
			}
			return tp, nil
		}

		return tp, err
	}

	tp.codecBytes = codecBytes
	return tp, nil
}

func (f *Forwarder) maybeStart() {
	if f.started {
		return
	}

	f.started = true
	f.preStartTime = time.Now()

	extPkt := &buffer.ExtPacket{
		Packet: &rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: uint16(rand.Intn(1<<14)) + uint16(1<<15), // a random number in third quartile of sequence number space
				Timestamp:      uint32(rand.Intn(1<<30)) + uint32(1<<31), // a random number in third quartile of time stamp space
			},
		},
	}
	f.rtpMunger.SetLastSnTs(extPkt)

	f.firstTS = extPkt.Packet.Timestamp
	f.logger.Infow(
		"starting with dummy forwarding",
		"sequenceNumber", extPkt.Packet.SequenceNumber,
		"timestamp", extPkt.Packet.Timestamp,
		"preStartTime", f.preStartTime,
	)
}

func (f *Forwarder) GetSnTsForPadding(num int, forceMarker bool) ([]SnTs, error) {
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
	return f.rtpMunger.UpdateAndGetPaddingSnTs(num, 0, 0, forceMarker, 0)
}

func (f *Forwarder) GetSnTsForBlankFrames(frameRate uint32, numPackets int) ([]SnTs, bool, error) {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.maybeStart()

	frameEndNeeded := !f.rtpMunger.IsOnFrameBoundary()
	if frameEndNeeded {
		numPackets++
	}

	lastTS := f.rtpMunger.GetLast().LastTS
	expectedTS := lastTS
	minTS := ^uint64(0)
	if f.getExpectedRTPTimestamp != nil {
		ts, min, err := f.getExpectedRTPTimestamp(time.Now())
		if err == nil {
			expectedTS = ts
			minTS = min
		}
	}
	nextTS, _ := getNextTimestamp(lastTS, expectedTS, expectedTS, minTS)
	snts, err := f.rtpMunger.UpdateAndGetPaddingSnTs(numPackets, f.codec.ClockRate, frameRate, frameEndNeeded, nextTS)
	return snts, frameEndNeeded, err
}

func (f *Forwarder) GetPadding(frameEndNeeded bool) ([]byte, error) {
	f.lock.Lock()
	defer f.lock.Unlock()

	return f.codecMunger.UpdateAndGetPadding(!frameEndNeeded)
}

func (f *Forwarder) GetRTPMungerParams() RTPMungerParams {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.rtpMunger.GetParams()
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
		distance++
	}

	return float64(distance) / float64(maxSeenLayer.Temporal+1)
}

func getNextTimestamp(lastTS uint32, refTS uint32, expectedTS uint32, minTS uint64) (uint32, string) {
	isInOrder := func(val1, val2 uint32) bool {
		diff := val1 - val2
		return diff != 0 && diff < (1<<31)
	}

	rl := isInOrder(refTS, lastTS)
	el := isInOrder(expectedTS, lastTS)
	er := isInOrder(expectedTS, refTS)

	nextTS := lastTS + 1
	explain := "l = r = e"

	switch {
	case rl && el && er: // lastTS < refTS < expectedTS
		nextTS = uint32(float64(refTS) + 0.05*float64(expectedTS-refTS))
		explain = fmt.Sprintf("l < r < e, %d, %d", refTS-lastTS, expectedTS-refTS)
	case rl && el && !er: // lastTS < expectedTS < refTS
		nextTS = uint32(float64(expectedTS) + 0.5*float64(refTS-expectedTS))
		explain = fmt.Sprintf("l < e < r, %d, %d", expectedTS-lastTS, refTS-expectedTS)
	case !rl && el && er: // refTS < lastTS < expectedTS
		nextTS = uint32(float64(lastTS) + 0.5*float64(expectedTS-lastTS))
		explain = fmt.Sprintf("r < l < e, %d, %d", lastTS-refTS, expectedTS-lastTS)
	case !rl && !el && er: // refTS < expectedTS < lastTS
		nextTS = lastTS + 1
		explain = fmt.Sprintf("r < e < l, %d, %d", expectedTS-refTS, lastTS-expectedTS)
	case rl && !el && !er: // expectedTS < lastTS < refTS
		nextTS = uint32(float64(lastTS) + 0.75*float64(refTS-lastTS))
		explain = fmt.Sprintf("e < l < r, %d, %d", lastTS-expectedTS, refTS-lastTS)
	case !rl && !el && !er: // expectedTS < refTS < lastTS
		nextTS = lastTS + 1
		explain = fmt.Sprintf("e < r < l, %d, %d", refTS-expectedTS, lastTS-refTS)
	}

	if minTS != ^uint64(0) && !isInOrder(nextTS, uint32(minTS)) {
		nextTS = uint32(minTS) + 1
	}

	return nextTS, explain
}

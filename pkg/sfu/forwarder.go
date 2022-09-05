package sfu

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v3"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	dd "github.com/livekit/livekit-server/pkg/sfu/dependencydescriptor"
)

//
// Forwarder
//
const (
	FlagPauseOnDowngrade  = true
	FlagFilterRTX         = true
	TransitionCostSpatial = 10
)

// -------------------------------------------------------------------

type ForwardingStatus int

const (
	ForwardingStatusOff ForwardingStatus = iota
	ForwardingStatusPartial
	ForwardingStatusOptimal
)

// -------------------------------------------------------------------

type VideoStreamingChange int

const (
	VideoStreamingChangeNone VideoStreamingChange = iota
	VideoStreamingChangePausing
	VideoStreamingChangeResuming
)

func (v VideoStreamingChange) String() string {
	switch v {
	case VideoStreamingChangeNone:
		return "NONE"
	case VideoStreamingChangePausing:
		return "PAUSING"
	case VideoStreamingChangeResuming:
		return "RESUMING"
	default:
		return fmt.Sprintf("%d", int(v))
	}
}

// -------------------------------------------------------------------

type VideoAllocationState int

const (
	VideoAllocationStateNone VideoAllocationState = iota
	VideoAllocationStateMuted
	VideoAllocationStateFeedDry
	VideoAllocationStateAwaitingMeasurement
	VideoAllocationStateOptimal
	VideoAllocationStateDeficient
)

func (v VideoAllocationState) String() string {
	switch v {
	case VideoAllocationStateNone:
		return "NONE"
	case VideoAllocationStateMuted:
		return "MUTED"
	case VideoAllocationStateFeedDry:
		return "FEED_DRY"
	case VideoAllocationStateAwaitingMeasurement:
		return "AWAITING_MEASUREMENT"
	case VideoAllocationStateOptimal:
		return "OPTIMAL"
	case VideoAllocationStateDeficient:
		return "DEFICIENT"
	default:
		return fmt.Sprintf("%d", int(v))
	}
}

type VideoAllocation struct {
	state              VideoAllocationState
	change             VideoStreamingChange
	bandwidthRequested int64
	bandwidthDelta     int64
	availableLayers    []int32
	exemptedLayers     []int32
	bitrates           Bitrates
	targetLayers       VideoLayers
	distanceToDesired  int32
}

func (v VideoAllocation) String() string {
	return fmt.Sprintf("VideoAllocation{state: %s, change: %s, bw: %d, del: %d, avail: %+v, exmpt: %+v, rates: %+v, target: %s}",
		v.state, v.change, v.bandwidthRequested, v.bandwidthDelta, v.availableLayers, v.exemptedLayers, v.bitrates, v.targetLayers)
}

var (
	VideoAllocationDefault = VideoAllocation{
		targetLayers: InvalidLayers,
	}
)

// -------------------------------------------------------------------

type VideoAllocationProvisional struct {
	muted           bool
	bitrates        Bitrates
	availableLayers []int32
	exemptedLayers  []int32
	maxLayers       VideoLayers

	allocatedLayers VideoLayers
}

// -------------------------------------------------------------------

type VideoTransition struct {
	from           VideoLayers
	to             VideoLayers
	bandwidthDelta int64
}

// -------------------------------------------------------------------

type TranslationParams struct {
	shouldDrop            bool
	isDroppingRelevant    bool
	isSwitchingToMaxLayer bool
	rtp                   *TranslationParamsRTP
	vp8                   *TranslationParamsVP8
	ddExtension           *dd.DependencyDescriptorExtension
	marker                bool

	// indicates this frame has 'Switch' decode indication for target layer
	// TODO : in theory, we need check frame chain is not broken for the target
	// but we don't have frame queue now, so just use decode target indication
	switchingToTargetLayer bool
}

// -------------------------------------------------------------------
type VideoLayers = buffer.VideoLayer

const (
	InvalidLayerSpatial  = buffer.InvalidLayerSpatial
	InvalidLayerTemporal = buffer.InvalidLayerTemporal

	DefaultMaxLayerSpatial  = buffer.DefaultMaxLayerSpatial
	DefaultMaxLayerTemporal = buffer.DefaultMaxLayerTemporal
)

var (
	InvalidLayers = buffer.InvalidLayers
)

// -------------------------------------------------------------------

type ForwarderState struct {
	LastTSCalc int64
	RTP        RTPMungerState
	VP8        VP8MungerState
}

// -------------------------------------------------------------------

type Forwarder struct {
	lock   sync.RWMutex
	codec  webrtc.RTPCodecCapability
	kind   webrtc.RTPCodecType
	logger logger.Logger

	muted bool

	started  bool
	lastSSRC uint32
	lTSCalc  int64

	maxLayers     VideoLayers
	currentLayers VideoLayers
	targetLayers  VideoLayers

	provisional *VideoAllocationProvisional

	lastAllocation VideoAllocation

	availableLayers []int32
	exemptedLayers  []int32

	rtpMunger *RTPMunger
	vp8Munger *VP8Munger

	isTemporalSupported bool

	ddLayerSelector *DDVideoLayerSelector
}

func NewForwarder(kind webrtc.RTPCodecType, logger logger.Logger) *Forwarder {
	f := &Forwarder{
		kind:   kind,
		logger: logger,

		// start off with nothing, let streamallocator set things
		currentLayers: InvalidLayers,
		targetLayers:  InvalidLayers,

		lastAllocation: VideoAllocationDefault,

		rtpMunger: NewRTPMunger(logger),
	}

	if f.kind == webrtc.RTPCodecTypeVideo {
		f.maxLayers = VideoLayers{Spatial: InvalidLayerSpatial, Temporal: DefaultMaxLayerTemporal}
	} else {
		f.maxLayers = InvalidLayers
	}

	return f
}

func (f *Forwarder) DetermineCodec(codec webrtc.RTPCodecCapability) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.codec.MimeType != "" {
		return
	}
	f.codec = codec

	switch strings.ToLower(codec.MimeType) {
	case "video/vp8":
		f.isTemporalSupported = true
		f.vp8Munger = NewVP8Munger(f.logger)
	case "video/av1":
		// TODO : we only enable dd layer selector for av1 now, at future we can
		// enable it for vp9 too
		f.ddLayerSelector = NewDDVideoLayerSelector(f.logger)
	}
}

func (f *Forwarder) GetState() ForwarderState {
	f.lock.RLock()
	defer f.lock.RUnlock()

	state := ForwarderState{
		LastTSCalc: f.lTSCalc,
		RTP:        f.rtpMunger.GetLast(),
	}

	if f.vp8Munger != nil {
		state.VP8 = f.vp8Munger.GetLast()
	}

	return state
}

func (f *Forwarder) SeedState(state ForwarderState) {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.lTSCalc = state.LastTSCalc
	f.rtpMunger.SeedLast(state.RTP)
	if f.vp8Munger != nil {
		f.vp8Munger.SeedLast(state.VP8)
	}

	f.started = true
}

func (f *Forwarder) Mute(muted bool) (bool, VideoLayers) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.muted == muted {
		return false, f.maxLayers
	}

	f.logger.Infow("setting mute", "muted", muted)
	f.muted = muted

	// resync when muted so that sequence numbers do not jump on unmute
	if muted {
		f.resyncLocked()
	}

	return true, f.maxLayers
}

func (f *Forwarder) IsMuted() bool {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.muted
}

func (f *Forwarder) SetMaxSpatialLayer(spatialLayer int32) (bool, VideoLayers, VideoLayers) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.kind == webrtc.RTPCodecTypeAudio || spatialLayer == f.maxLayers.Spatial {
		return false, f.maxLayers, f.currentLayers
	}

	f.logger.Infow("setting max spatial layer", "layer", spatialLayer)
	f.maxLayers.Spatial = spatialLayer

	return true, f.maxLayers, f.currentLayers
}

func (f *Forwarder) SetMaxTemporalLayer(temporalLayer int32) (bool, VideoLayers, VideoLayers) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.kind == webrtc.RTPCodecTypeAudio || temporalLayer == f.maxLayers.Temporal {
		return false, f.maxLayers, f.currentLayers
	}

	f.maxLayers.Temporal = temporalLayer

	return true, f.maxLayers, f.currentLayers
}

func (f *Forwarder) MaxLayers() VideoLayers {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.maxLayers
}

func (f *Forwarder) CurrentLayers() VideoLayers {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.currentLayers
}

func (f *Forwarder) TargetLayers() VideoLayers {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.targetLayers
}

func (f *Forwarder) GetForwardingStatus() ForwardingStatus {
	f.lock.RLock()
	defer f.lock.RUnlock()

	if f.muted || len(f.availableLayers) == 0 {
		return ForwardingStatusOptimal
	}

	if f.targetLayers == InvalidLayers {
		return ForwardingStatusOff
	}

	if f.targetLayers.Spatial < f.maxLayers.Spatial && f.targetLayers.Spatial < f.availableLayers[len(f.availableLayers)-1] {
		return ForwardingStatusPartial
	}

	return ForwardingStatusOptimal
}

func (f *Forwarder) IsReducedQuality() (int32, bool) {
	f.lock.RLock()
	defer f.lock.RUnlock()

	if f.muted || len(f.availableLayers) == 0 || f.targetLayers.Spatial == InvalidLayerSpatial {
		return 0, false
	}

	if f.currentLayers.Spatial != f.targetLayers.Spatial {
		//
		// Waiting for layer lock, do not declare reduced quality.
		// Note the target might actually be a lower layer than current.
		//
		return 0, false
	}

	distance := f.maxLayers.Spatial - f.currentLayers.Spatial
	if distance < 0 {
		distance = 0
	}

	return distance, f.lastAllocation.state == VideoAllocationStateDeficient
}

func (f *Forwarder) UpTrackLayersChange(availableLayers []int32, exemptedLayers []int32) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if len(availableLayers) > 0 {
		f.availableLayers = make([]int32, len(availableLayers))
		copy(f.availableLayers, availableLayers)
	} else {
		f.availableLayers = nil
	}

	if len(exemptedLayers) > 0 {
		f.exemptedLayers = make([]int32, len(exemptedLayers))
		copy(f.exemptedLayers, exemptedLayers)
	} else {
		f.exemptedLayers = nil
	}
}

func (f *Forwarder) getOptimalBandwidthNeeded(brs Bitrates, maxLayers VideoLayers) int64 {
	if f.muted {
		return 0
	}

	for i := maxLayers.Spatial; i >= 0; i-- {
		for j := maxLayers.Temporal; j >= 0; j-- {
			if brs[i][j] == 0 {
				continue
			}

			return brs[i][j]
		}
	}

	return 0
}

func (f *Forwarder) bitrateAvailable(brs Bitrates) bool {
	neededLayers := 0
	bitrateAvailableLayers := 0
	for _, layer := range f.availableLayers {
		if layer > f.maxLayers.Spatial {
			continue
		}

		//
		// Layers could be exempted from stream tracker.
		// If such a layer actually stops, it will not
		// be removed from available layers as it is exempt.
		// But, it could have zero bit rate as it actually stopped.
		// So, do not take exempt layers into bitrate availability condition.
		//
		exempt := false
		for _, el := range f.exemptedLayers {
			if layer == el {
				exempt = true
				break
			}
		}
		if exempt {
			continue
		}

		neededLayers++
		for t := f.maxLayers.Temporal; t >= 0; t-- {
			if brs[layer][t] != 0 {
				bitrateAvailableLayers++
				break
			}
		}
	}

	return bitrateAvailableLayers == neededLayers
}

func (f *Forwarder) getDistanceToDesired(brs Bitrates, targetLayers VideoLayers, maxLayers VideoLayers) int32 {
	if f.muted {
		return 0
	}

	found := false
	distance := int32(0)
	for s := maxLayers.Spatial; s >= 0; s-- {
		for t := maxLayers.Temporal; t >= 0; t-- {
			if brs[s][t] == 0 {
				continue
			}
			if s == targetLayers.Spatial && t == targetLayers.Temporal {
				found = true
				break
			}

			distance++
		}

		if found {
			break
		}
	}

	// maybe overshooting
	if !found && targetLayers.IsValid() {
		distance = 0
		for s := targetLayers.Spatial; s > f.maxLayers.Spatial; s-- {
			for t := f.maxLayers.Temporal; t >= 0; t-- {
				if targetLayers.Temporal < t || brs[s][t] == 0 {
					continue
				}
				distance--
			}
		}
	}

	return distance
}

func (f *Forwarder) IsDeficient() bool {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.lastAllocation.state == VideoAllocationStateDeficient
}

func (f *Forwarder) BandwidthRequested(brs Bitrates) int64 {
	f.lock.RLock()
	defer f.lock.RUnlock()

	if f.targetLayers == InvalidLayers {
		return 0
	}

	return brs[f.targetLayers.Spatial][f.targetLayers.Temporal]
}

func (f *Forwarder) DistanceToDesired() int32 {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.lastAllocation.distanceToDesired
}

func (f *Forwarder) AllocateOptimal(brs Bitrates, allowOvershoot bool) VideoAllocation {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.kind == webrtc.RTPCodecTypeAudio {
		return f.lastAllocation
	}

	state := VideoAllocationStateNone
	change := VideoStreamingChangeNone
	bandwidthRequested := int64(0)
	targetLayers := InvalidLayers

	switch {
	case f.muted:
		state = VideoAllocationStateMuted
	case len(f.availableLayers) == 0:
		// feed is dry
		state = VideoAllocationStateFeedDry
	case !f.bitrateAvailable(brs):
		// feed bitrate not yet calculated for all available layers
		state = VideoAllocationStateAwaitingMeasurement

		//
		// Resume with the highest layer available <= max subscribed layer
		// If already resumed, move allocation to the highest available layer <= max subscribed layer
		//
		targetLayers.Spatial = int32(math.Min(float64(f.maxLayers.Spatial), float64(f.availableLayers[len(f.availableLayers)-1])))
		targetLayers.Temporal = int32(math.Max(0, float64(f.maxLayers.Temporal)))

		if f.targetLayers == InvalidLayers && targetLayers.IsValid() {
			change = VideoStreamingChangeResuming
		}
	default:
		// allocate best layer available
		for s := f.maxLayers.Spatial; s >= 0; s-- {
			for t := f.maxLayers.Temporal; t >= 0; t-- {
				if brs[s][t] == 0 {
					continue
				}

				targetLayers = VideoLayers{
					Spatial:  s,
					Temporal: t,
				}

				bandwidthRequested = brs[s][t]
				state = VideoAllocationStateOptimal
				break
			}

			if bandwidthRequested != 0 {
				break
			}
		}

		if bandwidthRequested == 0 && f.maxLayers.IsValid() && allowOvershoot {
			// if we cannot allocate anything below max layer,
			// look for a layer above. It is okay to overshoot
			// in optimal allocation (i.e. no bandwidth restrictions).
			// It is possible that clients send only a higher layer.
			// To accommodate cases like that, try finding a layer
			// above the requested maximum to ensure streaming
			for s := f.maxLayers.Spatial + 1; s <= DefaultMaxLayerSpatial; s++ {
				for t := int32(0); t <= DefaultMaxLayerTemporal; t++ {
					if brs[s][t] == 0 {
						continue
					}

					targetLayers = VideoLayers{
						Spatial:  s,
						Temporal: t,
					}

					bandwidthRequested = brs[s][t]
					state = VideoAllocationStateOptimal
					f.logger.Infow("allowing overshoot", "maxLayer", f.maxLayers, "targetLayers", targetLayers)
					break
				}

				if bandwidthRequested != 0 {
					break
				}
			}
		}

		if bandwidthRequested == 0 && f.maxLayers.IsValid() {
			// if overshoot was allowed and it did not also find a layer,
			// keep target at exempted layer (if available) and the current layer is at that level.
			// i. e. exempted layer may really have stopped, so a layer switch to an exempted layer should
			// not happen as layer switch will send PLI requests. Just letting it continue at the current
			// layer if the current is exempted will protect against any stream tracker misdetects
			// OR latch on to the layer quicker when it restarts
			if f.currentLayers.IsValid() {
				for _, s := range f.exemptedLayers {
					if s <= f.maxLayers.Spatial && f.currentLayers.Spatial == s {
						targetLayers = f.currentLayers
						bandwidthRequested = brs[targetLayers.Spatial][targetLayers.Temporal]
						state = VideoAllocationStateDeficient
						break
					}
				}
			}
		}

		if f.targetLayers == InvalidLayers && targetLayers.IsValid() {
			change = VideoStreamingChangeResuming
		} else if f.targetLayers != InvalidLayers && !targetLayers.IsValid() {
			change = VideoStreamingChangePausing
		}

		if !targetLayers.IsValid() && f.maxLayers.IsValid() {
			state = VideoAllocationStateDeficient
		}
	}

	if !targetLayers.IsValid() {
		targetLayers = InvalidLayers
	}
	f.lastAllocation = VideoAllocation{
		state:              state,
		change:             change,
		bandwidthRequested: bandwidthRequested,
		bandwidthDelta:     bandwidthRequested - f.lastAllocation.bandwidthRequested,
		bitrates:           brs,
		targetLayers:       targetLayers,
		distanceToDesired:  f.getDistanceToDesired(brs, targetLayers, f.maxLayers),
	}
	if len(f.availableLayers) > 0 {
		f.lastAllocation.availableLayers = make([]int32, len(f.availableLayers))
		copy(f.lastAllocation.availableLayers, f.availableLayers)
	}
	if len(f.exemptedLayers) > 0 {
		f.lastAllocation.exemptedLayers = make([]int32, len(f.exemptedLayers))
		copy(f.lastAllocation.exemptedLayers, f.exemptedLayers)
	}

	f.setTargetLayers(f.lastAllocation.targetLayers)
	if f.targetLayers == InvalidLayers {
		f.resyncLocked()
	}

	return f.lastAllocation
}

func (f *Forwarder) ProvisionalAllocatePrepare(bitrates Bitrates) {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.provisional = &VideoAllocationProvisional{
		allocatedLayers: InvalidLayers,
		muted:           f.muted,
		bitrates:        bitrates,
		maxLayers:       f.maxLayers,
	}
	if len(f.availableLayers) > 0 {
		f.provisional.availableLayers = make([]int32, len(f.availableLayers))
		copy(f.provisional.availableLayers, f.availableLayers)
	}
	if len(f.exemptedLayers) > 0 {
		f.provisional.exemptedLayers = make([]int32, len(f.exemptedLayers))
		copy(f.provisional.exemptedLayers, f.exemptedLayers)
	}
}

func (f *Forwarder) ProvisionalAllocate(availableChannelCapacity int64, layers VideoLayers, allowPause bool, allowOvershoot bool) int64 {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.provisional.muted || !f.provisional.maxLayers.IsValid() || (!allowOvershoot && layers.GreaterThan(f.provisional.maxLayers)) {
		return 0
	}

	maybeAdoptExempted := func() int64 {
		br := int64(0)
		if f.currentLayers.IsValid() {
			for _, s := range f.provisional.exemptedLayers {
				if s <= f.provisional.maxLayers.Spatial && f.currentLayers.Spatial == s {
					f.provisional.allocatedLayers = f.currentLayers
					br = f.provisional.bitrates[f.provisional.allocatedLayers.Spatial][f.provisional.allocatedLayers.Temporal]
					break
				}
			}
		}
		return br
	}

	requiredBitrate := f.provisional.bitrates[layers.Spatial][layers.Temporal]
	if requiredBitrate == 0 {
		return maybeAdoptExempted()
	}

	alreadyAllocatedBitrate := int64(0)
	if f.provisional.allocatedLayers != InvalidLayers {
		alreadyAllocatedBitrate = f.provisional.bitrates[f.provisional.allocatedLayers.Spatial][f.provisional.allocatedLayers.Temporal]
	}

	// a layer under maximum fits, take it
	if !layers.GreaterThan(f.provisional.maxLayers) && requiredBitrate <= (availableChannelCapacity+alreadyAllocatedBitrate) {
		f.provisional.allocatedLayers = layers
		return requiredBitrate - alreadyAllocatedBitrate
	}

	//
	// Given layer does not fit. But overshoot is allowed.
	// Could be one of
	//  1. a layer below maximum that does not fit
	//  2. a layer above maximum which may or may not fit.
	// In any of those cases, take the lowest possible layer if pause is not allowed
	//
	if !allowPause && (f.provisional.allocatedLayers == InvalidLayers || !layers.GreaterThan(f.provisional.allocatedLayers)) {
		f.provisional.allocatedLayers = layers
		return requiredBitrate - alreadyAllocatedBitrate
	}

	return maybeAdoptExempted()
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

	if f.provisional.muted {
		f.provisional.allocatedLayers = InvalidLayers
		return VideoTransition{
			from:           f.targetLayers,
			to:             InvalidLayers,
			bandwidthDelta: 0 - f.lastAllocation.bandwidthRequested,
			// LK-TODO should this take current bitrate of current target layers?
		}
	}

	// check if we should preserve current target
	if f.targetLayers != InvalidLayers {
		// what is the highest that is available
		maximalLayers := InvalidLayers
		maximalBandwidthRequired := int64(0)
		for s := f.provisional.maxLayers.Spatial; s >= 0; s-- {
			for t := f.provisional.maxLayers.Temporal; t >= 0; t-- {
				if f.provisional.bitrates[s][t] != 0 {
					maximalLayers = VideoLayers{Spatial: s, Temporal: t}
					maximalBandwidthRequired = f.provisional.bitrates[s][t]
					break
				}
			}

			if maximalBandwidthRequired != 0 {
				break
			}
		}

		if maximalLayers != InvalidLayers {
			if !f.targetLayers.GreaterThan(maximalLayers) && f.provisional.bitrates[f.targetLayers.Spatial][f.targetLayers.Temporal] != 0 {
				// currently streaming and wanting an upgrade, just preserve current target in the cooperative scheme of things
				f.provisional.allocatedLayers = f.targetLayers
				return VideoTransition{
					from:           f.targetLayers,
					to:             f.targetLayers,
					bandwidthDelta: 0,
				}
			}

			if f.targetLayers.GreaterThan(maximalLayers) {
				// maximalLayers <= f.targetLayers, make the down move
				f.provisional.allocatedLayers = maximalLayers
				return VideoTransition{
					from:           f.targetLayers,
					to:             maximalLayers,
					bandwidthDelta: maximalBandwidthRequired - f.lastAllocation.bandwidthRequested,
				}
			}
		}
	}

	findNextLayer := func(
		minSpatial, maxSpatial int32,
		minTemporal, maxTemporal int32,
	) (VideoLayers, int64) {
		layers := InvalidLayers
		bw := int64(0)
		for s := minSpatial; s <= maxSpatial; s++ {
			for t := minTemporal; t <= maxTemporal; t++ {
				if f.provisional.bitrates[s][t] != 0 {
					layers = VideoLayers{Spatial: s, Temporal: t}
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

	targetLayers := f.targetLayers
	bandwidthRequired := int64(0)
	if targetLayers == InvalidLayers {
		// currently not streaming, find minimal
		// NOTE: a layer in feed could have paused and there could be other options than going back to minimal,
		// but the cooperative scheme knocks things back to minimal
		targetLayers, bandwidthRequired = findNextLayer(
			0, f.provisional.maxLayers.Spatial,
			0, f.provisional.maxLayers.Temporal,
		)
	}

	// could not find a minimal layer, overshoot if allowed
	if bandwidthRequired == 0 && f.provisional.maxLayers.IsValid() && allowOvershoot {
		targetLayers, bandwidthRequired = findNextLayer(
			f.provisional.maxLayers.Spatial+1, DefaultMaxLayerSpatial,
			0, DefaultMaxLayerTemporal,
		)
	}

	// adopt exempted layer if current is at one of the exempted layers below maximum
	if bandwidthRequired == 0 && f.provisional.maxLayers.IsValid() && f.currentLayers.IsValid() {
		for _, s := range f.provisional.exemptedLayers {
			if s <= f.provisional.maxLayers.Spatial && f.currentLayers.Spatial == s {
				targetLayers = f.currentLayers
				bandwidthRequired = f.provisional.bitrates[targetLayers.Spatial][targetLayers.Temporal]
				break
			}
		}
	}

	// turn off if nothing found, not even an exempted layer to continue with
	if bandwidthRequired == 0 && (!f.currentLayers.IsValid() || f.currentLayers != targetLayers) {
		targetLayers = InvalidLayers
	}

	f.provisional.allocatedLayers = targetLayers
	return VideoTransition{
		from:           f.targetLayers,
		to:             targetLayers,
		bandwidthDelta: bandwidthRequired - f.lastAllocation.bandwidthRequested,
	}
}

func (f *Forwarder) ProvisionalAllocateGetBestWeightedTransition() VideoTransition {
	//
	// This is called when a track needs a change (could be mute/unmute, subscribed layers changed, published layers changed)
	// when channel is congested.
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

	if f.provisional.muted {
		f.provisional.allocatedLayers = InvalidLayers
		return VideoTransition{
			from:           f.targetLayers,
			to:             InvalidLayers,
			bandwidthDelta: 0 - f.lastAllocation.bandwidthRequested,
			// LK-TODO should this take current bitrate of current target layers?
		}
	}

	maxReachableLayerTemporal := InvalidLayerTemporal
	for t := f.provisional.maxLayers.Temporal; t >= 0; t-- {
		for s := f.provisional.maxLayers.Spatial; s >= 0; s-- {
			if f.provisional.bitrates[s][t] != 0 {
				maxReachableLayerTemporal = t
				break
			}
		}
		if maxReachableLayerTemporal != InvalidLayerTemporal {
			break
		}
	}

	if maxReachableLayerTemporal == InvalidLayerTemporal {
		// stick to an exempted layer if available
		if f.currentLayers.IsValid() {
			for _, s := range f.provisional.exemptedLayers {
				if s <= f.provisional.maxLayers.Spatial && f.currentLayers.Spatial == s {
					f.provisional.allocatedLayers = f.currentLayers
					return VideoTransition{
						from:           f.targetLayers,
						to:             f.provisional.allocatedLayers,
						bandwidthDelta: 0 - f.lastAllocation.bandwidthRequested,
						// LK-TODO should this take current bitrate of current target layers?
					}
				}
			}
		}

		// feed has gone dry,
		f.provisional.allocatedLayers = InvalidLayers
		return VideoTransition{
			from:           f.targetLayers,
			to:             InvalidLayers,
			bandwidthDelta: 0 - f.lastAllocation.bandwidthRequested,
			// LK-TODO should this take current bitrate of current target layers?
		}
	}

	// starting from minimum to target, find transition which gives the best
	// transition taking into account bits saved vs cost of such a transition
	bestLayers := InvalidLayers
	bestBandwidthDelta := int64(0)
	bestValue := float32(0)
	for s := int32(0); s <= f.targetLayers.Spatial; s++ {
		for t := int32(0); t <= f.targetLayers.Temporal; t++ {
			if s == f.targetLayers.Spatial && t == f.targetLayers.Temporal {
				break
			}

			bandwidthDelta := int64(math.Max(float64(0), float64(f.lastAllocation.bandwidthRequested-f.provisional.bitrates[s][t])))

			transitionCost := int32(0)
			if f.targetLayers.Spatial != s {
				transitionCost = TransitionCostSpatial
			}

			qualityCost := (maxReachableLayerTemporal+1)*(f.targetLayers.Spatial-s) + (f.targetLayers.Temporal - t)

			value := float32(0)
			if (transitionCost + qualityCost) != 0 {
				value = float32(bandwidthDelta) / float32(transitionCost+qualityCost)
			}
			if value > bestValue || (value == bestValue && bandwidthDelta > bestBandwidthDelta) {
				bestValue = value
				bestBandwidthDelta = bandwidthDelta
				bestLayers = VideoLayers{Spatial: s, Temporal: t}
			}
		}
	}

	f.provisional.allocatedLayers = bestLayers
	return VideoTransition{
		from:           f.targetLayers,
		to:             bestLayers,
		bandwidthDelta: bestBandwidthDelta,
	}
}

func (f *Forwarder) ProvisionalAllocateCommit() VideoAllocation {
	f.lock.Lock()
	defer f.lock.Unlock()

	state := VideoAllocationStateNone
	change := VideoStreamingChangeNone
	bandwidthRequested := int64(0)

	switch {
	case f.provisional.muted:
		state = VideoAllocationStateMuted
	case len(f.provisional.availableLayers) == 0:
		// feed is dry
		state = VideoAllocationStateFeedDry
	case f.provisional.allocatedLayers == InvalidLayers:
		state = VideoAllocationStateDeficient

		if f.targetLayers != InvalidLayers {
			change = VideoStreamingChangePausing
		}
	default:
		optimalBandwidthNeeded := f.getOptimalBandwidthNeeded(f.provisional.bitrates, f.provisional.maxLayers)
		bandwidthRequested = f.provisional.bitrates[f.provisional.allocatedLayers.Spatial][f.provisional.allocatedLayers.Temporal]
		if f.provisional.allocatedLayers.GreaterThan(f.provisional.maxLayers) || (optimalBandwidthNeeded > 0 && bandwidthRequested >= optimalBandwidthNeeded) {
			// could be greater than optimal if overshooting
			state = VideoAllocationStateOptimal
		} else {
			//
			// Optimal bandwidth could be 0 if using exempted layer.
			// Exempted layer is still treated as deficient.
			//
			state = VideoAllocationStateDeficient
		}

		if f.targetLayers == InvalidLayers {
			change = VideoStreamingChangeResuming
		}
	}

	f.lastAllocation = VideoAllocation{
		state:              state,
		change:             change,
		bandwidthRequested: bandwidthRequested,
		bandwidthDelta:     bandwidthRequested - f.lastAllocation.bandwidthRequested,
		bitrates:           f.provisional.bitrates,
		targetLayers:       f.provisional.allocatedLayers,
		distanceToDesired:  f.getDistanceToDesired(f.provisional.bitrates, f.provisional.allocatedLayers, f.provisional.maxLayers),
	}
	if len(f.provisional.availableLayers) > 0 {
		f.lastAllocation.availableLayers = make([]int32, len(f.provisional.availableLayers))
		copy(f.lastAllocation.availableLayers, f.provisional.availableLayers)
	}
	if len(f.provisional.exemptedLayers) > 0 {
		f.lastAllocation.exemptedLayers = make([]int32, len(f.provisional.exemptedLayers))
		copy(f.lastAllocation.exemptedLayers, f.provisional.exemptedLayers)
	}

	f.setTargetLayers(f.lastAllocation.targetLayers)
	if f.targetLayers == InvalidLayers {
		f.resyncLocked()
	}

	return f.lastAllocation
}

func (f *Forwarder) AllocateNextHigher(availableChannelCapacity int64, brs Bitrates, allowOvershoot bool) (VideoAllocation, bool) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.kind == webrtc.RTPCodecTypeAudio {
		f.lastAllocation.change = VideoStreamingChangeNone
		return f.lastAllocation, false
	}

	// if not deficient, nothing to do
	if f.lastAllocation.state != VideoAllocationStateDeficient {
		f.lastAllocation.change = VideoStreamingChangeNone
		return f.lastAllocation, false
	}

	// if targets are still pending, don't increase
	if f.targetLayers != InvalidLayers && f.targetLayers != f.currentLayers {
		f.lastAllocation.change = VideoStreamingChangeNone
		return f.lastAllocation, false
	}

	optimalBandwidthNeeded := f.getOptimalBandwidthNeeded(brs, f.maxLayers)

	alreadyAllocated := int64(0)
	if f.targetLayers != InvalidLayers {
		alreadyAllocated = brs[f.targetLayers.Spatial][f.targetLayers.Temporal]
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

				if !allowOvershoot && bandwidthRequested-alreadyAllocated > availableChannelCapacity {
					// next higher available layer does not fit, return
					f.lastAllocation.change = VideoStreamingChangeNone
					return true, f.lastAllocation, false
				}

				targetLayers := VideoLayers{Spatial: s, Temporal: t}
				state := VideoAllocationStateDeficient
				if targetLayers.GreaterThan(f.maxLayers) || (optimalBandwidthNeeded > 0 && bandwidthRequested >= optimalBandwidthNeeded) {
					state = VideoAllocationStateOptimal
				}

				change := VideoStreamingChangeNone
				if f.targetLayers == InvalidLayers {
					change = VideoStreamingChangeResuming
				}

				f.lastAllocation = VideoAllocation{
					state:              state,
					change:             change,
					bandwidthRequested: bandwidthRequested,
					bandwidthDelta:     bandwidthRequested - alreadyAllocated,
					bitrates:           brs,
					targetLayers:       targetLayers,
					distanceToDesired:  f.getDistanceToDesired(brs, targetLayers, f.maxLayers),
				}
				if len(f.availableLayers) > 0 {
					f.lastAllocation.availableLayers = make([]int32, len(f.availableLayers))
					copy(f.lastAllocation.availableLayers, f.availableLayers)
				}
				if len(f.exemptedLayers) > 0 {
					f.lastAllocation.exemptedLayers = make([]int32, len(f.exemptedLayers))
					copy(f.lastAllocation.exemptedLayers, f.exemptedLayers)
				}

				f.setTargetLayers(f.lastAllocation.targetLayers)
				return true, f.lastAllocation, true
			}
		}

		return false, VideoAllocation{}, false
	}

	done := false
	var allocation VideoAllocation
	boosted := false

	// try moving temporal layer up in currently streaming spatial layer
	if f.targetLayers != InvalidLayers {
		done, allocation, boosted = doAllocation(
			f.targetLayers.Spatial, f.targetLayers.Spatial,
			f.targetLayers.Temporal+1, f.maxLayers.Temporal,
		)
		if done {
			return allocation, boosted
		}
	}

	// try moving spatial layer up if temporal layer move up is not available
	done, allocation, boosted = doAllocation(
		f.targetLayers.Spatial+1, f.maxLayers.Spatial,
		0, f.maxLayers.Temporal,
	)
	if done {
		return allocation, boosted
	}

	if allowOvershoot && f.maxLayers.IsValid() {
		done, allocation, boosted = doAllocation(
			f.maxLayers.Spatial+1, DefaultMaxLayerSpatial,
			0, DefaultMaxLayerTemporal,
		)
		if done {
			return allocation, boosted
		}
	}

	f.lastAllocation.change = VideoStreamingChangeNone
	return f.lastAllocation, false
}

func (f *Forwarder) GetNextHigherTransition(brs Bitrates, allowOvershoot bool) (VideoTransition, bool) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.kind == webrtc.RTPCodecTypeAudio {
		return VideoTransition{}, false
	}

	// if not deficient, nothing to do
	if f.lastAllocation.state != VideoAllocationStateDeficient {
		return VideoTransition{}, false
	}

	// if targets are still pending, don't increase
	if f.targetLayers != InvalidLayers && f.targetLayers != f.currentLayers {
		return VideoTransition{}, false
	}

	alreadyAllocated := int64(0)
	if f.targetLayers != InvalidLayers {
		alreadyAllocated = brs[f.targetLayers.Spatial][f.targetLayers.Temporal]
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
					from:           f.targetLayers,
					to:             VideoLayers{Spatial: s, Temporal: t},
					bandwidthDelta: bandwidthRequested - alreadyAllocated,
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
	if f.targetLayers != InvalidLayers {
		done, transition, isAvailable = findNextHigher(
			f.targetLayers.Spatial, f.targetLayers.Spatial,
			f.targetLayers.Temporal+1, f.maxLayers.Temporal,
		)
		if done {
			return transition, isAvailable
		}
	}

	// try moving spatial layer up if temporal layer move up is not available
	done, transition, isAvailable = findNextHigher(
		f.targetLayers.Spatial+1, f.maxLayers.Spatial,
		0, f.maxLayers.Temporal,
	)
	if done {
		return transition, isAvailable
	}

	if allowOvershoot && f.maxLayers.IsValid() {
		done, transition, isAvailable = findNextHigher(
			f.maxLayers.Spatial+1, DefaultMaxLayerSpatial,
			0, DefaultMaxLayerTemporal,
		)
		if done {
			return transition, isAvailable
		}
	}

	return VideoTransition{}, false
}

func (f *Forwarder) Pause(brs Bitrates) VideoAllocation {
	f.lock.Lock()
	defer f.lock.Unlock()

	state := VideoAllocationStateNone
	change := VideoStreamingChangeNone

	switch {
	case f.muted:
		state = VideoAllocationStateMuted
	case len(f.availableLayers) == 0:
		// feed is dry
		state = VideoAllocationStateFeedDry
	default:
		// feed bitrate is not yet calculated or pausing due to lack of bandwidth
		state = VideoAllocationStateDeficient

		if f.targetLayers != InvalidLayers {
			change = VideoStreamingChangePausing
		}
	}

	f.lastAllocation = VideoAllocation{
		state:              state,
		change:             change,
		bandwidthRequested: 0,
		bandwidthDelta:     0 - f.lastAllocation.bandwidthRequested,
		bitrates:           brs,
		targetLayers:       InvalidLayers,
		distanceToDesired:  f.getDistanceToDesired(brs, InvalidLayers, f.maxLayers),
	}
	if len(f.availableLayers) > 0 {
		f.lastAllocation.availableLayers = make([]int32, len(f.availableLayers))
		copy(f.lastAllocation.availableLayers, f.availableLayers)
	}
	if len(f.exemptedLayers) > 0 {
		f.lastAllocation.exemptedLayers = make([]int32, len(f.exemptedLayers))
		copy(f.lastAllocation.exemptedLayers, f.exemptedLayers)
	}

	f.setTargetLayers(f.lastAllocation.targetLayers)
	if f.targetLayers == InvalidLayers {
		f.resyncLocked()
	}

	return f.lastAllocation
}

func (f *Forwarder) setTargetLayers(targetLayers VideoLayers) {
	f.targetLayers = targetLayers
	if f.ddLayerSelector != nil {
		f.ddLayerSelector.SelectLayer(targetLayers)
	}
}

func (f *Forwarder) Resync() {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.resyncLocked()
}

func (f *Forwarder) resyncLocked() {
	f.currentLayers = InvalidLayers
	f.lastSSRC = 0
}

func (f *Forwarder) CheckSync() (locked bool, layer int32) {
	f.lock.RLock()
	defer f.lock.RUnlock()

	layer = f.targetLayers.Spatial
	locked = f.targetLayers.Spatial == f.currentLayers.Spatial

	return
}

func (f *Forwarder) FilterRTX(nacks []uint16) (filtered []uint16, disallowedLayers [DefaultMaxLayerSpatial + 1]bool) {
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
	for layer := int32(0); layer < DefaultMaxLayerSpatial+1; layer++ {
		if f.lastAllocation.state == VideoAllocationStateDeficient &&
			(f.targetLayers.Spatial < f.currentLayers.Spatial || layer > f.currentLayers.Spatial) {
			disallowedLayers[layer] = true
		}
	}

	return
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
func (f *Forwarder) getTranslationParamsCommon(extPkt *buffer.ExtPacket, tp *TranslationParams) (*TranslationParams, error) {
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
			// look at RTCP SR to get timestamps and align (and figure out alignment
			// of layers and use that during layer switch in simulcast case).
			// That can get tricky. Given the complexity of that approach, maybe
			// this is just fine till it is not :-).
			// LK-TODO-END

			// Compute how much time passed between the old RTP extPkt
			// and the current packet, and fix timestamp on source change
			tDiffMs := (extPkt.Arrival - f.lTSCalc) / 1e6
			if tDiffMs < 0 {
				tDiffMs = 0
			}
			td := uint32(tDiffMs * int64(f.codec.ClockRate) / 1000)
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

	if tp == nil {
		tp = &TranslationParams{}
	}
	tpRTP, err := f.rtpMunger.UpdateAndGetSnTs(extPkt)
	if err != nil {
		tp.shouldDrop = true
		if err == ErrPaddingOnlyPacket || err == ErrDuplicatePacket || err == ErrOutOfOrderSequenceNumberCacheMiss {
			if err == ErrOutOfOrderSequenceNumberCacheMiss {
				tp.isDroppingRelevant = true
			}
			return tp, nil
		}

		tp.isDroppingRelevant = true
		return tp, err
	}

	tp.rtp = tpRTP
	return tp, nil
}

// should be called with lock held
func (f *Forwarder) getTranslationParamsAudio(extPkt *buffer.ExtPacket) (*TranslationParams, error) {
	return f.getTranslationParamsCommon(extPkt, nil)
}

// should be called with lock held
func (f *Forwarder) getTranslationParamsVideo(extPkt *buffer.ExtPacket, layer int32) (*TranslationParams, error) {
	tp := &TranslationParams{}

	if f.targetLayers == InvalidLayers {
		// stream is paused by streamallocator
		tp.shouldDrop = true
		return tp, nil
	}

	if f.ddLayerSelector != nil {
		if selected := f.ddLayerSelector.Select(extPkt, tp); !selected {
			tp.shouldDrop = true
			f.rtpMunger.PacketDropped(extPkt)
			return tp, nil
		}
	}

	if f.targetLayers.Spatial != f.currentLayers.Spatial {
		if f.targetLayers.Spatial == layer {
			if extPkt.KeyFrame || tp.switchingToTargetLayer {
				// lock to target layer
				f.logger.Infow("locking to target layer", "current", f.currentLayers, "target", f.targetLayers)
				f.currentLayers.Spatial = f.targetLayers.Spatial
				if !f.isTemporalSupported {
					f.currentLayers.Temporal = f.targetLayers.Temporal
				}
				// TODO : we switch to target layer immediately now since we assume all frame chain is integrity
				//   if we have frame chain check, should switch only if target chain is not broken and decodable
				// if f.ddLayerSelector != nil {
				// 	f.ddLayerSelector.SelectLayer(f.currentLayers)
				// }
				if f.currentLayers.Spatial == f.maxLayers.Spatial {
					tp.isSwitchingToMaxLayer = true
				}
			}
		}
	}

	// if we have layer selector, let it decide whether to drop or not
	if f.ddLayerSelector == nil && f.currentLayers.Spatial != layer {
		tp.shouldDrop = true
		return tp, nil
	}

	if FlagPauseOnDowngrade && f.targetLayers.Spatial < f.currentLayers.Spatial && f.lastAllocation.state == VideoAllocationStateDeficient {
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
		// In the case of subscription change, higher should continue streaming
		// to ensure smooth transition.
		//
		// To differentiate, drop only when in DEFICIENT state.
		//
		tp.shouldDrop = true
		tp.isDroppingRelevant = true
		return tp, nil
	}

	_, err := f.getTranslationParamsCommon(extPkt, tp)
	if tp.shouldDrop || f.vp8Munger == nil || len(extPkt.Packet.Payload) == 0 {
		return tp, err
	}

	// catch up temporal layer if necessary
	if f.currentLayers.Temporal != f.targetLayers.Temporal {
		incomingVP8, ok := extPkt.Payload.(buffer.VP8)
		if ok {
			if incomingVP8.TIDPresent == 1 && incomingVP8.TID <= uint8(f.targetLayers.Temporal) {
				f.currentLayers.Temporal = f.targetLayers.Temporal
			}
		}
	}

	tpVP8, err := f.vp8Munger.UpdateAndGet(extPkt, tp.rtp.snOrdering, f.currentLayers.Temporal)
	if err != nil {
		tp.rtp = nil
		tp.shouldDrop = true
		if err == ErrFilteredVP8TemporalLayer || err == ErrOutOfOrderVP8PictureIdCacheMiss {
			if err == ErrFilteredVP8TemporalLayer {
				// filtered temporal layer, update sequence number offset to prevent holes
				f.rtpMunger.PacketDropped(extPkt)
			}
			if err == ErrOutOfOrderVP8PictureIdCacheMiss {
				tp.isDroppingRelevant = true
			}
			return tp, nil
		}

		tp.isDroppingRelevant = true
		return tp, err
	}

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
	if f.targetLayers == InvalidLayers {
		forceMarker = true
	}
	return f.rtpMunger.UpdateAndGetPaddingSnTs(num, 0, 0, forceMarker)
}

func (f *Forwarder) GetSnTsForBlankFrames(frameRate uint32, numPackets int) ([]SnTs, bool, error) {
	f.lock.Lock()
	defer f.lock.Unlock()

	// NOTE: not using diff of current time and previous packet time (lTSCalc) as this
	// driven by a timer, there might be slight differences compared to the frame rate.
	// As the differences are going to be small (and also not to update RTP time stamp
	// by those small differences), not doing the diff.
	f.lTSCalc = time.Now().UnixNano()

	frameEndNeeded := !f.rtpMunger.IsOnFrameBoundary()
	if frameEndNeeded {
		numPackets++
	}
	snts, err := f.rtpMunger.UpdateAndGetPaddingSnTs(numPackets, f.codec.ClockRate, frameRate, frameEndNeeded)
	return snts, frameEndNeeded, err
}

func (f *Forwarder) GetPaddingVP8(frameEndNeeded bool) *buffer.VP8 {
	f.lock.Lock()
	defer f.lock.Unlock()

	return f.vp8Munger.UpdateAndGetPadding(!frameEndNeeded)
}

func (f *Forwarder) GetRTPMungerParams() RTPMungerParams {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.rtpMunger.GetParams()
}

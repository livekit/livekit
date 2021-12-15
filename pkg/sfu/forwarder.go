package sfu

import (
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

//
// Forwarder
//
const (
	FlagPauseOnDowngrade = false
)

type ForwardingStatus int

const (
	ForwardingStatusOff ForwardingStatus = iota
	ForwardingStatusPartial
	ForwardingStatusOptimal
)

type LayerDirection int

const (
	LayerDirectionLowToHigh LayerDirection = iota
	LayerDirectionHighToLow
)

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
	availableLayers    []uint16
	bitrates           Bitrates
	targetLayers       VideoLayers
	distanceToDesired  int32
}

func (v VideoAllocation) String() string {
	return fmt.Sprintf("VideoAllocation{state: %s, change: %s, bw: %d, del: %d, avail: %+v, rates: %+v, target: %s}",
		v.state, v.change, v.bandwidthRequested, v.bandwidthDelta, v.availableLayers, v.bitrates, v.targetLayers)
}

var (
	VideoAllocationDefault = VideoAllocation{
		targetLayers: InvalidLayers,
	}
)

type VideoAllocationProvisional struct {
	layers   VideoLayers
	muted    bool
	bitrates Bitrates
}

type VideoTransition struct {
	from           VideoLayers
	to             VideoLayers
	bandwidthDelta int64
}

const (
	TransitionCostSpatial = 10
)

type TranslationParams struct {
	shouldDrop         bool
	isDroppingRelevant bool
	shouldSendPLI      bool
	rtp                *TranslationParamsRTP
	vp8                *TranslationParamsVP8
}

type VideoLayers struct {
	spatial  int32
	temporal int32
}

func (v VideoLayers) String() string {
	return fmt.Sprintf("VideoLayers{s: %d, t: %d}", v.spatial, v.temporal)
}

func (v VideoLayers) GreaterThan(v2 VideoLayers) bool {
	return v.spatial > v2.spatial || (v.spatial == v2.spatial && v.temporal > v2.temporal)
}

const (
	DefaultMaxLayerSpatial  = int32(2)
	DefaultMaxLayerTemporal = int32(3)
)

var (
	InvalidLayers = VideoLayers{
		spatial:  -1,
		temporal: -1,
	}

	MinLayers = VideoLayers{
		spatial:  0,
		temporal: 0,
	}

	DefaultMaxLayers = VideoLayers{
		spatial:  DefaultMaxLayerSpatial,
		temporal: DefaultMaxLayerTemporal,
	}
)

type Forwarder struct {
	lock  sync.RWMutex
	codec webrtc.RTPCodecCapability
	kind  webrtc.RTPCodecType

	muted bool

	started  bool
	lastSSRC uint32
	lTSCalc  int64

	maxLayers     VideoLayers
	currentLayers VideoLayers
	targetLayers  VideoLayers

	provisional *VideoAllocationProvisional

	lastAllocation VideoAllocation

	availableLayers []uint16

	rtpMunger *RTPMunger
	vp8Munger *VP8Munger
}

func NewForwarder(codec webrtc.RTPCodecCapability, kind webrtc.RTPCodecType) *Forwarder {
	f := &Forwarder{
		codec: codec,
		kind:  kind,

		// start off with nothing, let streamallocator set things
		currentLayers: InvalidLayers,
		targetLayers:  InvalidLayers,

		lastAllocation: VideoAllocationDefault,

		rtpMunger: NewRTPMunger(),
	}

	if strings.ToLower(codec.MimeType) == "video/vp8" {
		f.vp8Munger = NewVP8Munger()
	}

	if f.kind == webrtc.RTPCodecTypeVideo {
		f.maxLayers = DefaultMaxLayers
	} else {
		f.maxLayers = InvalidLayers
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

	if f.kind == webrtc.RTPCodecTypeAudio || spatialLayer == f.maxLayers.spatial {
		return false, InvalidLayers
	}

	f.maxLayers.spatial = spatialLayer

	return true, f.maxLayers
}

func (f *Forwarder) SetMaxTemporalLayer(temporalLayer int32) (bool, VideoLayers) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.kind == webrtc.RTPCodecTypeAudio || temporalLayer == f.maxLayers.temporal {
		return false, InvalidLayers
	}

	f.maxLayers.temporal = temporalLayer

	return true, f.maxLayers
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

	if f.targetLayers == InvalidLayers {
		return ForwardingStatusOff
	}

	if f.targetLayers.spatial < f.maxLayers.spatial {
		return ForwardingStatusPartial
	}

	return ForwardingStatusOptimal
}

func (f *Forwarder) UptrackLayersChange(availableLayers []uint16) {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.availableLayers = availableLayers
}

func (f *Forwarder) getOptimalBandwidthNeeded(brs Bitrates) int64 {
	for i := f.maxLayers.spatial; i >= 0; i-- {
		for j := f.maxLayers.temporal; j >= 0; j-- {
			if brs[i][j] == 0 {
				continue
			}

			return brs[i][j]
		}
	}

	return 0
}

func (f *Forwarder) getDistanceToDesired(brs Bitrates, targetLayers VideoLayers) int32 {
	if f.muted {
		return 0
	}

	distance := int32(0)
	for s := f.maxLayers.spatial; s >= 0; s-- {
		found := false
		for t := f.maxLayers.temporal; t >= 0; t-- {
			if brs[s][t] == 0 {
				continue
			}
			if s == targetLayers.spatial && t == targetLayers.temporal {
				found = true
				break
			}

			distance++
		}

		if found {
			break
		}
	}

	return distance
}

func (f *Forwarder) IsDeficient() bool {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.lastAllocation.state == VideoAllocationStateDeficient
}

func (f *Forwarder) BandwidthRequested() int64 {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.lastAllocation.bandwidthRequested
}

func (f *Forwarder) DistanceToDesired() int32 {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.lastAllocation.distanceToDesired
}

func (f *Forwarder) Allocate(availableChannelCapacity int64, brs Bitrates) VideoAllocation {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.kind == webrtc.RTPCodecTypeAudio {
		return f.lastAllocation
	}

	optimalBandwidthNeeded := f.getOptimalBandwidthNeeded(brs)

	state := VideoAllocationStateNone
	change := VideoStreamingChangeNone
	bandwidthRequested := int64(0)
	targetLayers := InvalidLayers

	switch {
	case f.muted:
		state = VideoAllocationStateMuted
	case optimalBandwidthNeeded == 0:
		if len(f.availableLayers) == 0 {
			// feed is dry
			state = VideoAllocationStateFeedDry
		} else {
			// feed bitrate is not yet calculated
			state = VideoAllocationStateAwaitingMeasurement

			if availableChannelCapacity == ChannelCapacityInfinity {
				//
				// Channel capacity allows a free pass.
				// So, resume with the highest layer available <= max subscribed layer
				// If already resumed, move allocation to the highest available layer <= max subscribed layer
				//
				targetLayers.spatial = int32(math.Min(float64(f.maxLayers.spatial), float64(f.availableLayers[len(f.availableLayers)-1])))
				targetLayers.temporal = int32(math.Max(0, float64(f.maxLayers.temporal)))

				if f.targetLayers == InvalidLayers {
					change = VideoStreamingChangeResuming
				}
			} else {
				// disable forwarding as it is not known how big this stream is
				// and if it will fit in the available channel capacity
				f.currentLayers = InvalidLayers

				state = VideoAllocationStateDeficient

				if f.targetLayers != InvalidLayers {
					change = VideoStreamingChangePausing
				}
			}
		}
	default:
		// allocate best layer that fits
		for s := f.maxLayers.spatial; s >= 0; s-- {
			for t := f.maxLayers.temporal; t >= 0; t-- {
				if brs[s][t] == 0 {
					continue
				}

				if brs[s][t] <= availableChannelCapacity {
					targetLayers = VideoLayers{
						spatial:  s,
						temporal: t,
					}

					bandwidthRequested = brs[s][t]
					if bandwidthRequested == optimalBandwidthNeeded {
						state = VideoAllocationStateOptimal
					} else {
						state = VideoAllocationStateDeficient
					}

					if f.targetLayers == InvalidLayers {
						change = VideoStreamingChangeResuming
					}
					break
				}
			}
			if bandwidthRequested != 0 {
				break
			}
		}

		if bandwidthRequested == 0 {
			state = VideoAllocationStateDeficient

			if f.targetLayers != InvalidLayers {
				change = VideoStreamingChangePausing
			}
		}
	}

	f.lastAllocation = VideoAllocation{
		state:              state,
		change:             change,
		bandwidthRequested: bandwidthRequested,
		bandwidthDelta:     bandwidthRequested - f.lastAllocation.bandwidthRequested,
		availableLayers:    f.availableLayers,
		bitrates:           brs,
		targetLayers:       targetLayers,
		distanceToDesired:  f.getDistanceToDesired(brs, targetLayers),
	}
	f.targetLayers = f.lastAllocation.targetLayers
	if f.targetLayers == InvalidLayers {
		f.currentLayers = InvalidLayers
	}

	return f.lastAllocation
}

func (f *Forwarder) ProvisionalAllocatePrepare(bitrates Bitrates) {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.provisional = &VideoAllocationProvisional{
		layers:   InvalidLayers,
		muted:    f.muted,
		bitrates: bitrates,
	}
}

func (f *Forwarder) ProvisionalAllocate(availableChannelCapacity int64, layers VideoLayers) int64 {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.provisional.muted {
		return 0
	}

	if layers.GreaterThan(f.maxLayers) {
		return 0
	}

	requiredBitrate := f.provisional.bitrates[layers.spatial][layers.temporal]
	if requiredBitrate == 0 {
		return 0
	}

	alreadyAllocatedBitrate := int64(0)
	if f.provisional.layers != InvalidLayers {
		alreadyAllocatedBitrate = f.provisional.bitrates[f.provisional.layers.spatial][f.provisional.layers.temporal]
	}

	if requiredBitrate <= (availableChannelCapacity + alreadyAllocatedBitrate) {
		f.provisional.layers = layers
		return requiredBitrate
	}

	return 0
}

func (f *Forwarder) ProvisionalAllocateGetCooperativeTransition() VideoTransition {
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
	//   - Try to keep tracks streaming, i. e. no pauses even if not at optimal layers
	//   - Do not make an upgrade as it could affect other tracks
	//
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.provisional.muted {
		f.provisional.layers = InvalidLayers
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
		for s := f.maxLayers.spatial; s >= 0; s-- {
			for t := f.maxLayers.temporal; t >= 0; t-- {
				if f.provisional.bitrates[s][t] != 0 {
					maximalLayers = VideoLayers{spatial: s, temporal: t}
					maximalBandwidthRequired = f.provisional.bitrates[s][t]
					break
				}
			}

			if maximalBandwidthRequired != 0 {
				break
			}
		}

		if maximalLayers != InvalidLayers {
			if !f.targetLayers.GreaterThan(maximalLayers) && (f.provisional.bitrates[f.targetLayers.spatial][f.targetLayers.temporal] != 0) {
				// currently streaming and wanting an upgrade, just preserve current target in the cooperative scheme of things
				f.provisional.layers = f.targetLayers
				return VideoTransition{
					from:           f.targetLayers,
					to:             f.targetLayers,
					bandwidthDelta: 0,
				}
			}

			if f.targetLayers.GreaterThan(maximalLayers) {
				// maximalLayers <= f.targetLayers, make the down move
				f.provisional.layers = maximalLayers
				return VideoTransition{
					from:           f.targetLayers,
					to:             maximalLayers,
					bandwidthDelta: maximalBandwidthRequired - f.lastAllocation.bandwidthRequested,
				}
			}
		}
	}

	// currently not streaming, find minimal
	// NOTE: a layer in feed could have paused and there could be other options than going back to minimal, but the cooperative scheme knocks things back to minimal
	minimalLayers := InvalidLayers
	bandwidthRequired := int64(0)
	for s := int32(0); s <= f.maxLayers.spatial; s++ {
		for t := int32(0); s <= f.maxLayers.temporal; t++ {
			if f.provisional.bitrates[s][t] != 0 {
				minimalLayers = VideoLayers{spatial: s, temporal: t}
				bandwidthRequired = f.provisional.bitrates[s][t]
				break
			}
		}

		if bandwidthRequired != 0 {
			break
		}
	}

	targetLayers := f.targetLayers
	if targetLayers == InvalidLayers || targetLayers.GreaterThan(minimalLayers) || (f.provisional.bitrates[targetLayers.spatial][targetLayers.temporal] == 0) {
		targetLayers = minimalLayers
	}

	f.provisional.layers = targetLayers
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
	// The goal is to keep all tracks streaming as much as possible. So, the track that needs a change needs bits to be unpaused.
	//
	// This tries to figure out how much it can contribute
	//   1. Track muted OR feed dry - can contribute everything back in case it was using bits.
	//   2. Look at all possible down transitions from current target and find the best offer.
	//      Best offer is calculated as bits saved moving to a down layer divided by cost.
	//      Cost has two components
	//        a. Transition cost: Spatial layer switch is expensive due to key frame requiremnt, but temporal layer switch is free.
	//        b. Quality cost: The farther away from desired layers, the higher the quality cost.
	//
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.provisional.muted {
		f.provisional.layers = InvalidLayers
		return VideoTransition{
			from:           f.targetLayers,
			to:             InvalidLayers,
			bandwidthDelta: 0 - f.lastAllocation.bandwidthRequested,
			// LK-TODO should this take current bitrate of current target layers?
		}
	}

	maxReachableLayerTemporal := int32(-1)
	for t := f.maxLayers.temporal; t >= 0; t-- {
		for s := f.maxLayers.spatial; s >= 0; s-- {
			if f.provisional.bitrates[s][t] != 0 {
				maxReachableLayerTemporal = t
				break
			}
		}
		if maxReachableLayerTemporal != -1 {
			break
		}
	}

	if maxReachableLayerTemporal == -1 {
		// feed has gone dry,
		f.provisional.layers = InvalidLayers
		return VideoTransition{
			from:           f.targetLayers,
			to:             InvalidLayers,
			bandwidthDelta: 0 - f.lastAllocation.bandwidthRequested,
		}
	}

	// starting from mimimum to target, find transition which gives the best
	// transition taking into account bits saved vs cost of such a transition
	bestLayers := InvalidLayers
	bestBandwidthDelta := int64(0)
	bestValue := float32(0)
	for s := int32(0); s <= f.targetLayers.spatial; s++ {
		for t := int32(0); t <= f.targetLayers.temporal; t++ {
			if s == f.targetLayers.spatial && t == f.targetLayers.temporal {
				break
			}

			bandwidthDelta := int64(math.Max(float64(0), float64(f.lastAllocation.bandwidthRequested-f.provisional.bitrates[s][t])))

			transitionCost := int32(0)
			if f.targetLayers.spatial != s {
				transitionCost = TransitionCostSpatial
			}

			qualityCost := (maxReachableLayerTemporal+1)*(f.targetLayers.spatial-s) + (f.targetLayers.temporal - t)

			value := float32(0)
			if (transitionCost + qualityCost) != 0 {
				value = float32(bandwidthDelta) / float32(transitionCost+qualityCost)
			}
			if value > bestValue || (value == bestValue && bandwidthDelta > bestBandwidthDelta) {
				bestValue = value
				bestBandwidthDelta = bandwidthDelta
				bestLayers = VideoLayers{spatial: s, temporal: t}
			}
		}
	}

	f.provisional.layers = bestLayers
	return VideoTransition{
		from:           f.targetLayers,
		to:             bestLayers,
		bandwidthDelta: bestBandwidthDelta,
	}
}

func (f *Forwarder) ProvisionalAllocateCommit() VideoAllocation {
	f.lock.Lock()
	defer f.lock.Unlock()

	optimalBandwidthNeeded := f.getOptimalBandwidthNeeded(f.provisional.bitrates)

	state := VideoAllocationStateNone
	change := VideoStreamingChangeNone
	bandwidthRequested := int64(0)

	switch {
	case f.provisional.muted:
		state = VideoAllocationStateMuted
	case optimalBandwidthNeeded == 0:
		if len(f.availableLayers) == 0 {
			// feed is dry
			state = VideoAllocationStateFeedDry
		} else {
			// feed bitrate is not yet calculated
			state = VideoAllocationStateDeficient

			// disable forwarding as it is not known how big this stream is
			// and if it will fit in the available channel capacity

			if f.targetLayers != InvalidLayers {
				change = VideoStreamingChangePausing
			}
		}
	case f.provisional.layers == InvalidLayers:
		state = VideoAllocationStateDeficient

		if f.targetLayers != InvalidLayers {
			change = VideoStreamingChangePausing
		}
	default:
		bandwidthRequested = f.provisional.bitrates[f.provisional.layers.spatial][f.provisional.layers.temporal]
		if bandwidthRequested == optimalBandwidthNeeded {
			state = VideoAllocationStateOptimal
		} else {
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
		availableLayers:    f.availableLayers,
		bitrates:           f.provisional.bitrates,
		targetLayers:       f.provisional.layers,
		distanceToDesired:  f.getDistanceToDesired(f.provisional.bitrates, f.provisional.layers),
	}
	f.targetLayers = f.lastAllocation.targetLayers
	if f.targetLayers == InvalidLayers {
		f.currentLayers = InvalidLayers
	}

	return f.lastAllocation
}

func (f *Forwarder) FinalizeAllocate(brs Bitrates) VideoAllocation {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.lastAllocation.state != VideoAllocationStateAwaitingMeasurement {
		return f.lastAllocation
	}

	optimalBandwidthNeeded := f.getOptimalBandwidthNeeded(brs)
	if optimalBandwidthNeeded == 0 {
		if len(f.availableLayers) == 0 {
			// feed dry
			f.lastAllocation = VideoAllocation{
				state:              VideoAllocationStateFeedDry,
				change:             VideoStreamingChangeNone,
				bandwidthRequested: 0,
				bandwidthDelta:     0 - f.lastAllocation.bandwidthRequested,
				availableLayers:    f.availableLayers,
				bitrates:           brs,
				targetLayers:       InvalidLayers,
				distanceToDesired:  f.getDistanceToDesired(brs, InvalidLayers),
			}
			f.targetLayers = f.lastAllocation.targetLayers
			if f.targetLayers == InvalidLayers {
				f.currentLayers = InvalidLayers
			}
		}

		// still awaiting measurement
		return f.lastAllocation
	}

	// finalize using optimal layer
	for s := f.maxLayers.spatial; s >= 0; s-- {
		for t := f.maxLayers.temporal; t >= 0; t-- {
			bandwidthRequested := brs[s][t]
			if bandwidthRequested == 0 {
				continue
			}

			state := VideoAllocationStateOptimal
			if bandwidthRequested != optimalBandwidthNeeded {
				state = VideoAllocationStateDeficient
			}

			change := VideoStreamingChangeNone
			if f.targetLayers == InvalidLayers {
				change = VideoStreamingChangeResuming
			}

			targetLayers := VideoLayers{spatial: s, temporal: t}
			f.lastAllocation = VideoAllocation{
				state:              state,
				change:             change,
				bandwidthRequested: bandwidthRequested,
				bandwidthDelta:     bandwidthRequested - f.lastAllocation.bandwidthRequested,
				availableLayers:    f.availableLayers,
				bitrates:           brs,
				targetLayers:       targetLayers,
				distanceToDesired:  f.getDistanceToDesired(brs, targetLayers),
			}
			f.targetLayers = f.lastAllocation.targetLayers
			return f.lastAllocation
		}
	}

	return f.lastAllocation
}

func (f *Forwarder) AllocateNextHigher(brs Bitrates) (VideoAllocation, bool) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.kind == webrtc.RTPCodecTypeAudio {
		return f.lastAllocation, false
	}

	// if not deficient, nothing to do
	if f.lastAllocation.state != VideoAllocationStateDeficient {
		return f.lastAllocation, false
	}

	// if targets are still pending, don't increase
	if f.targetLayers != InvalidLayers && f.targetLayers != f.currentLayers {
		return f.lastAllocation, false
	}

	optimalBandwidthNeeded := f.getOptimalBandwidthNeeded(brs)
	if optimalBandwidthNeeded == 0 {
		// either feed is dry or awaiting measurement, don't hunt for higher
		return f.lastAllocation, false
	}

	// try moving temporal layer up in currently streaming spatial layer
	if f.targetLayers != InvalidLayers {
		for t := f.targetLayers.temporal + 1; t <= f.maxLayers.temporal; t++ {
			bandwidthRequested := brs[f.targetLayers.spatial][t]
			if bandwidthRequested == 0 {
				continue
			}

			state := VideoAllocationStateOptimal
			if bandwidthRequested != optimalBandwidthNeeded {
				state = VideoAllocationStateDeficient
			}

			targetLayers := VideoLayers{spatial: f.targetLayers.spatial, temporal: t}
			f.lastAllocation = VideoAllocation{
				state:              state,
				change:             VideoStreamingChangeNone,
				bandwidthRequested: bandwidthRequested,
				bandwidthDelta:     bandwidthRequested - f.lastAllocation.bandwidthRequested,
				availableLayers:    f.availableLayers,
				bitrates:           brs,
				targetLayers:       targetLayers,
				distanceToDesired:  f.getDistanceToDesired(brs, targetLayers),
			}
			f.targetLayers = f.lastAllocation.targetLayers
			return f.lastAllocation, true
		}
	}

	// try moving spatial layer up if temporal layer move up is not available
	for s := f.targetLayers.spatial + 1; s <= f.maxLayers.spatial; s++ {
		for t := int32(0); t <= f.maxLayers.temporal; t++ {
			bandwidthRequested := brs[s][t]
			if bandwidthRequested == 0 {
				continue
			}

			state := VideoAllocationStateOptimal
			if bandwidthRequested != optimalBandwidthNeeded {
				state = VideoAllocationStateDeficient
			}

			change := VideoStreamingChangeNone
			if f.targetLayers == InvalidLayers {
				change = VideoStreamingChangeResuming
			}

			targetLayers := VideoLayers{spatial: s, temporal: t}
			f.lastAllocation = VideoAllocation{
				state:              state,
				change:             change,
				bandwidthRequested: bandwidthRequested,
				bandwidthDelta:     bandwidthRequested - f.lastAllocation.bandwidthRequested,
				availableLayers:    f.availableLayers,
				bitrates:           brs,
				targetLayers:       targetLayers,
				distanceToDesired:  f.getDistanceToDesired(brs, targetLayers),
			}
			f.targetLayers = f.lastAllocation.targetLayers
			return f.lastAllocation, true
		}
	}

	return f.lastAllocation, false
}

func (f *Forwarder) Pause(brs Bitrates) VideoAllocation {
	f.lock.Lock()
	defer f.lock.Unlock()

	optimalBandwidthNeeded := f.getOptimalBandwidthNeeded(brs)

	state := VideoAllocationStateNone
	change := VideoStreamingChangeNone

	switch {
	case f.muted:
		state = VideoAllocationStateMuted
	case optimalBandwidthNeeded == 0:
		if len(f.availableLayers) == 0 {
			// feed is dry
			state = VideoAllocationStateFeedDry
		} else {
			// feed bitrate is not yet calculated
			state = VideoAllocationStateDeficient

			if f.targetLayers != InvalidLayers {
				change = VideoStreamingChangePausing
			}
		}
	default:
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
		availableLayers:    f.availableLayers,
		bitrates:           brs,
		targetLayers:       InvalidLayers,
		distanceToDesired:  f.getDistanceToDesired(brs, InvalidLayers),
	}
	f.targetLayers = f.lastAllocation.targetLayers
	if f.targetLayers == InvalidLayers {
		f.currentLayers = InvalidLayers
	}

	return f.lastAllocation
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
func (f *Forwarder) getTranslationParamsVideo(extPkt *buffer.ExtPacket, layer int32) (*TranslationParams, error) {
	tp := &TranslationParams{}

	if f.targetLayers == InvalidLayers {
		// stream is paused by streamallocator
		tp.shouldDrop = true
		return tp, nil
	}

	tp.shouldSendPLI = false
	if f.targetLayers.spatial != f.currentLayers.spatial {
		if f.targetLayers.spatial == layer {
			if extPkt.KeyFrame {
				// lock to target layer
				f.currentLayers.spatial = f.targetLayers.spatial
			} else {
				tp.shouldSendPLI = true
			}
		}
	}

	if f.currentLayers.spatial != layer {
		tp.shouldDrop = true
		return tp, nil
	}

	if FlagPauseOnDowngrade && f.targetLayers.spatial < f.currentLayers.spatial && f.targetLayers.spatial < f.maxLayers.spatial {
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
		tp.isDroppingRelevant = true
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
			if err == ErrOutOfOrderSequenceNumberCacheMiss {
				tp.isDroppingRelevant = true
			}
			return tp, nil
		}

		tp.isDroppingRelevant = true
		return tp, err
	}

	if f.vp8Munger == nil || len(extPkt.Packet.Payload) == 0 {
		tp.rtp = tpRTP
		return tp, nil
	}

	// catch up temporal layer if necessary
	if f.currentLayers.temporal != f.targetLayers.temporal {
		incomingVP8, ok := extPkt.Payload.(buffer.VP8)
		if ok {
			if incomingVP8.TIDPresent == 1 && incomingVP8.TID <= uint8(f.targetLayers.temporal) {
				f.currentLayers.temporal = f.targetLayers.temporal
			}
		}
	}

	tpVP8, err := f.vp8Munger.UpdateAndGet(extPkt, tpRTP.snOrdering, f.currentLayers.temporal)
	if err != nil {
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
	if f.targetLayers == InvalidLayers {
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

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

type LayerPreference int

const (
	LayerPreferenceNone LayerPreference = iota
	LayerPreferenceSpatial
	LayerPreferenceTemporal
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
	bitrates           [3][4]int64
	targetLayers       VideoLayers
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

type TranslationParams struct {
	shouldDrop    bool
	shouldSendPLI bool
	rtp           *TranslationParamsRTP
	vp8           *TranslationParamsVP8
}

type VideoLayers struct {
	spatial  int32
	temporal int32
}

func (v VideoLayers) String() string {
	return fmt.Sprintf("VideoLayers{s: %d, t: %d}", v.spatial, v.temporal)
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

	layerPref LayerPreference

	muted bool

	started  bool
	lastSSRC uint32
	lTSCalc  int64

	maxLayers     VideoLayers
	currentLayers VideoLayers
	targetLayers  VideoLayers

	lastAllocation VideoAllocation

	availableLayers []uint16

	rtpMunger *RTPMunger
	vp8Munger *VP8Munger
}

func NewForwarder(codec webrtc.RTPCodecCapability, kind webrtc.RTPCodecType) *Forwarder {
	f := &Forwarder{
		codec: codec,
		kind:  kind,

		layerPref: LayerPreferenceSpatial,

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

func (f *Forwarder) SetMaxSpatialLayer(spatialLayer int32) (bool, VideoLayers, LayerPreference) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.kind == webrtc.RTPCodecTypeAudio || spatialLayer == f.maxLayers.spatial {
		return false, InvalidLayers, LayerPreferenceNone
	}

	f.maxLayers.spatial = spatialLayer

	return true, f.maxLayers, f.layerPref
}

func (f *Forwarder) SetMaxTemporalLayer(temporalLayer int32) (bool, VideoLayers, LayerPreference) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.kind == webrtc.RTPCodecTypeAudio || temporalLayer == f.maxLayers.temporal {
		return false, InvalidLayers, LayerPreferenceNone
	}

	f.maxLayers.temporal = temporalLayer

	return true, f.maxLayers, f.layerPref
}

func (f *Forwarder) MaxLayers() VideoLayers {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.maxLayers
}

func (f *Forwarder) GetLayerPreference() LayerPreference {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.layerPref
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

func (f *Forwarder) disable() {
	f.currentLayers = InvalidLayers
	f.targetLayers = InvalidLayers
}

func (f *Forwarder) getOptimalBandwidthNeeded(brs [3][4]int64) int64 {
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

func (f *Forwarder) findBestLayers(
	minLayers VideoLayers,
	maxLayers VideoLayers,
	brs [3][4]int64,
	optimalBandwidthNeeded int64,
	direction LayerDirection,
	preference LayerPreference,
	availableChannelCapacity int64,
	canPause bool,
) VideoAllocation {
	targetLayers := InvalidLayers

	switch direction {
	case LayerDirectionLowToHigh:
		switch preference {
		case LayerPreferenceSpatial:
			for i := minLayers.spatial; i <= maxLayers.spatial; i++ {
				for j := minLayers.temporal; j <= maxLayers.temporal; j++ {
					if brs[i][j] != 0 && brs[i][j] <= availableChannelCapacity {
						targetLayers = VideoLayers{
							spatial:  i,
							temporal: j,
						}
						break
					}
				}
				if targetLayers != InvalidLayers {
					break
				}
			}
		case LayerPreferenceTemporal:
			for i := minLayers.temporal; i <= maxLayers.temporal; i++ {
				for j := minLayers.spatial; j <= maxLayers.spatial; j++ {
					if brs[j][i] != 0 && brs[j][i] <= availableChannelCapacity {
						targetLayers = VideoLayers{
							spatial:  j,
							temporal: i,
						}
						break
					}
				}
				if targetLayers != InvalidLayers {
					break
				}
			}
		}
	case LayerDirectionHighToLow:
		switch preference {
		case LayerPreferenceSpatial:
			for i := maxLayers.spatial; i >= minLayers.spatial; i-- {
				for j := maxLayers.temporal; j >= minLayers.temporal; j-- {
					if brs[i][j] != 0 && brs[i][j] <= availableChannelCapacity {
						targetLayers = VideoLayers{
							spatial:  i,
							temporal: j,
						}
						break
					}
				}
				if targetLayers != InvalidLayers {
					break
				}
			}
		case LayerPreferenceTemporal:
			for i := maxLayers.temporal; i >= minLayers.temporal; i-- {
				for j := maxLayers.spatial; j >= minLayers.spatial; j-- {
					if brs[j][i] != 0 && brs[j][i] <= availableChannelCapacity {
						targetLayers = VideoLayers{
							spatial:  j,
							temporal: i,
						}
						break
					}
				}
				if targetLayers != InvalidLayers {
					break
				}
			}
		}
	}

	if targetLayers == InvalidLayers && !canPause {
		//
		// Do not pause if preserving even if allocation does not fit in available channel capacity.
		//
		// Note that currently streamed layers could have a different bitrate compared to
		// when the allocation was done. But not updating to avoid any unnecessary perturbation
		// in the allocation system. Let channel changes happen and update state as needed via
		// a fresh allocation.
		//
		return f.lastAllocation
	}

	var allocation VideoAllocation

	// change in streaming state?
	switch {
	case f.targetLayers != InvalidLayers && targetLayers == InvalidLayers:
		allocation.change = VideoStreamingChangePausing
	case f.targetLayers == InvalidLayers && targetLayers != InvalidLayers:
		allocation.change = VideoStreamingChangeResuming
	}

	// how much bandwidth is needed and delta from previous allocation
	if targetLayers == InvalidLayers {
		allocation.bandwidthRequested = 0
	} else {
		allocation.bandwidthRequested = brs[targetLayers.spatial][targetLayers.temporal]
	}
	allocation.bandwidthDelta = allocation.bandwidthRequested - f.lastAllocation.bandwidthRequested

	// state of allocation
	if allocation.bandwidthRequested == optimalBandwidthNeeded {
		allocation.state = VideoAllocationStateOptimal
	} else {
		allocation.state = VideoAllocationStateDeficient
	}

	allocation.availableLayers = f.availableLayers
	allocation.bitrates = brs
	allocation.targetLayers = targetLayers

	return allocation
}

func (f *Forwarder) allocate(availableChannelCapacity int64, canPause bool, brs [3][4]int64) {
	// should never get called on audio tracks, just for safety
	if f.kind == webrtc.RTPCodecTypeAudio {
		return
	}

	if f.muted {
		f.lastAllocation = VideoAllocation{
			state:              VideoAllocationStateMuted,
			change:             VideoStreamingChangeNone,
			bandwidthRequested: 0,
			bandwidthDelta:     0 - f.lastAllocation.bandwidthRequested,
			availableLayers:    f.availableLayers,
			bitrates:           brs,
			targetLayers:       f.targetLayers,
		}
		return
	}

	optimalBandwidthNeeded := f.getOptimalBandwidthNeeded(brs)
	if optimalBandwidthNeeded == 0 {
		if len(f.availableLayers) == 0 {
			// feed is dry
			f.lastAllocation = VideoAllocation{
				state:              VideoAllocationStateFeedDry,
				change:             VideoStreamingChangeNone,
				bandwidthRequested: 0,
				bandwidthDelta:     0 - f.lastAllocation.bandwidthRequested,
				availableLayers:    f.availableLayers,
				bitrates:           brs,
				targetLayers:       f.targetLayers,
			}
			return
		}

		// feed bitrate is not yet calculated
		if availableChannelCapacity == ChannelCapacityInfinity {
			//
			// Channel capacity allows a free pass.
			// So, resume with the highest layer available <= max subscribed layer
			// If already resumed, move allocation to the highest available layer <= max subscribed layer
			//
			change := VideoStreamingChangeNone
			if f.targetLayers == InvalidLayers {
				change = VideoStreamingChangeResuming
			}

			f.targetLayers.spatial = int32(f.availableLayers[len(f.availableLayers)-1])
			if f.targetLayers.spatial > f.maxLayers.spatial {
				f.targetLayers.spatial = f.maxLayers.spatial
			}

			f.targetLayers.temporal = int32(math.Max(0, float64(f.maxLayers.temporal)))

			f.lastAllocation = VideoAllocation{
				state:              VideoAllocationStateAwaitingMeasurement,
				change:             change,
				bandwidthRequested: 0, // unavailable yet
				bandwidthDelta:     0 - f.lastAllocation.bandwidthRequested,
				availableLayers:    f.availableLayers,
				bitrates:           brs,
				targetLayers:       f.targetLayers,
			}
		} else {
			// if not optimistically started, nothing else to do
			if f.targetLayers == InvalidLayers {
				f.lastAllocation = VideoAllocation{
					state:              VideoAllocationStateDeficient,
					change:             VideoStreamingChangeNone,
					bandwidthRequested: 0, // unavailable yet
					bandwidthDelta:     0 - f.lastAllocation.bandwidthRequested,
					availableLayers:    f.availableLayers,
					bitrates:           brs,
					targetLayers:       f.targetLayers,
				}
			} else if canPause {
				// disable it as it is not known how big this stream is
				// and if it will fit in the available channel capacity
				f.disable()

				f.lastAllocation = VideoAllocation{
					state:              VideoAllocationStateDeficient,
					change:             VideoStreamingChangePausing,
					bandwidthRequested: 0, // unavailable yet
					bandwidthDelta:     0 - f.lastAllocation.bandwidthRequested,
					availableLayers:    f.availableLayers,
					bitrates:           brs,
					targetLayers:       f.targetLayers,
				}
			}
		}
		return
	}

	f.lastAllocation = f.findBestLayers(
		MinLayers,
		f.maxLayers,
		brs,
		optimalBandwidthNeeded,
		LayerDirectionHighToLow,
		f.layerPref,
		availableChannelCapacity,
		canPause,
	)
	f.targetLayers = f.lastAllocation.targetLayers
}

func (f *Forwarder) Allocate(availableChannelCapacity int64, brs [3][4]int64) VideoAllocation {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.allocate(availableChannelCapacity, true, brs)
	return f.lastAllocation
}

func (f *Forwarder) TryAllocate(additionalChannelCapacity int64, brs [3][4]int64) VideoAllocation {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.allocate(f.lastAllocation.bandwidthRequested+additionalChannelCapacity, false, brs)
	return f.lastAllocation
}

func (f *Forwarder) FinalizeAllocate(brs [3][4]int64) VideoAllocation {
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
				targetLayers:       f.targetLayers,
			}
		}

		// still awaiting measurement
		return f.lastAllocation
	}

	f.lastAllocation = f.findBestLayers(
		MinLayers,
		f.maxLayers,
		brs,
		optimalBandwidthNeeded,
		LayerDirectionHighToLow,
		f.layerPref,
		ChannelCapacityInfinity,
		false,
	)
	f.targetLayers = f.lastAllocation.targetLayers
	return f.lastAllocation
}

func (f *Forwarder) AllocateNextHigher(brs [3][4]int64) (VideoAllocation, bool) {
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
		minLayers := VideoLayers{
			spatial:  f.targetLayers.spatial,
			temporal: f.targetLayers.temporal + 1,
		}
		maxLayers := VideoLayers{
			spatial:  f.targetLayers.spatial,
			temporal: f.maxLayers.temporal,
		}
		allocation := f.findBestLayers(
			minLayers,
			maxLayers,
			brs,
			optimalBandwidthNeeded,
			LayerDirectionLowToHigh,
			f.layerPref,
			ChannelCapacityInfinity,
			false,
		)
		if allocation.targetLayers != f.targetLayers {
			f.lastAllocation = allocation
			f.targetLayers = allocation.targetLayers
			return f.lastAllocation, true
		}
	}

	// try moving spatial layer up if temporal layer move up is not available
	minLayers := VideoLayers{
		spatial:  f.targetLayers.spatial + 1,
		temporal: 0,
	}
	maxLayers := VideoLayers{
		spatial:  f.maxLayers.spatial,
		temporal: f.maxLayers.temporal,
	}
	allocation := f.findBestLayers(
		minLayers,
		maxLayers,
		brs,
		optimalBandwidthNeeded,
		LayerDirectionLowToHigh,
		f.layerPref,
		ChannelCapacityInfinity,
		false,
	)
	if allocation.targetLayers != f.targetLayers {
		f.lastAllocation = allocation
		f.targetLayers = allocation.targetLayers
		return f.lastAllocation, true
	}

	return f.lastAllocation, false
}

/*
func (f *Forwarder) AllocationState() VideoAllocationState {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.lastAllocationState
}

func (f *Forwarder) AllocationBandwidth() int64 {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.lastAllocationRequestBps
}
*/
func (f *Forwarder) LastAllocation() VideoAllocation {
	f.lock.RLock()
	defer f.lock.RUnlock()

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
			return tp, nil
		}

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
			return tp, nil
		}

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
			return tp, nil
		}

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

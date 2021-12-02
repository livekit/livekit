package sfu

import (
	"fmt"
	"strings"
	"sync"

	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

//
// Forwarder
//
type VideoStreamingChange int

const (
	VideoStreamingChangeNone VideoStreamingChange = iota
	VideoStreamingChangePausing
	VideoStreamingChangeResuming
)

type VideoAllocationState int

const (
	VideoAllocationStateNone VideoAllocationState = iota
	VideoAllocationStateMuted
	VideoAllocationStateFeedDry
	VideoAllocationStateAwaitingMeasurement
	VideoAllocationStateOptimal
	VideoAllocationStateDeficient
)

type VideoAllocationResult struct {
	change             VideoStreamingChange
	state              VideoAllocationState
	bandwidthRequested int64
	bandwidthDelta     int64
}

type Forwarder struct {
	id string	// REMOVE
	peerId string	// REMOVE

	lock  sync.RWMutex
	codec webrtc.RTPCodecCapability
	kind  webrtc.RTPCodecType

	muted bool

	started  bool
	lastSSRC uint32
	lTSCalc  int64

	maxSpatialLayer     int32
	currentSpatialLayer int32
	targetSpatialLayer  int32

	maxTemporalLayer     int32
	currentTemporalLayer int32
	targetTemporalLayer  int32

	lastAllocationState      VideoAllocationState
	lastAllocationRequestBps int64

	availableLayers []uint16

	rtpMunger *RTPMunger
	vp8Munger *VP8Munger
}

func NewForwarder(id string, peerId string, codec webrtc.RTPCodecCapability, kind webrtc.RTPCodecType) *Forwarder {
	f := &Forwarder{
		id: id,
		peerId: peerId,
		codec: codec,
		kind:  kind,

		// start off with nothing, let streamallocator set things
		currentSpatialLayer:  InvalidSpatialLayer,
		targetSpatialLayer:   InvalidSpatialLayer,
		currentTemporalLayer: InvalidTemporalLayer,
		targetTemporalLayer:  InvalidTemporalLayer,

		lastAllocationState: VideoAllocationStateNone,

		rtpMunger: NewRTPMunger(),
	}

	if strings.ToLower(codec.MimeType) == "video/vp8" {
		f.vp8Munger = NewVP8Munger()
	}

	if f.kind == webrtc.RTPCodecTypeVideo {
		f.maxSpatialLayer = 2
		f.maxTemporalLayer = 2
	} else {
		f.maxSpatialLayer = InvalidSpatialLayer
		f.maxTemporalLayer = InvalidTemporalLayer
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

	if spatialLayer == f.maxSpatialLayer {
		return false, VideoLayers{}
	}

	f.maxSpatialLayer = spatialLayer

	return true, VideoLayers{
		spatial:  f.maxSpatialLayer,
		temporal: f.maxTemporalLayer,
	}
}

func (f *Forwarder) CurrentSpatialLayer() int32 {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.currentSpatialLayer
}

func (f *Forwarder) TargetSpatialLayer() int32 {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.targetSpatialLayer
}

func (f *Forwarder) SetMaxTemporalLayer(temporalLayer int32) (bool, VideoLayers) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if temporalLayer == f.maxTemporalLayer {
		return false, VideoLayers{}
	}

	f.maxTemporalLayer = temporalLayer

	return true, VideoLayers{
		spatial:  f.maxSpatialLayer,
		temporal: f.maxTemporalLayer,
	}
}

func (f *Forwarder) MaxLayers() VideoLayers {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return VideoLayers{
		spatial:  f.maxSpatialLayer,
		temporal: f.maxTemporalLayer,
	}
}

func (f *Forwarder) GetForwardingStatus() ForwardingStatus {
	f.lock.RLock()
	defer f.lock.RUnlock()

	if f.targetSpatialLayer == InvalidSpatialLayer {
		return ForwardingStatusOff
	}

	if f.targetSpatialLayer < f.maxSpatialLayer {
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
	f.currentSpatialLayer = InvalidSpatialLayer
	f.targetSpatialLayer = InvalidSpatialLayer

	f.currentTemporalLayer = InvalidTemporalLayer
	f.targetTemporalLayer = InvalidTemporalLayer
}

func (f *Forwarder) getOptimalBandwidthNeeded(brs [3][4]int64) int64 {
	// LK-TODO for temporal preference, traverse the bitrates array the other way
	for i := f.maxSpatialLayer; i >= 0; i-- {
		for j := f.maxTemporalLayer; j >= 0; j-- {
			if brs[i][j] == 0 {
				continue
			}

			return brs[i][j]
		}
	}

	return 0
}

func (f *Forwarder) allocate(availableChannelCapacity int64, canPause bool, brs [3][4]int64) (result VideoAllocationResult) {
	// should never get called on audio tracks, just for safety
	if f.kind == webrtc.RTPCodecTypeAudio {
		return
	}

	if f.muted {
		result.state = VideoAllocationStateMuted
		result.bandwidthRequested = 0
		result.bandwidthDelta = result.bandwidthRequested - f.lastAllocationRequestBps

		f.lastAllocationState = result.state
		f.lastAllocationRequestBps = result.bandwidthRequested
		return
	}

	optimalBandwidthNeeded := f.getOptimalBandwidthNeeded(brs)
	fmt.Printf("SA_DEBUG id: %s, peerId: %s, optimal: %d, availalble layers: %+v, bitrates: %+v, availableChannelCapacity: %d\n", f.id, f.peerId, optimalBandwidthNeeded, f.availableLayers, brs, availableChannelCapacity)	// REMOVE
	if optimalBandwidthNeeded == 0 {
		if len(f.availableLayers) == 0 {
			// feed is dry
			result.state = VideoAllocationStateFeedDry
			result.bandwidthRequested = 0
			result.bandwidthDelta = result.bandwidthRequested - f.lastAllocationRequestBps

			f.lastAllocationState = result.state
			f.lastAllocationRequestBps = result.bandwidthRequested
			return
		}

		// feed bitrate is not yet calculated
		fmt.Printf("SA_DEBUG id: %s, peerId: %s, am2\n", f.id, f.peerId)	// REMOVE
		result.state = VideoAllocationStateAwaitingMeasurement
		f.lastAllocationState = result.state

		if availableChannelCapacity == ChannelCapacityInfinity {
			// channel capacity allows a free pass.
			// So, resume with the highest layer available <= max subscribed layer

/*
			// if already optimistically started, nothing else to do
			if f.targetSpatialLayer != InvalidSpatialLayer {
				return
			}
*/
			if f.targetSpatialLayer == InvalidSpatialLayer {
				result.change = VideoStreamingChangeResuming
			}

			f.targetSpatialLayer = int32(f.availableLayers[len(f.availableLayers)-1])
			if f.targetSpatialLayer > f.maxSpatialLayer {
				f.targetSpatialLayer = f.maxSpatialLayer
			}

			f.targetTemporalLayer = f.maxTemporalLayer
			if f.targetTemporalLayer == InvalidTemporalLayer {
				f.targetTemporalLayer = 0
			}

			fmt.Printf("SA_DEBUG, id: %s, peerId: %s, free allocating %d, %d\n", f.id, f.peerId, f.targetSpatialLayer, f.targetTemporalLayer)	// REMOVE
		} else {
			// if not optimistically started, nothing else to do
			if f.targetSpatialLayer == InvalidSpatialLayer {
				return
			}

			if canPause {
				// disable it as it is not known how big this stream is
				// and if it will fit in the available channel capacity
				result.change = VideoStreamingChangePausing
				result.state = VideoAllocationStateDeficient
				result.bandwidthRequested = 0
				result.bandwidthDelta = result.bandwidthRequested - f.lastAllocationRequestBps

				f.lastAllocationState = result.state
				f.lastAllocationRequestBps = result.bandwidthRequested

				f.disable()
			}
		}
		return
	}

	// LK-TODO for temporal preference, traverse the bitrates array the other way
	for i := f.maxSpatialLayer; i >= 0; i-- {
		for j := f.maxTemporalLayer; j >= 0; j-- {
			if brs[i][j] == 0 {
				continue
			}
			if brs[i][j] < availableChannelCapacity {
				if f.targetSpatialLayer == InvalidSpatialLayer {
					result.change = VideoStreamingChangeResuming
				}
				result.bandwidthRequested = brs[i][j]
				result.bandwidthDelta = result.bandwidthRequested - f.lastAllocationRequestBps
				if result.bandwidthRequested == optimalBandwidthNeeded {
					result.state = VideoAllocationStateOptimal
				} else {
					result.state = VideoAllocationStateDeficient
				}

				f.lastAllocationState = result.state
				f.lastAllocationRequestBps = result.bandwidthRequested

				f.targetSpatialLayer = int32(i)
				f.targetTemporalLayer = int32(j)
				fmt.Printf("SA_DEBUG, id: %s, peerId: %s, allocating %d, %d\n", f.id, f.peerId, f.targetSpatialLayer, f.targetTemporalLayer)	// REMOVE
				return
			}
		}
	}

	if !canPause {
		// do not pause if preserving
		// although preserving, currently streamed layers could have a different bitrate,
		// but not updating to prevent entropy increase.
		result.state = f.lastAllocationState
		return
	}

	// no layer fits in the available channel capacity, disable the track
	if f.targetSpatialLayer != InvalidSpatialLayer {
		result.change = VideoStreamingChangePausing
	}
	result.state = VideoAllocationStateDeficient
	result.bandwidthRequested = 0
	result.bandwidthDelta = result.bandwidthRequested - f.lastAllocationRequestBps

	f.lastAllocationState = result.state
	f.lastAllocationRequestBps = result.bandwidthRequested

	f.disable()
	return
}

func (f *Forwarder) Allocate(availableChannelCapacity int64, brs [3][4]int64) VideoAllocationResult {
	f.lock.Lock()
	defer f.lock.Unlock()

	return f.allocate(availableChannelCapacity, true, brs)
}

func (f *Forwarder) TryAllocate(additionalChannelCapacity int64, brs [3][4]int64) VideoAllocationResult {
	f.lock.Lock()
	defer f.lock.Unlock()

	return f.allocate(f.lastAllocationRequestBps+additionalChannelCapacity, false, brs)
}

func (f *Forwarder) FinalizeAllocate(brs [3][4]int64) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.lastAllocationState != VideoAllocationStateAwaitingMeasurement {
		return
	}

	optimalBandwidthNeeded := f.getOptimalBandwidthNeeded(brs)
	fmt.Printf("SA_DEBUG finalize id: %s, peerId: %s, optimal: %d, availalble layers: %+v, bitrates: %+v\n", f.id, f.peerId, optimalBandwidthNeeded, f.availableLayers, brs)	// REMOVE
	if optimalBandwidthNeeded == 0 {
		if len(f.availableLayers) == 0 {
			// feed dry
			f.lastAllocationState = VideoAllocationStateFeedDry
			f.lastAllocationRequestBps = 0
		}

		// still awaiting measurement
		return
	}

	// LK-TODO for temporal preference, traverse the bitrates array the other way
	for i := f.maxSpatialLayer; i >= 0; i-- {
		for j := f.maxTemporalLayer; j >= 0; j-- {
			if brs[i][j] == 0 {
				continue
			}

			f.lastAllocationState = VideoAllocationStateOptimal
			f.lastAllocationRequestBps = brs[i][j]

			f.targetSpatialLayer = int32(i)
			f.targetTemporalLayer = int32(j)
			fmt.Printf("SA_DEBUG, id: %s, peerId: %s, finalize allocating %d, %d\n", f.id, f.peerId, f.targetSpatialLayer, f.targetTemporalLayer)	// REMOVE
			return
		}
	}
}

func (f *Forwarder) AllocateNextHigher(brs [3][4]int64) bool {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.kind == webrtc.RTPCodecTypeAudio {
		return false
	}

	// if targets are still pending, don't increase
	if f.targetSpatialLayer != InvalidSpatialLayer {
		if f.targetSpatialLayer != f.currentSpatialLayer || f.targetTemporalLayer != f.currentTemporalLayer {
			return false
		}
	}

	optimalBandwidthNeeded := f.getOptimalBandwidthNeeded(brs)
	if optimalBandwidthNeeded == 0 {
		if len(f.availableLayers) == 0 {
			f.lastAllocationState = VideoAllocationStateFeedDry
			f.lastAllocationRequestBps = 0
			return false
		}

		// bitrates not available yet
		fmt.Printf("SA_DEBUG id: %s, peerId: %s, am1\n", f.id, f.peerId)	// REMOVE
		f.lastAllocationState = VideoAllocationStateAwaitingMeasurement
		f.lastAllocationRequestBps = 0
		return false
	}

	// try moving temporal layer up in the current spatial layer
	nextTemporalLayer := f.currentTemporalLayer + 1
	currentSpatialLayer := f.currentSpatialLayer
	if currentSpatialLayer == InvalidSpatialLayer {
		currentSpatialLayer = 0
	}
	if nextTemporalLayer <= f.maxTemporalLayer && brs[currentSpatialLayer][nextTemporalLayer] > 0 {
		f.targetSpatialLayer = currentSpatialLayer
		f.targetTemporalLayer = nextTemporalLayer

		f.lastAllocationRequestBps = brs[currentSpatialLayer][nextTemporalLayer]
		if f.lastAllocationRequestBps < optimalBandwidthNeeded {
			f.lastAllocationState = VideoAllocationStateDeficient
		} else {
			f.lastAllocationState = VideoAllocationStateOptimal
		}
		return true
	}

	// try moving spatial layer up if already at max temporal layer of current spatial layer
	nextSpatialLayer := f.currentSpatialLayer + 1
	if nextSpatialLayer <= f.maxSpatialLayer && brs[nextSpatialLayer][0] > 0 {
		f.targetSpatialLayer = nextSpatialLayer
		f.targetTemporalLayer = 0

		f.lastAllocationRequestBps = brs[nextSpatialLayer][0]
		if f.lastAllocationRequestBps < optimalBandwidthNeeded {
			f.lastAllocationState = VideoAllocationStateDeficient
		} else {
			f.lastAllocationState = VideoAllocationStateOptimal
		}
		return true
	}

	return false
}

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

	if f.targetSpatialLayer == InvalidSpatialLayer {
		// stream is paused by streamallocator
		tp.shouldDrop = true
		return tp, nil
	}

	tp.shouldSendPLI = false
	if f.targetSpatialLayer != f.currentSpatialLayer {
		if f.targetSpatialLayer == layer {
			if extPkt.KeyFrame {
				// lock to target layer
				fmt.Printf("SA_DEBUG id: %s, peerId: %s, locking to target layer, %d -> %d\n", f.id, f.peerId, f.currentSpatialLayer, f.targetSpatialLayer)	// ERMOVE
				f.currentSpatialLayer = f.targetSpatialLayer
			} else {
				tp.shouldSendPLI = true
			}
		}
	}

	if f.currentSpatialLayer != layer {
		tp.shouldDrop = true
		return tp, nil
	}

	if f.targetSpatialLayer < f.currentSpatialLayer && f.targetSpatialLayer < f.maxSpatialLayer {
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

	if f.vp8Munger == nil {
		tp.rtp = tpRTP
		return tp, nil
	}

	tpVP8, err := f.vp8Munger.UpdateAndGet(extPkt, tpRTP.snOrdering, f.currentTemporalLayer)
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

	// catch up temporal layer if necessary
	if tpVP8 != nil && f.currentTemporalLayer != f.targetTemporalLayer {
		if tpVP8.header.TIDPresent == 1 && tpVP8.header.TID <= uint8(f.targetTemporalLayer) {
			f.currentTemporalLayer = f.targetTemporalLayer
		}
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
	if f.targetSpatialLayer == InvalidSpatialLayer {
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

func (f *Forwarder) GetPaddingVP8(frameEndNeeded bool) (*buffer.VP8, error) {
	f.lock.Lock()
	defer f.lock.Unlock()

	return f.vp8Munger.UpdateAndGetPadding(!frameEndNeeded)
}

func (f *Forwarder) GetRTPMungerParams() RTPMungerParams {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.rtpMunger.GetParams()
}

package buffer

import (
	"container/list"

	"github.com/livekit/protocol/logger"
)

var minFramesForCalculation = [DefaultMaxLayerTemporal + 1]int{8, 15, 40}

type frameInfo struct {
	seq       uint16
	ts        uint32
	fn        uint16
	spatial   int32
	temporal  int32
	frameDiff []int
}

type FrameRateCalculator interface {
	RecvPacket(ep *ExtPacket) bool
	GetFrameRate() []float32
	Completed() bool
}

// -----------------------------
// FrameRateCalculator based on PictureID in VP8
type FrameRateCalculatorVP8 struct {
	frameRates   [DefaultMaxLayerTemporal + 1]float32
	clockRate    uint32
	logger       logger.Logger
	firstFrames  [DefaultMaxLayerTemporal + 1]*frameInfo
	secondFrames [DefaultMaxLayerTemporal + 1]*frameInfo
	fnReceived   [64]*frameInfo
	baseFrame    *frameInfo
	completed    bool
}

func NewFrameRateCalculatorVP8(clockRate uint32, logger logger.Logger) *FrameRateCalculatorVP8 {
	return &FrameRateCalculatorVP8{
		clockRate: clockRate,
		logger:    logger,
	}
}

func (f *FrameRateCalculatorVP8) Completed() bool {
	return f.completed
}

func (f *FrameRateCalculatorVP8) RecvPacket(ep *ExtPacket) bool {
	if f.completed {
		return true
	}
	vp8, ok := ep.Payload.(VP8)
	if !ok {
		f.logger.Debugw("no vp8 payload", "sn", ep.Packet.SequenceNumber)
		return false
	}
	fn := vp8.PictureID

	if ep.Temporal >= int32(len(f.frameRates)) {
		f.logger.Warnw("invalid temporal layer", nil, "temporal", ep.Temporal)
		return false
	}

	temporal := ep.Temporal
	if temporal < 0 {
		temporal = 0
	}

	if f.baseFrame == nil {
		f.baseFrame = &frameInfo{seq: ep.Packet.SequenceNumber, ts: ep.Packet.Timestamp, fn: fn}
		f.fnReceived[0] = f.baseFrame
		f.firstFrames[temporal] = f.baseFrame
		return false
	}

	baseDiff := fn - f.baseFrame.fn
	if baseDiff == 0 || baseDiff > 0x4000 {
		return false
	}

	if baseDiff >= uint16(len(f.fnReceived)) {
		// frame number is not continuous, reset
		f.reset()

		return false
	}

	if f.fnReceived[baseDiff] != nil {
		return false
	}

	fi := &frameInfo{
		seq:      ep.Packet.SequenceNumber,
		ts:       ep.Packet.Timestamp,
		fn:       fn,
		temporal: temporal,
	}
	f.fnReceived[baseDiff] = fi

	firstFrame := f.firstFrames[temporal]
	secondFrame := f.secondFrames[temporal]
	if firstFrame == nil {
		f.firstFrames[temporal] = fi
		firstFrame = fi
	} else {
		if (secondFrame == nil || secondFrame.fn < fn) && fn != firstFrame.fn && (fn-firstFrame.fn) < 0x4000 {
			f.secondFrames[temporal] = fi
		}
	}

	return f.calc()
}

func (f *FrameRateCalculatorVP8) calc() bool {
	var rateCounter int
	for currentTemporal := int32(0); currentTemporal <= int32(DefaultMaxLayerTemporal); currentTemporal++ {
		if f.frameRates[currentTemporal] > 0 {
			rateCounter++
			continue
		}

		ff := f.firstFrames[currentTemporal]
		sf := f.secondFrames[currentTemporal]
		// lower temporal layer has been calculated, but higher layer has not received any frames, it should not exist
		if rateCounter > 0 && ff == nil {
			rateCounter++
			continue
		}
		if ff == nil || sf == nil {
			continue
		}

		var frameCount int
		lastTs := ff.ts
		for j := ff.fn - f.baseFrame.fn + 1; j < sf.fn-f.baseFrame.fn+1; j++ {
			if f := f.fnReceived[j]; f == nil {
				break
			} else if f.temporal <= currentTemporal {
				frameCount++
				lastTs = f.ts
			}
		}
		if frameCount >= minFramesForCalculation[currentTemporal] {
			f.frameRates[currentTemporal] = float32(f.clockRate) / float32(lastTs-ff.ts) * float32(frameCount)
			rateCounter++
		}
	}

	if rateCounter == len(f.frameRates) {
		f.completed = true

		// normalize frame rates, Microsoft Edge use 3 temporal layers for vp8 but the middle layer has chance to
		// get a very low frame rate, so we need to normalize the frame rate(use fixed ration 1:2 of highest layer for that layer)
		if f.frameRates[2] > 0 && f.frameRates[2] > f.frameRates[1]*3 {
			f.frameRates[1] = f.frameRates[2] / 2
		}
		f.logger.Debugw("frame rate calculated", "rate", f.frameRates)
		f.reset()
		return true
	}
	return false
}

func (f *FrameRateCalculatorVP8) reset() {
	for i := range f.firstFrames {
		f.firstFrames[i] = nil
		f.secondFrames[i] = nil
	}

	for i := range f.fnReceived {
		f.fnReceived[i] = nil
	}
	f.baseFrame = nil
}

func (f *FrameRateCalculatorVP8) GetFrameRate() []float32 {
	return f.frameRates[:]
}

// -----------------------------
// FrameRateCalculator based on Dependency descriptor

type FrameRateCalculatorDD struct {
	frameRates   [DefaultMaxLayerSpatial + 1][DefaultMaxLayerTemporal + 1]float32
	clockRate    uint32
	logger       logger.Logger
	firstFrames  [DefaultMaxLayerSpatial + 1][DefaultMaxLayerTemporal + 1]*frameInfo
	secondFrames [DefaultMaxLayerSpatial + 1][DefaultMaxLayerTemporal + 1]*frameInfo
	spatial      int
	fnReceived   [256]*frameInfo
	baseFrame    *frameInfo
	completed    bool

	// frames for each decode target
	targetFrames [DefaultMaxLayerSpatial + 1][DefaultMaxLayerTemporal + 1]list.List

	maxSpatial, maxTemporal int32
}

func NewFrameRateCalculatorDD(clockRate uint32, logger logger.Logger) *FrameRateCalculatorDD {
	return &FrameRateCalculatorDD{
		clockRate:   clockRate,
		logger:      logger,
		maxSpatial:  DefaultMaxLayerSpatial,
		maxTemporal: DefaultMaxLayerTemporal,
	}
}

func (f *FrameRateCalculatorDD) Completed() bool {
	return f.completed
}

func (f *FrameRateCalculatorDD) SetMaxLayer(spatial, temporal int32) {
	f.maxSpatial, f.maxTemporal = spatial, temporal
}

func (f *FrameRateCalculatorDD) RecvPacket(ep *ExtPacket) bool {
	if f.completed {
		return true
	}

	if ep.DependencyDescriptor == nil {
		f.logger.Infow("dependency descriptor is nil", nil)
		return false
	}

	spatial := ep.Spatial
	// non-SVC codec will set spatial to -1
	if spatial < 0 {
		spatial = 0
	}
	temporal := ep.Temporal
	if temporal < 0 || temporal > DefaultMaxLayerTemporal || spatial > DefaultMaxLayerSpatial {
		f.logger.Warnw("invalid spatial or temporal", nil, "spatial", spatial, "temporal", temporal, "sn", ep.Packet.SequenceNumber)
		return false
	}

	fn := ep.DependencyDescriptor.FrameNumber
	if f.baseFrame == nil {
		f.baseFrame = &frameInfo{seq: ep.Packet.SequenceNumber, ts: ep.Packet.Timestamp, fn: fn}
		f.fnReceived[0] = f.baseFrame
		f.firstFrames[spatial][temporal] = f.baseFrame
		f.secondFrames[spatial][temporal] = f.baseFrame
		return false
	}

	baseDiff := fn - f.baseFrame.fn
	if baseDiff == 0 || baseDiff > 0x8000 {
		return false
	}

	if baseDiff >= uint16(len(f.fnReceived)) {
		// frame number is not continuous, reset
		f.baseFrame = nil
		for i := range f.firstFrames {
			for j := range f.firstFrames[i] {
				f.firstFrames[i][j] = nil
				f.secondFrames[i][j] = nil
				f.targetFrames[i][j].Init()
			}
		}
		for i := range f.fnReceived {
			f.fnReceived[i] = nil
		}
		return false
	}

	if f.fnReceived[baseDiff] != nil {
		return false
	}

	fi := &frameInfo{
		seq:       ep.Packet.SequenceNumber,
		ts:        ep.Packet.Timestamp,
		fn:        fn,
		temporal:  temporal,
		spatial:   spatial,
		frameDiff: ep.DependencyDescriptor.FrameDependencies.FrameDiffs,
	}
	f.fnReceived[baseDiff] = fi

	if f.firstFrames[spatial][temporal] == nil {
		f.firstFrames[spatial][temporal] = fi
		f.secondFrames[spatial][temporal] = fi
		return false
	}

	chain := &f.targetFrames[spatial][temporal]
	if chain.Len() == 0 {
		chain.PushBack(fn)
	}
	for _, fdiff := range ep.DependencyDescriptor.FrameDependencies.FrameDiffs {
		dependFrame := fn - uint16(fdiff)
		// frame too old, ignore
		if dependFrame-f.secondFrames[spatial][temporal].fn > 0x8000 {
			continue
		}

	insertFrame:
		for e := chain.Back(); e != nil; e = e.Prev() {
			val := e.Value.(uint16)
			switch {
			case val == dependFrame:
				break insertFrame
			case val < dependFrame:
				chain.InsertAfter(dependFrame, e)
				break insertFrame
			default:
				if e == chain.Front() {
					chain.PushFront(dependFrame)
				}
			}
		}
	}
	return f.calc()
}

func (f *FrameRateCalculatorDD) calc() bool {
	var rateCounter int
	for currentSpatial := int32(0); currentSpatial <= f.maxSpatial; currentSpatial++ {
		var currentSpatialRateCounter int
		for currentTemporal := int32(0); currentTemporal <= f.maxTemporal; currentTemporal++ {
			if f.frameRates[currentSpatial][currentTemporal] > 0 {
				rateCounter++
				currentSpatialRateCounter++
				continue
			}

			firstFrame := f.firstFrames[currentSpatial][currentTemporal]
			// lower temporal layer has been calculated, but higher layer has not received any frames, it should not exist
			if currentSpatialRateCounter > 0 && firstFrame == nil {
				currentSpatialRateCounter++
				rateCounter++
				continue
			}

			chain := &f.targetFrames[currentSpatial][currentTemporal]

			// find last decodable frame (no dependency frame is lost)
			var lastFrame *frameInfo
			for e := chain.Front(); e != nil; e = e.Next() {
				diff := e.Value.(uint16) - f.baseFrame.fn
				if diff >= uint16(len(f.fnReceived)) {
					continue
				}

				fi := f.fnReceived[diff]
				if fi == nil {
					break
				} else {
					lastFrame = fi
					if firstFrame == nil && fi.spatial == currentSpatial && fi.temporal == currentTemporal {
						firstFrame = fi
					}
				}
			}

			if lastFrame != nil && lastFrame.fn > f.secondFrames[currentSpatial][currentTemporal].fn {
				f.secondFrames[currentSpatial][currentTemporal] = lastFrame
			} else {
				continue
			}

			frameCount := 0
			for i := firstFrame.fn - f.baseFrame.fn; i <= lastFrame.fn-f.baseFrame.fn; i++ {
				fi := f.fnReceived[i]
				if fi == nil {
					continue
				}
				if fi.spatial == currentSpatial && fi.temporal <= currentTemporal {
					frameCount++
				}
			}

			if frameCount >= minFramesForCalculation[currentTemporal] && lastFrame.ts > firstFrame.ts {
				f.frameRates[currentSpatial][currentTemporal] = float32(f.clockRate) / float32(lastFrame.ts-firstFrame.ts) * float32(frameCount)
				rateCounter++
			}
		}
	}

	if rateCounter == int(f.maxSpatial+1)*int(f.maxTemporal+1) {
		f.completed = true
		f.close()

		f.logger.Debugw("frame rate calculated", "spatial", f.spatial, "rate", f.frameRates)
		return true
	}
	return false
}

func (f *FrameRateCalculatorDD) GetFrameRateForSpatial(spatial int32) []float32 {
	if spatial < 0 || spatial >= int32(len(f.frameRates)) {
		return nil
	}
	return f.frameRates[spatial][:]
}

func (f *FrameRateCalculatorDD) close() {
	f.baseFrame = nil
	for i := range f.firstFrames {
		for j := range f.firstFrames[i] {
			f.firstFrames[i][j] = nil
			f.secondFrames[i][j] = nil
		}
	}

	for i := range f.fnReceived {
		f.fnReceived[i] = nil
	}
	for i := range f.targetFrames {
		for j := range f.targetFrames[i] {
			f.targetFrames[i][j].Init()
		}
	}
}

func (f *FrameRateCalculatorDD) GetFrameRateCalculatorForSpatial(spatial int32) *FrameRateCalculatorForDDLayer {
	return &FrameRateCalculatorForDDLayer{
		FrameRateCalculatorDD: f,
		spatial:               spatial,
	}
}

type FrameRateCalculatorForDDLayer struct {
	*FrameRateCalculatorDD
	spatial int32
}

func (f *FrameRateCalculatorForDDLayer) GetFrameRate() []float32 {
	return f.FrameRateCalculatorDD.GetFrameRateForSpatial(f.spatial)
}

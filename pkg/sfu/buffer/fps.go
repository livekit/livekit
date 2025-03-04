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
	"container/list"

	"github.com/pion/rtp/codecs"

	"github.com/livekit/protocol/logger"
)

var minFramesForCalculation = [...]int{8, 15, 40, 60}

type frameInfo struct {
	startSeq  uint16
	endSeq    uint16
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

// FrameRateCalculator based on PictureID in VPx
type frameRateCalculatorVPx struct {
	frameRates   [DefaultMaxLayerTemporal + 1]float32
	clockRate    uint32
	logger       logger.Logger
	firstFrames  [DefaultMaxLayerTemporal + 1]*frameInfo
	secondFrames [DefaultMaxLayerTemporal + 1]*frameInfo
	fnReceived   [64]*frameInfo
	baseFrame    *frameInfo
	completed    bool
}

func newFrameRateCalculatorVPx(clockRate uint32, logger logger.Logger) *frameRateCalculatorVPx {
	return &frameRateCalculatorVPx{
		clockRate: clockRate,
		logger:    logger,
	}
}

func (f *frameRateCalculatorVPx) Completed() bool {
	return f.completed
}

func (f *frameRateCalculatorVPx) RecvPacket(ep *ExtPacket, fn uint16) bool {
	if f.completed {
		return true
	}

	if ep.Temporal >= int32(len(f.frameRates)) {
		f.logger.Warnw("invalid temporal layer", nil, "temporal", ep.Temporal)
		return false
	}

	temporal := ep.Temporal
	if temporal < 0 {
		temporal = 0
	}

	if f.baseFrame == nil {
		f.baseFrame = &frameInfo{ts: ep.Packet.Timestamp, fn: fn}
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

func (f *frameRateCalculatorVPx) calc() bool {
	var rateCounter int
	for currentTemporal := int32(0); currentTemporal <= DefaultMaxLayerTemporal; currentTemporal++ {
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
		f.reset()
		return true
	}
	return false
}

func (f *frameRateCalculatorVPx) reset() {
	for i := range f.firstFrames {
		f.firstFrames[i] = nil
		f.secondFrames[i] = nil
	}

	for i := range f.fnReceived {
		f.fnReceived[i] = nil
	}
	f.baseFrame = nil
}

func (f *frameRateCalculatorVPx) GetFrameRate() []float32 {
	return f.frameRates[:]
}

// -----------------------------

// FrameRateCalculator based on PictureID in VP8
type FrameRateCalculatorVP8 struct {
	*frameRateCalculatorVPx
	logger logger.Logger
}

func NewFrameRateCalculatorVP8(clockRate uint32, logger logger.Logger) *FrameRateCalculatorVP8 {
	return &FrameRateCalculatorVP8{
		frameRateCalculatorVPx: newFrameRateCalculatorVPx(clockRate, logger),
		logger:                 logger,
	}
}

func (f *FrameRateCalculatorVP8) RecvPacket(ep *ExtPacket) bool {
	if f.frameRateCalculatorVPx.Completed() {
		return true
	}

	vp8, ok := ep.Payload.(VP8)
	if !ok {
		f.logger.Debugw("no vp8 payload", "sn", ep.Packet.SequenceNumber)
		return false
	}
	success := f.frameRateCalculatorVPx.RecvPacket(ep, vp8.PictureID)

	if f.frameRateCalculatorVPx.Completed() {
		f.logger.Debugw("frame rate calculated", "rate", f.frameRateCalculatorVPx.GetFrameRate())
	}

	return success
}

// -----------------------------

// FrameRateCalculator based on PictureID in VP9
type FrameRateCalculatorVP9 struct {
	logger    logger.Logger
	completed bool

	// VP9-TODO - this is assuming three spatial layers. As `completed` marker relies on all layers being finished, have to assume this. FIX.
	//            Maybe look at number of layers in livekit.TrackInfo and declare completed once advertised layers are measured
	frameRateCalculatorsVPx [DefaultMaxLayerSpatial + 1]*frameRateCalculatorVPx
}

func NewFrameRateCalculatorVP9(clockRate uint32, logger logger.Logger) *FrameRateCalculatorVP9 {
	f := &FrameRateCalculatorVP9{
		logger: logger,
	}

	for i := range f.frameRateCalculatorsVPx {
		f.frameRateCalculatorsVPx[i] = newFrameRateCalculatorVPx(clockRate, logger)
	}

	return f
}

func (f *FrameRateCalculatorVP9) Completed() bool {
	return f.completed
}

func (f *FrameRateCalculatorVP9) RecvPacket(ep *ExtPacket) bool {
	if f.completed {
		return true
	}

	vp9, ok := ep.Payload.(codecs.VP9Packet)
	if !ok {
		f.logger.Debugw("no vp9 payload", "sn", ep.Packet.SequenceNumber)
		return false
	}

	if ep.Spatial < 0 || ep.Spatial >= int32(len(f.frameRateCalculatorsVPx)) || f.frameRateCalculatorsVPx[ep.Spatial] == nil {
		f.logger.Debugw("invalid spatial layer", "sn", ep.Packet.SequenceNumber, "spatial", ep.Spatial)
		return false
	}

	success := f.frameRateCalculatorsVPx[ep.Spatial].RecvPacket(ep, vp9.PictureID)

	completed := true
	for _, frc := range f.frameRateCalculatorsVPx {
		if !frc.Completed() {
			completed = false
			break
		}
	}

	if completed {
		f.completed = true

		var frameRates [DefaultMaxLayerSpatial + 1][]float32
		for i := range f.frameRateCalculatorsVPx {
			frameRates[i] = f.frameRateCalculatorsVPx[i].GetFrameRate()
		}
		f.logger.Debugw("frame rate calculated", "rate", frameRates)
	}

	return success
}

func (f *FrameRateCalculatorVP9) GetFrameRateForSpatial(spatial int32) []float32 {
	if spatial < 0 || spatial >= int32(len(f.frameRateCalculatorsVPx)) || f.frameRateCalculatorsVPx[spatial] == nil {
		return nil
	}
	return f.frameRateCalculatorsVPx[spatial].GetFrameRate()
}

func (f *FrameRateCalculatorVP9) GetFrameRateCalculatorForSpatial(spatial int32) *FrameRateCalculatorForVP9Layer {
	return &FrameRateCalculatorForVP9Layer{
		FrameRateCalculatorVP9: f,
		spatial:                spatial,
	}
}

// -----------------------------

type FrameRateCalculatorForVP9Layer struct {
	*FrameRateCalculatorVP9
	spatial int32
}

func (f *FrameRateCalculatorForVP9Layer) GetFrameRate() []float32 {
	return f.FrameRateCalculatorVP9.GetFrameRateForSpatial(f.spatial)
}

// -----------------------------------------------

// FrameRateCalculator based on Dependency descriptor
type FrameRateCalculatorDD struct {
	frameRates   [DefaultMaxLayerSpatial + 1][DefaultMaxLayerTemporal + 1]float32
	clockRate    uint32
	logger       logger.Logger
	firstFrames  [DefaultMaxLayerSpatial + 1][DefaultMaxLayerTemporal + 1]*frameInfo
	secondFrames [DefaultMaxLayerSpatial + 1][DefaultMaxLayerTemporal + 1]*frameInfo
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
		f.logger.Debugw("dependency descriptor is nil")
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

	fn := ep.DependencyDescriptor.Descriptor.FrameNumber
	if f.baseFrame == nil {
		f.baseFrame = &frameInfo{ts: ep.Packet.Timestamp, fn: fn}
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
		ts:        ep.Packet.Timestamp,
		fn:        fn,
		temporal:  temporal,
		spatial:   spatial,
		frameDiff: ep.DependencyDescriptor.Descriptor.FrameDependencies.FrameDiffs,
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
	for _, fdiff := range ep.DependencyDescriptor.Descriptor.FrameDependencies.FrameDiffs {
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
			case sn16LT(val, dependFrame):
				chain.InsertAfter(dependFrame, e)
				break insertFrame
			default:
				if e == chain.Front() {
					chain.PushFront(dependFrame)
					break insertFrame
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

		f.logger.Debugw("frame rate calculated", "rate", f.frameRates)
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

// -----------------------------------------------

type FrameRateCalculatorForDDLayer struct {
	*FrameRateCalculatorDD
	spatial int32
}

func (f *FrameRateCalculatorForDDLayer) GetFrameRate() []float32 {
	return f.FrameRateCalculatorDD.GetFrameRateForSpatial(f.spatial)
}

// -----------------------------------------------

type FrameRateCalculatorH26x struct {
	frameRates [DefaultMaxLayerTemporal + 1]float32
	clockRate  uint32
	logger     logger.Logger
	fnReceived *list.List
	baseFrame  *frameInfo
	completed  bool
}

func NewFrameRateCalculatorH26x(clockRate uint32, logger logger.Logger) *FrameRateCalculatorH26x {
	return &FrameRateCalculatorH26x{
		clockRate: clockRate,
		logger:    logger,
	}
}

func (f *FrameRateCalculatorH26x) Completed() bool {
	return f.completed
}

func (f *FrameRateCalculatorH26x) RecvPacket(ep *ExtPacket) bool {
	if f.completed {
		return true
	}

	if ep.Temporal >= int32(len(f.frameRates)) {
		f.logger.Warnw("invalid temporal layer", nil, "temporal", ep.Temporal)
		return false
	}

	temporal := ep.Temporal
	if temporal < 0 {
		temporal = 0
	}

	if f.baseFrame == nil {
		f.baseFrame = &frameInfo{
			startSeq: ep.Packet.SequenceNumber,
			endSeq:   ep.Packet.SequenceNumber,
			ts:       ep.Packet.Timestamp,
			temporal: temporal,
		}
		f.fnReceived = list.New()
		f.fnReceived.PushBack(f.baseFrame)
		return false
	}

	if sn16LTOrEqual(ep.Packet.SequenceNumber, f.baseFrame.startSeq) {
		return false
	}

insertFrame:
	for e := f.fnReceived.Back(); e != nil; e = e.Prev() {
		frame := e.Value.(*frameInfo)
		switch {
		case frame.ts == ep.Packet.Timestamp:
			if sn16LT(frame.endSeq, ep.Packet.SequenceNumber) {
				frame.endSeq = ep.Packet.SequenceNumber
			}
			if sn16LT(ep.Packet.SequenceNumber, frame.startSeq) {
				frame.startSeq = ep.Packet.SequenceNumber
			}
			break insertFrame
		case sn32LT(frame.ts, ep.Packet.Timestamp):
			f.fnReceived.InsertAfter(&frameInfo{
				startSeq: ep.Packet.SequenceNumber,
				endSeq:   ep.Packet.SequenceNumber,
				ts:       ep.Packet.Timestamp,
				temporal: temporal,
			}, e)
			break insertFrame
		default:
			if e == f.fnReceived.Front() {
				f.fnReceived.PushFront(&frameInfo{
					startSeq: ep.Packet.SequenceNumber,
					endSeq:   ep.Packet.SequenceNumber,
					ts:       ep.Packet.Timestamp,
					temporal: temporal,
				})
				break insertFrame
			}
		}
	}

	return f.calc()
}

func (f *FrameRateCalculatorH26x) calc() bool {
	frameCounts := make([]int, DefaultMaxLayerTemporal+1)
	var totalFrameCount int
	var tsDuration int
	cur := f.fnReceived.Front()
	for {
		next := cur.Next()
		if next == nil {
			break
		}
		ff := cur.Value.(*frameInfo)
		nf := next.Value.(*frameInfo)
		if nf.startSeq-ff.endSeq == 1 {
			totalFrameCount++
			tsDuration += int(nf.ts - ff.ts)
			for i := int(nf.temporal); i < len(frameCounts); i++ {
				frameCounts[i]++
			}
		} else {
			// reset to find continuous frames
			totalFrameCount = 0
			for i := range frameCounts {
				frameCounts[i] = 0
			}
			tsDuration = 0
		}

		// received enough continuous frames, calculate fps
		if totalFrameCount >= minFramesForCalculation[DefaultMaxLayerTemporal] {
			for currentTemporal := int32(0); currentTemporal <= DefaultMaxLayerTemporal; currentTemporal++ {
				count := frameCounts[currentTemporal]
				if currentTemporal > 0 && count == frameCounts[currentTemporal-1] {
					// no frames for this temporal layer
					f.frameRates[currentTemporal] = 0
				} else {
					f.frameRates[currentTemporal] = float32(f.clockRate) / float32(tsDuration) * float32(count)
				}
			}
			f.logger.Debugw("fps changed", "fps", f.GetFrameRate())
			f.completed = true
			f.reset()
			return true
		}

		cur = next
	}

	return false
}

func (f *FrameRateCalculatorH26x) reset() {
	f.fnReceived.Init()
	f.baseFrame = nil
}

func (f *FrameRateCalculatorH26x) GetFrameRate() []float32 {
	return f.frameRates[:]
}

// -----------------------------------------------
func sn16LT(a, b uint16) bool {
	return a-b > 0x8000
}

func sn16LTOrEqual(a, b uint16) bool {
	return a == b || a-b > 0x8000
}

func sn32LT(a, b uint32) bool {
	return a-b > 0x80000000
}

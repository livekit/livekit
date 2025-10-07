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

package utils

import (
	"unsafe"

	"go.uber.org/zap/zapcore"
)

type number interface {
	uint16 | uint32
}

type extendedNumber interface {
	uint32 | uint64
}

type WrapAroundParams struct {
	IsRestartAllowed bool
}

type WrapAround[T number, ET extendedNumber] struct {
	params    WrapAroundParams
	fullRange ET

	initialized     bool
	start           T
	highest         T
	cycles          ET
	extendedHighest ET
}

func NewWrapAround[T number, ET extendedNumber](params WrapAroundParams) *WrapAround[T, ET] {
	var t T
	return &WrapAround[T, ET]{
		params:    params,
		fullRange: 1 << (unsafe.Sizeof(t) * 8),
	}
}

func (w *WrapAround[T, ET]) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if w == nil {
		return nil
	}

	e.AddUint64("fullRange", uint64(w.fullRange))
	e.AddBool("initialized", w.initialized)
	e.AddUint64("start", uint64(w.start))
	e.AddUint64("highest", uint64(w.highest))
	e.AddUint64("cycles", uint64(w.cycles))
	e.AddUint64("extendedHighest", uint64(w.extendedHighest))
	return nil
}

func (w *WrapAround[T, ET]) Seed(from *WrapAround[T, ET]) {
	w.initialized = from.initialized
	w.start = from.start
	w.highest = from.highest
	w.cycles = from.cycles
	w.updateExtendedHighest()
}

type WrapAroundUpdateResult[ET extendedNumber] struct {
	IsUnhandled        bool // when set, other fields are invalid
	IsRestart          bool
	PreExtendedStart   ET // valid only if IsRestart = true
	PreExtendedHighest ET
	ExtendedVal        ET
}

func (w *WrapAroundUpdateResult[ET]) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if w == nil {
		return nil
	}

	e.AddBool("IsUnhandled", w.IsUnhandled)
	e.AddBool("IsRestart", w.IsRestart)
	e.AddUint64("PreExtendedStart", uint64(w.PreExtendedStart))
	e.AddUint64("PreExtendedHighest", uint64(w.PreExtendedHighest))
	e.AddUint64("ExtendedVal", uint64(w.ExtendedVal))
	return nil
}

func (w *WrapAround[T, ET]) UpdateWithOrderKnown(val T, orderKnown bool) (result WrapAroundUpdateResult[ET]) {
	if !w.initialized {
		result.PreExtendedHighest = ET(val) - 1
		result.ExtendedVal = ET(val)

		w.start = val
		w.highest = val
		w.updateExtendedHighest()
		w.initialized = true
		return
	}

	if !orderKnown {
		gap := val - w.highest
		if gap > T(w.fullRange>>1) {
			// out-of-order
			return w.maybeAdjustStart(val)
		}
	}

	// in-order
	result.PreExtendedHighest = w.extendedHighest

	if val < w.highest {
		w.cycles += w.fullRange
	}
	w.highest = val

	w.updateExtendedHighest()
	result.ExtendedVal = w.extendedHighest
	return
}

func (w *WrapAround[T, ET]) Update(val T) (result WrapAroundUpdateResult[ET]) {
	return w.UpdateWithOrderKnown(val, false)
}

func (w *WrapAround[T, ET]) UndoUpdate(result WrapAroundUpdateResult[ET]) {
	if !w.initialized || result.PreExtendedHighest >= result.ExtendedVal {
		return
	}

	w.ResetHighest(result.PreExtendedHighest)
}

func (w *WrapAround[T, ET]) Rollover(val T, numCycles int) (result WrapAroundUpdateResult[ET]) {
	if numCycles < 0 || !w.initialized {
		return w.Update(val)
	}

	result.PreExtendedHighest = w.extendedHighest

	w.cycles += ET(numCycles) * w.fullRange
	w.highest = val

	w.updateExtendedHighest()
	result.ExtendedVal = w.extendedHighest
	return
}

func (w *WrapAround[T, ET]) RollbackRestart(ev ET) {
	if w.isWrapBack(w.start, T(ev)) {
		w.cycles -= w.fullRange
		w.updateExtendedHighest()
	}
	w.start = T(ev)
}

func (w *WrapAround[T, ET]) ResetHighest(ev ET) {
	w.highest = T(ev)
	w.cycles = ev & ^(w.fullRange - 1)
	w.updateExtendedHighest()
}

func (w *WrapAround[T, ET]) GetStart() T {
	return w.start
}

func (w *WrapAround[T, ET]) GetExtendedStart() ET {
	return ET(w.start)
}

func (w *WrapAround[T, ET]) GetHighest() T {
	return w.highest
}

func (w *WrapAround[T, ET]) GetExtendedHighest() ET {
	return w.extendedHighest
}

func (w *WrapAround[T, ET]) updateExtendedHighest() {
	w.extendedHighest = getExtended(w.cycles, w.highest)
}

func (w *WrapAround[T, ET]) maybeAdjustStart(val T) (result WrapAroundUpdateResult[ET]) {
	// re-adjust start if necessary. The conditions are
	// 1. Not seen more than half the range yet
	// 1. wrap back compared to start and not completed a half cycle, sequences like (10, 65530) in uint16 space
	// 2. no wrap around, but out-of-order compared to start and not completed a half cycle , sequences like (10, 9), (65530, 65528) in uint16 space
	cycles := w.cycles
	totalNum := w.GetExtendedHighest() - w.GetExtendedStart() + 1
	if totalNum > (w.fullRange >> 1) {
		if w.isWrapBack(val, w.highest) {
			cycles -= w.fullRange
		}
		result.PreExtendedHighest = w.extendedHighest
		result.ExtendedVal = getExtended(cycles, val)
		return
	}

	if val-w.start > T(w.fullRange>>1) {
		if w.params.IsRestartAllowed {
			// out-of-order with existing start => a new start
			result.IsRestart = true
			if val > w.start {
				result.PreExtendedStart = w.fullRange + ET(w.start)
			} else {
				result.PreExtendedStart = ET(w.start)
			}

			if w.isWrapBack(val, w.highest) {
				w.cycles = w.fullRange
				w.updateExtendedHighest()
				cycles = 0
			}
			w.start = val
		} else {
			result.IsUnhandled = true
		}
	} else {
		if w.isWrapBack(val, w.highest) {
			cycles -= w.fullRange
		}
	}
	result.PreExtendedHighest = w.extendedHighest
	result.ExtendedVal = getExtended(cycles, val)
	return
}

func (w *WrapAround[T, ET]) isWrapBack(earlier T, later T) bool {
	return ET(later) < (w.fullRange>>1) && ET(earlier) >= (w.fullRange>>1)
}

// ------------------------------------

func getExtended[T number, ET extendedNumber](cycles ET, val T) ET {
	return cycles + ET(val)
}

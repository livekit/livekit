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
	"errors"
	"math"
	"unsafe"
)

const (
	minRanges = 1
)

var (
	errReversedOrder = errors.New("end is before start")
	errKeyNotFound   = errors.New("key not found")
)

type rangeType interface {
	uint32 | uint64
}

type valueType interface {
	uint32 | uint64
}

type rangeVal[RT rangeType, VT valueType] struct {
	start RT
	end   RT
	value VT
}

type RangeMap[RT rangeType, VT valueType] struct {
	halfRange RT

	size         int
	ranges       []rangeVal[RT, VT]
	runningValue VT
}

func NewRangeMap[RT rangeType, VT valueType](size int) *RangeMap[RT, VT] {
	var t RT
	return &RangeMap[RT, VT]{
		halfRange: 1 << ((unsafe.Sizeof(t) * 8) - 1),
		size:      int(math.Max(float64(size), float64(minRanges))),
	}
}

func (r *RangeMap[RT, VT]) ClearAndResetValue(val VT) {
	r.ranges = r.ranges[:0]
	r.runningValue = val
}

func (r *RangeMap[RT, VT]) CloseRangeAndIncValue(endExclusive RT, inc VT) error {
	if err := r.closeRange(endExclusive); err != nil {
		return err
	}
	r.runningValue += inc
	return nil
}

func (r *RangeMap[RT, VT]) CloseRangeAndDecValue(endExclusive RT, dec VT) error {
	if err := r.closeRange(endExclusive); err != nil {
		return err
	}
	r.runningValue -= dec
	return nil
}

func (r *RangeMap[RT, VT]) closeRange(endExclusive RT) error {
	numRanges := len(r.ranges)
	startInclusive := RT(0)
	if numRanges != 0 {
		startInclusive = r.ranges[numRanges-1].end + 1

		if endExclusive == r.ranges[numRanges-1].end+1 {
			// key already in map and corresponding range recorded
			return nil
		}
	}
	if endExclusive == startInclusive || endExclusive-startInclusive > r.halfRange {
		return errReversedOrder
	}

	r.ranges = append(r.ranges, rangeVal[RT, VT]{
		start: startInclusive,
		end:   endExclusive - 1,
		value: r.runningValue,
	})

	r.prune()
	return nil
}

func (r *RangeMap[RT, VT]) GetValue(key RT) (VT, error) {
	numRanges := len(r.ranges)
	if numRanges != 0 {
		if key > r.ranges[numRanges-1].end {
			return r.runningValue, nil
		}

		if key < r.ranges[0].start {
			return 0, errKeyNotFound
		}
	}

	for _, rv := range r.ranges {
		if key-rv.start < r.halfRange && rv.end-key < r.halfRange {
			return rv.value, nil
		}
	}

	return r.runningValue, nil
}

func (r *RangeMap[RT, VT]) prune() {
	if len(r.ranges) > r.size {
		r.ranges = r.ranges[len(r.ranges)-r.size:]
	}
}

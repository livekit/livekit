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
)

type rangeType interface {
	uint32
}

type valueType interface {
	uint32
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
		halfRange: 1 << (unsafe.Sizeof(t) * 8) >> 1,
		size:      int(math.Max(float64(size), float64(minRanges))),
	}
}

func (r *RangeMap[RT, VT]) IncValue(inc VT) {
	r.runningValue += inc
}

func (r *RangeMap[RT, VT]) AddRange(startInclusive RT, endExclusive RT) error {
	if endExclusive-startInclusive > r.halfRange {
		return errReversedOrder
	}

	isNewRange := true
	// check if last range can be extended
	if len(r.ranges) != 0 {
		lr := &r.ranges[len(r.ranges)-1]
		if lr.value == r.runningValue {
			lr.end = endExclusive - 1
			isNewRange = false
		} else {
			// end last range before start and start a new range
			lr.end = startInclusive - 1
		}
	}

	if isNewRange {
		r.ranges = append(r.ranges, rangeVal[RT, VT]{
			start: startInclusive,
			end:   endExclusive - 1,
			value: r.runningValue,
		})
	}
	r.prune()
	return nil
}

func (r *RangeMap[RT, VT]) GetValue(key RT) VT {
	for _, rv := range r.ranges {
		if key-rv.start < r.halfRange && rv.end-key < r.halfRange {
			return rv.value
		}
	}

	return r.runningValue
}

func (r *RangeMap[RT, VT]) prune() {
	if len(r.ranges) > r.size {
		r.ranges = r.ranges[len(r.ranges)-r.size:]
	}
}

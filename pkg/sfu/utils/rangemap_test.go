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
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRangeMapUint32(t *testing.T) {
	r := NewRangeMap[uint32, uint32](2)

	// getting value for any key should be 0 default
	value, err := r.GetValue(33333)
	require.NoError(t, err)
	require.Equal(t, uint32(0), value)

	expectedRanges := []rangeVal[uint32, uint32]{
		{
			start: 0,
			end:   0,
			value: 0,
		},
	}
	require.Equal(t, expectedRanges, r.ranges)

	// add an exclusion, should create a new range
	err = r.ExcludeRange(10, 11)
	require.NoError(t, err)

	expectedRanges = []rangeVal[uint32, uint32]{
		{
			start: 0,
			end:   9,
			value: 0,
		},
		{
			start: 11,
			end:   0,
			value: 1,
		},
	}
	require.Equal(t, expectedRanges, r.ranges)

	// getting value in old range should return 0
	value, err = r.GetValue(6)
	require.NoError(t, err)
	require.Equal(t, uint32(0), value)

	// newer should return 1
	value, err = r.GetValue(11)
	require.NoError(t, err)
	require.Equal(t, uint32(1), value)

	// excluded range should return error
	value, err = r.GetValue(10)
	require.ErrorIs(t, err, errKeyExcluded)

	// out-of-order exclusion should return error
	err = r.ExcludeRange(9, 10)
	require.ErrorIs(t, err, errReversedOrder)

	// flipped exclusion should return error
	err = r.ExcludeRange(12, 11)
	require.ErrorIs(t, err, errReversedOrder)
	err = r.ExcludeRange(11, 11)
	require.ErrorIs(t, err, errReversedOrder)

	// add adjacent exclusion range of length = 1
	err = r.ExcludeRange(11, 12)
	require.NoError(t, err)

	expectedRanges = []rangeVal[uint32, uint32]{
		{
			start: 0,
			end:   9,
			value: 0,
		},
		{
			start: 12,
			end:   0,
			value: 2,
		},
	}
	require.Equal(t, expectedRanges, r.ranges)

	// excluded range should return error, now is excluded because exclusion range could be extended
	value, err = r.GetValue(11)
	require.ErrorIs(t, err, errKeyExcluded)

	// getting value in old range should return 0
	value, err = r.GetValue(6)
	require.NoError(t, err)

	// newer should return 2
	value, err = r.GetValue(12)
	require.NoError(t, err)
	require.Equal(t, uint32(2), value)

	// add adjacent exclusion range of length = 10
	err = r.ExcludeRange(12, 22)
	require.NoError(t, err)

	expectedRanges = []rangeVal[uint32, uint32]{
		{
			start: 0,
			end:   9,
			value: 0,
		},
		{
			start: 22,
			end:   0,
			value: 12,
		},
	}
	require.Equal(t, expectedRanges, r.ranges)

	// excluded range should return error, now is excluded because exclusion range could be extended
	value, err = r.GetValue(15)
	require.ErrorIs(t, err, errKeyExcluded)

	// newer should return 12
	value, err = r.GetValue(25)
	require.NoError(t, err)
	require.Equal(t, uint32(12), value)

	// add a disjoint exclusion of length = 4
	err = r.ExcludeRange(26, 30)
	require.NoError(t, err)

	expectedRanges = []rangeVal[uint32, uint32]{
		{
			start: 0,
			end:   9,
			value: 0,
		},
		{
			start: 22,
			end:   25,
			value: 12,
		},
		{
			start: 30,
			end:   0,
			value: 16,
		},
	}
	require.Equal(t, expectedRanges, r.ranges)

	// get a value from newly closed range [22, 25]
	value, err = r.GetValue(23)
	require.NoError(t, err)
	require.Equal(t, uint32(12), value)

	// add a disjoint exclusion of length = 1
	err = r.ExcludeRange(50, 51)
	require.NoError(t, err)

	// previously first range would have been pruned due to size limitations
	expectedRanges = []rangeVal[uint32, uint32]{
		{
			start: 22,
			end:   25,
			value: 12,
		},
		{
			start: 30,
			end:   49,
			value: 16,
		},
		{
			start: 51,
			end:   0,
			value: 17,
		},
	}
	require.Equal(t, expectedRanges, r.ranges)

	// excluded range should return error
	value, err = r.GetValue(50)
	require.ErrorIs(t, err, errKeyExcluded)
	value, err = r.GetValue(28)
	require.ErrorIs(t, err, errKeyExcluded)
	value, err = r.GetValue(17)
	require.ErrorIs(t, err, errKeyTooOld)

	// previously valid, but aged out key should return error
	value, err = r.GetValue(5)
	require.ErrorIs(t, err, errKeyTooOld)

	// valid range access should return values
	value, err = r.GetValue(24)
	require.NoError(t, err)
	require.Equal(t, uint32(12), value)

	value, err = r.GetValue(34)
	require.NoError(t, err)
	require.Equal(t, uint32(16), value)

	value, err = r.GetValue(49)
	require.NoError(t, err)
	require.Equal(t, uint32(16), value)

	value, err = r.GetValue(55555555)
	require.NoError(t, err)
	require.Equal(t, uint32(17), value)

	// reset
	r.ClearAndResetValue(24, 23)
	expectedRanges = []rangeVal[uint32, uint32]{
		{
			start: 24,
			end:   0,
			value: 23,
		},
	}
	require.Equal(t, expectedRanges, r.ranges)

	value, err = r.GetValue(55555555)
	require.NoError(t, err)
	require.Equal(t, uint32(23), value)

	// decrement value and ensure that any key after start in ClearAndResetValue above returns that value
	// (as given end is higher than open range start, open range should be closed and a new range added)
	r.DecValue(34, 12)

	expectedRanges = []rangeVal[uint32, uint32]{
		{
			start: 24,
			end:   34,
			value: 23,
		},
		{
			start: 35,
			end:   0,
			value: 11,
		},
	}
	require.Equal(t, expectedRanges, r.ranges)

	value, err = r.GetValue(55555555)
	require.NoError(t, err)
	require.Equal(t, uint32(11), value)

	// add an exclusion and then decrement value
	err = r.ExcludeRange(40, 45)
	require.NoError(t, err)

	expectedRanges = []rangeVal[uint32, uint32]{
		{
			start: 24,
			end:   34,
			value: 23,
		},
		{
			start: 35,
			end:   39,
			value: 11,
		},
		{
			start: 45,
			end:   0,
			value: 16,
		},
	}
	require.Equal(t, expectedRanges, r.ranges)

	// before first range access
	value, err = r.GetValue(5)
	require.ErrorIs(t, err, errKeyTooOld)

	// first range access
	value, err = r.GetValue(25)
	require.NoError(t, err)
	require.Equal(t, uint32(23), value)

	// second range access
	value, err = r.GetValue(35)
	require.NoError(t, err)
	require.Equal(t, uint32(11), value)

	// open range access
	value, err = r.GetValue(55555555)
	require.NoError(t, err)
	require.Equal(t, uint32(16), value)

	r.DecValue(66, 6)

	expectedRanges = []rangeVal[uint32, uint32]{
		{
			start: 35,
			end:   39,
			value: 11,
		},
		{
			start: 45,
			end:   66,
			value: 16,
		},
		{
			start: 67,
			end:   0,
			value: 10,
		},
	}
	require.Equal(t, expectedRanges, r.ranges)

	// aged out range access
	value, err = r.GetValue(25)
	require.ErrorIs(t, err, errKeyTooOld)

	// access closed range before decrementing value
	value, err = r.GetValue(66)
	require.NoError(t, err)
	require.Equal(t, uint32(16), value)

	// open range access
	value, err = r.GetValue(67)
	require.NoError(t, err)
	require.Equal(t, uint32(10), value)

	// decrement with old end and check that open range gets decremented
	r.DecValue(66, 6)

	expectedRanges = []rangeVal[uint32, uint32]{
		{
			start: 35,
			end:   39,
			value: 11,
		},
		{
			start: 45,
			end:   66,
			value: 16,
		},
		{
			start: 67,
			end:   0,
			value: 4,
		},
	}
	require.Equal(t, expectedRanges, r.ranges)

	// open range access should get decremented value
	value, err = r.GetValue(67)
	require.NoError(t, err)
	require.Equal(t, uint32(4), value)
}

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
	value, err = r.GetValue(0xffffffff)
	require.NoError(t, err)
	require.Equal(t, uint32(0), value)

	// getting value for any key should be incremented value
	r.IncValue(2)
	value, err = r.GetValue(66666666)
	require.NoError(t, err)
	require.Equal(t, uint32(2), value)
	value, err = r.GetValue(0)
	require.NoError(t, err)
	require.Equal(t, uint32(2), value)

	// add a could of ranges, as the value is same should just extend
	err = r.AddRange(10, 20)
	require.NoError(t, err)
	err = r.AddRange(30, 40)
	require.NoError(t, err)
	require.Equal(t, 1, len(r.ranges))
	require.Equal(t, uint32(10), r.ranges[0].start)
	require.Equal(t, uint32(39), r.ranges[0].end)
	require.Equal(t, uint32(2), r.ranges[0].value)

	// bump value
	r.IncValue(1)
	// getting value in previously added range should return 2
	value, err = r.GetValue(22)
	require.NoError(t, err)
	require.Equal(t, uint32(2), value)
	// outside range should return 3
	value, err = r.GetValue(662)
	require.NoError(t, err)
	require.Equal(t, uint32(3), value)

	// adding out-of-order range should return error
	err = r.AddRange(60, 50)
	require.Error(t, err, errReversedOrder)

	// adding overlapping should return error
	err = r.AddRange(30, 50)
	require.Error(t, err, errReversedOrder)

	// adding a non-overlapping range should extend previous range and add new one
	err = r.AddRange(50, 60)
	require.NoError(t, err)
	require.Equal(t, 2, len(r.ranges))

	require.Equal(t, uint32(10), r.ranges[0].start)
	require.Equal(t, uint32(49), r.ranges[0].end)
	require.Equal(t, uint32(2), r.ranges[0].value)

	require.Equal(t, uint32(50), r.ranges[1].start)
	require.Equal(t, uint32(59), r.ranges[1].end)
	require.Equal(t, uint32(3), r.ranges[1].value)

	// getting an old value should not succeed, but start of first range should return no error
	value, err = r.GetValue(9)
	require.Error(t, err, errKeyNotFound)
	value, err = r.GetValue(10)
	require.NoError(t, err)
	require.Equal(t, uint32(2), value)

	// adding another range should prune the first one as size if set to 2
	r.IncValue(10)
	err = r.AddRange(1000, 1233)
	require.NoError(t, err)
	require.Equal(t, 2, len(r.ranges))

	require.Equal(t, uint32(50), r.ranges[0].start)
	require.Equal(t, uint32(999), r.ranges[0].end)
	require.Equal(t, uint32(3), r.ranges[0].value)

	require.Equal(t, uint32(1000), r.ranges[1].start)
	require.Equal(t, uint32(1232), r.ranges[1].end)
	require.Equal(t, uint32(13), r.ranges[1].value)

	// previously valid range should return key not found after pruning
	value, err = r.GetValue(10)
	require.Error(t, err, errKeyNotFound)

	value, err = r.GetValue(999)
	require.NoError(t, err)
	require.Equal(t, uint32(3), value)

	value, err = r.GetValue(1200)
	require.NoError(t, err)
	require.Equal(t, uint32(13), value)

	// something newer than what is in ranges should return running value
	value, err = r.GetValue(3000)
	require.NoError(t, err)
	require.Equal(t, uint32(13), value)
}

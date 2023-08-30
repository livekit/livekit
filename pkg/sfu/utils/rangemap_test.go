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

	// getting value for older key should be un-incremented value
	// should get incremented value post the end
	err = r.CloseRangeAndIncValue(66666667, 2)
	require.NoError(t, err)
	value, err = r.GetValue(66666666)
	require.NoError(t, err)
	require.Equal(t, uint32(0), value)
	value, err = r.GetValue(0)
	require.NoError(t, err)
	require.Equal(t, uint32(0), value)
	value, err = r.GetValue(66666667)
	require.NoError(t, err)
	require.Equal(t, uint32(2), value)

	require.Equal(t, uint32(0), r.ranges[0].start)
	require.Equal(t, uint32(66666666), r.ranges[0].end)
	require.Equal(t, uint32(0), r.ranges[0].value)

	// duplicate should not create a new range
	err = r.CloseRangeAndIncValue(66666667, 2)
	require.NoError(t, err)

	require.Equal(t, 1, len(r.ranges))

	require.Equal(t, uint32(0), r.ranges[0].start)
	require.Equal(t, uint32(66666666), r.ranges[0].end)
	require.Equal(t, uint32(0), r.ranges[0].value)

	require.Equal(t, uint32(4), r.runningValue)

	// out-of-order should fail
	err = r.CloseRangeAndIncValue(66666666, 2)
	require.Error(t, err, errReversedOrder)

	// add a couple of more increments and the first one should fall off
	err = r.CloseRangeAndIncValue(88888889, 2)
	require.NoError(t, err)

	require.Equal(t, uint32(0), r.ranges[0].start)
	require.Equal(t, uint32(66666666), r.ranges[0].end)
	require.Equal(t, uint32(0), r.ranges[0].value)

	require.Equal(t, uint32(66666667), r.ranges[1].start)
	require.Equal(t, uint32(88888888), r.ranges[1].end)
	require.Equal(t, uint32(4), r.ranges[1].value)

	err = r.CloseRangeAndIncValue(99999999, 2)
	require.NoError(t, err)

	require.Equal(t, uint32(66666667), r.ranges[0].start)
	require.Equal(t, uint32(88888888), r.ranges[0].end)
	require.Equal(t, uint32(4), r.ranges[0].value)

	require.Equal(t, uint32(88888889), r.ranges[1].start)
	require.Equal(t, uint32(99999998), r.ranges[1].end)
	require.Equal(t, uint32(6), r.ranges[1].value)

	// getting an old value should not succeed, but start of first range should return no error
	value, err = r.GetValue(66666666)
	require.Error(t, err, errKeyNotFound)
	value, err = r.GetValue(66666667)
	require.NoError(t, err)
	require.Equal(t, uint32(4), value)

	// something newer than what is in ranges should return running value
	value, err = r.GetValue(99999999)
	require.NoError(t, err)
	require.Equal(t, uint32(8), value)

	// decrement running value
	err = r.CloseRangeAndDecValue(99999999+1000, 3)
	require.NoError(t, err)

	require.Equal(t, uint32(88888889), r.ranges[0].start)
	require.Equal(t, uint32(99999998), r.ranges[0].end)
	require.Equal(t, uint32(6), r.ranges[0].value)

	require.Equal(t, uint32(99999999), r.ranges[1].start)
	require.Equal(t, uint32(99999999+999), r.ranges[1].end)
	require.Equal(t, uint32(8), r.ranges[1].value)

	value, err = r.GetValue(99999999)
	require.NoError(t, err)
	require.Equal(t, uint32(8), value)

	value, err = r.GetValue(99999999 + 1000)
	require.NoError(t, err)
	require.Equal(t, uint32(5), value)
}

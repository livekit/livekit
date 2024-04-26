// Copyright 2024 LiveKit, Inc.
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

package playoutdelay

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPlayoutDelay(t *testing.T) {
	p1 := PlayOutDelay{Min: 100, Max: 200}
	b, err := p1.Marshal()
	require.NoError(t, err)
	require.Len(t, b, playoutDelayExtensionSize)
	var p2 PlayOutDelay
	err = p2.Unmarshal(b)
	require.NoError(t, err)
	require.Equal(t, p1, p2)

	// overflow
	p3 := PlayOutDelay{Min: 100, Max: (1 << 12) * 10}
	_, err = p3.Marshal()
	require.ErrorIs(t, err, errPlayoutDelayOverflow)

	// too small
	p4 := PlayOutDelay{}
	err = p4.Unmarshal([]byte{0x00, 0x00})
	require.ErrorIs(t, err, errTooSmall)

	// from value
	p5 := PlayoutDelayFromValue(1<<12*10, 1<<12*10+10)
	_, err = p5.Marshal()
	require.NoError(t, err)
	require.Equal(t, uint16((1<<12)-1)*10, p5.Min)
	require.Equal(t, uint16((1<<12)-1)*10, p5.Max)

	p6 := PlayOutDelay{Min: 100, Max: PlayoutDelayMaxValue}
	bytes, err := p6.Marshal()
	require.NoError(t, err)
	p6Unmarshal := PlayOutDelay{}
	err = p6Unmarshal.Unmarshal(bytes)
	require.NoError(t, err)
	require.Equal(t, p6, p6Unmarshal)
}

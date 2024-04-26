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

package buffer

import (
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"

	dd "github.com/livekit/livekit-server/pkg/sfu/rtpextension/dependencydescriptor"
)

func TestFrameIntegrityChecker(t *testing.T) {
	fc := NewFrameIntegrityChecker(100, 1000)

	// first frame out of order
	fc.AddPacket(10, 10, &dd.DependencyDescriptor{})
	require.False(t, fc.FrameIntegrity(10))
	fc.AddPacket(9, 10, &dd.DependencyDescriptor{FirstPacketInFrame: true})
	require.False(t, fc.FrameIntegrity(10))
	fc.AddPacket(11, 10, &dd.DependencyDescriptor{LastPacketInFrame: true})
	require.True(t, fc.FrameIntegrity(10))

	// single packet frame
	fc.AddPacket(100, 100, &dd.DependencyDescriptor{FirstPacketInFrame: true, LastPacketInFrame: true})
	require.True(t, fc.FrameIntegrity(100))
	require.False(t, fc.FrameIntegrity(101))
	require.False(t, fc.FrameIntegrity(99))

	// frame too old than first frame
	fc.AddPacket(99, 99, &dd.DependencyDescriptor{FirstPacketInFrame: true, LastPacketInFrame: true})

	// multiple packet frame, out of order
	fc.AddPacket(2001, 2001, &dd.DependencyDescriptor{})
	require.False(t, fc.FrameIntegrity(2001))
	require.False(t, fc.FrameIntegrity(1999))
	// out of frame count(100)
	require.False(t, fc.FrameIntegrity(100))
	require.False(t, fc.FrameIntegrity(1900))

	fc.AddPacket(2000, 2001, &dd.DependencyDescriptor{FirstPacketInFrame: true})
	require.False(t, fc.FrameIntegrity(2001))
	fc.AddPacket(2002, 2001, &dd.DependencyDescriptor{LastPacketInFrame: true})
	require.True(t, fc.FrameIntegrity(2001))
	// duplicate packet
	fc.AddPacket(2001, 2001, &dd.DependencyDescriptor{})
	require.True(t, fc.FrameIntegrity(2001))

	// frame too old
	fc.AddPacket(900, 1900, &dd.DependencyDescriptor{FirstPacketInFrame: true, LastPacketInFrame: true})
	require.False(t, fc.FrameIntegrity(1900))

	for frame := uint64(2002); frame < 2102; frame++ {
		// large frame (1000 packets) out of order / retransmitted
		firstFrame := uint64(3000 + (frame-2002)*1000)
		lastFrame := uint64(3999 + (frame-2002)*1000)
		frames := make([]uint64, 0, lastFrame-firstFrame+1)
		for i := firstFrame; i <= lastFrame; i++ {
			frames = append(frames, i)
		}
		require.False(t, fc.FrameIntegrity(frame))
		rand.Seed(int64(frame))
		rand.Shuffle(len(frames), func(i, j int) { frames[i], frames[j] = frames[j], frames[i] })
		for i, f := range frames {
			fc.AddPacket(f, frame, &dd.DependencyDescriptor{
				FirstPacketInFrame: f == firstFrame,
				LastPacketInFrame:  f == lastFrame,
			})
			require.Equal(t, i == len(frames)-1, fc.FrameIntegrity(frame), i)
		}
		require.True(t, fc.FrameIntegrity(frame))
	}
}

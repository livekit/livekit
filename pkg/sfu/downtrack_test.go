// Copyright 2026 LiveKit, Inc.
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

package sfu_test

import (
	"testing"

	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/pacer"
	"github.com/livekit/livekit-server/pkg/sfu/sfufakes"
)

func TestDownTrackSetReceiver(t *testing.T) {
	newDownTrack := func(t *testing.T, r sfu.TrackReceiver) *sfu.DownTrack {
		dt, err := sfu.NewDownTrack(sfu.DownTrackParams{
			Codecs:        []webrtc.RTPCodecParameters{vp8CodecParams},
			Receiver:      r,
			BufferFactory: buffer.NewFactoryOfBufferFactory(500, 500).CreateBufferFactory(),
			Pacer:         pacer.NewPassThrough(logger.GetLogger(), newNullBWE()),
			Logger:        logger.GetLogger(),
			Listener:      &sfufakes.FakeDownTrackListener{},
			StreamID:      "fake-stream",
			SubID:         "fake-sub",
			MaxTrack:      500,
		})
		require.NoError(t, err)
		t.Cleanup(dt.Close)
		return dt
	}

	t.Run("moves to the new receiver", func(t *testing.T) {
		r1 := newFakeTrackReceiver(vp8CodecParams)
		dt := newDownTrack(t, r1)

		r2 := newFakeTrackReceiver(vp8CodecParams)
		dt.SetReceiver(r2)

		require.Equal(t, sfu.TrackReceiver(r2), dt.Receiver())
		require.Equal(t, 1, r1.DeleteDownTrackCallCount())
		require.Equal(t, 1, r2.AddDownTrackCallCount())
		require.Same(t, dt, r2.AddDownTrackArgsForCall(0))
	})

	t.Run("closed before the switch", func(t *testing.T) {
		r1 := newFakeTrackReceiver(vp8CodecParams)
		dt := newDownTrack(t, r1)
		dt.Close()

		r2 := newFakeTrackReceiver(vp8CodecParams)
		dt.SetReceiver(r2)

		require.Equal(t, sfu.TrackReceiver(r1), dt.Receiver())
		require.Zero(t, r2.AddOnReadyCallCount())
		require.Zero(t, r2.AddDownTrackCallCount())
	})

	// SetReceiver releases bindLock while it registers for readiness and handles a codec
	// change; a close in that window deletes the down track from the new receiver before
	// it was added, so it must not be added afterwards
	t.Run("closed during the switch", func(t *testing.T) {
		r1 := newFakeTrackReceiver(vp8CodecParams)
		dt := newDownTrack(t, r1)

		r2 := newFakeTrackReceiver(vp8CodecParams)
		r2.AddOnReadyCalls(func(f func()) {
			dt.Close()
			f()
		})
		dt.SetReceiver(r2)

		require.True(t, dt.IsClosed())
		require.Equal(t, 1, r2.DeleteDownTrackCallCount())
		require.Zero(t, r2.AddDownTrackCallCount(), "closed down track was added to the new receiver")
	})
}

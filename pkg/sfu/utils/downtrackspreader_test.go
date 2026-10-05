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

package utils

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
)

type testPacket struct{ seq int }

type testSender struct {
	id     livekit.ParticipantID
	writes atomic.Int32
	accept int32
}

func (s *testSender) SubscriberID() livekit.ParticipantID { return s.id }

func (s *testSender) WriteRTP(pkt *testPacket, layer int32) int32 {
	s.writes.Add(1)
	return s.accept
}

func newTestSpreader(numDownTracks, threshold int) (*DownTrackSpreader[*testSender], []*testSender) {
	d := NewDownTrackSpreader[*testSender](DownTrackSpreaderParams{Threshold: threshold})
	senders := make([]*testSender, numDownTracks)
	for i := range senders {
		senders[i] = &testSender{id: livekit.ParticipantID(fmt.Sprintf("p%d", i)), accept: 1}
		d.Store(senders[i])
	}
	return d, senders
}

func TestBroadcastRTP(t *testing.T) {
	for _, tc := range []struct {
		name          string
		numDownTracks int
		threshold     int
		maxAllocs     float64
	}{
		{"serial", 5, 20, 0},
		{"parallel", 50, 20, 2}, // shared state and worker funcval
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, senders := newTestSpreader(tc.numDownTracks, tc.threshold)
			senders[0].accept = 0 // one down track that drops
			pkt := &testPacket{}

			written := BroadcastRTP(d, pkt, 2)
			require.EqualValues(t, tc.numDownTracks-1, written)
			for _, s := range senders {
				require.EqualValues(t, 1, s.writes.Load())
			}

			if !RaceEnabled {
				allocs := testing.AllocsPerRun(1000, func() { BroadcastRTP(d, pkt, 2) })
				require.LessOrEqual(t, allocs, tc.maxAllocs)
			}
		})
	}
}

func TestBroadcastRTPEmpty(t *testing.T) {
	d := NewDownTrackSpreader[*testSender](DownTrackSpreaderParams{Threshold: 20})
	require.EqualValues(t, 0, BroadcastRTP(d, &testPacket{}, 0))
}

func TestDownTrackSpreaderResetRemainsReusable(t *testing.T) {
	spreader := NewDownTrackSpreader[*testSender](DownTrackSpreaderParams{})
	first := &testSender{id: "first"}
	second := &testSender{id: "second"}

	spreader.Store(first)
	require.Equal(t, []*testSender{first}, spreader.ResetAndGetDownTracks())
	require.Zero(t, spreader.DownTrackCount())

	spreader.Store(second)
	require.Equal(t, []*testSender{second}, spreader.GetDownTracks())
}

func TestDownTrackSpreaderRejectsStoreAfterClose(t *testing.T) {
	spreader := NewDownTrackSpreader[*testSender](DownTrackSpreaderParams{})
	first := &testSender{id: "first"}
	late := &testSender{id: "late"}

	spreader.Store(first)
	require.Equal(t, []*testSender{first}, spreader.CloseAndGetDownTracks())
	require.Zero(t, spreader.DownTrackCount())

	require.False(t, spreader.TryStore(late))
	require.Zero(t, spreader.DownTrackCount())
}

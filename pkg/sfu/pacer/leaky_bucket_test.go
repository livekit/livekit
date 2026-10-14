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

package pacer

import (
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/ccutils"
)

// pacedWriter records send times so tests can observe the pacing of writes
type pacedWriter struct {
	lock  sync.Mutex
	times []time.Time
}

func (w *pacedWriter) WriteRTP(_ *rtp.Header, payload []byte) (int, error) {
	w.lock.Lock()
	defer w.lock.Unlock()

	w.times = append(w.times, time.Now())
	return len(payload), nil
}

func (w *pacedWriter) Write(data []byte) (int, error) {
	return len(data), nil
}

func (w *pacedWriter) snapshot() (int, time.Time, time.Time) {
	w.lock.Lock()
	defer w.lock.Unlock()

	if len(w.times) == 0 {
		return 0, time.Time{}, time.Time{}
	}
	return len(w.times), w.times[0], w.times[len(w.times)-1]
}

func enqueuePackets(l *LeakyBucket, w *pacedWriter, count, payloadSize int) {
	for i := 0; i < count; i++ {
		p := PacketFactory.Get().(*Packet)
		header := &rtp.Header{Version: 2, SequenceNumber: uint16(i), Timestamp: uint32(i), SSRC: 1}
		*p = Packet{
			Header:      header,
			HeaderSize:  header.MarshalSize(),
			Payload:     make([]byte, payloadSize),
			WriteStream: w,
		}
		l.Enqueue(p)
	}
}

func TestLeakyBucketPacesToBitrate(t *testing.T) {
	w := &pacedWriter{}
	l := NewLeakyBucket(logger.GetLogger(), nil, 10*time.Millisecond, 2_000_000)
	defer l.Stop()

	// 40 packets of 500 bytes is 20 kB, 160 kbit. at 2 Mbps that is 80 ms of
	// budget, and the bucket allows up to 2x overshoot per interval, so full
	// delivery cannot complete in fewer than 4 release intervals
	enqueuePackets(l, w, 40, 500)

	require.Eventually(t, func() bool {
		count, _, _ := w.snapshot()
		return count == 40
	}, 5*time.Second, 5*time.Millisecond)

	_, first, last := w.snapshot()
	require.GreaterOrEqual(t, last.Sub(first), 30*time.Millisecond)
}

func TestLeakyBucketUsesProbeTargetBitrate(t *testing.T) {
	w := &pacedWriter{}
	l := NewLeakyBucket(logger.GetLogger(), nil, 10*time.Millisecond, 100_000)
	defer l.Stop()

	// at 100 kbps alone, 20 kB takes 1.6 s to deliver
	l.StartProbeCluster(ccutils.ProbeClusterInfo{
		Id: 1,
		Goal: ccutils.ProbeClusterGoal{
			DesiredBps:   8_000_000,
			Duration:     time.Second,
			DesiredBytes: 1 << 20,
		},
	})

	enqueuePackets(l, w, 40, 500)

	// while the cluster is active, egress paces at the probe target, so the
	// same 20 kB must drain in a small fraction of the 1.6 s
	require.Eventually(t, func() bool {
		count, _, _ := w.snapshot()
		return count == 40
	}, time.Second, 5*time.Millisecond)

	// once the cluster ends, pacing returns to the media bitrate
	l.EndProbeCluster(1)

	enqueuePackets(l, w, 40, 500)
	time.Sleep(100 * time.Millisecond)

	count, _, _ := w.snapshot()
	require.Less(t, count, 80)
}

func TestLeakyBucketBoundsQueue(t *testing.T) {
	w := &pacedWriter{}
	l := NewLeakyBucket(logger.GetLogger(), nil, 10*time.Millisecond, 100_000)
	defer l.Stop()

	// the queue holds at most 0.5 s of media at the active rate, 6,250 bytes
	// at 100 kbps. 400 packets of 500 bytes far exceed it, so most of them
	// must be dropped and the queue must still drain
	enqueuePackets(l, w, 400, 500)

	var last int
	require.Eventually(t, func() bool {
		count, _, _ := w.snapshot()
		stable := count == last && count > 0
		last = count
		return stable
	}, 10*time.Second, 100*time.Millisecond)

	count, _, _ := w.snapshot()
	require.Less(t, count, 400)
	require.Greater(t, count, 0)
}

func TestLeakyBucketPassesOversizedPacket(t *testing.T) {
	w := &pacedWriter{}
	l := NewLeakyBucket(logger.GetLogger(), nil, 10*time.Millisecond, 100_000)
	defer l.Stop()

	// 10 kB is larger than the whole queue bound (0.5 s at 100 kbps is 6,250
	// bytes). a packet that size must still be delivered rather than dropped
	// the moment it is enqueued
	enqueuePackets(l, w, 1, 10_000)

	require.Eventually(t, func() bool {
		count, _, _ := w.snapshot()
		return count == 1
	}, 2*time.Second, 5*time.Millisecond)
}

func TestLeakyBucketDropsAtFrameBoundaries(t *testing.T) {
	// drive Enqueue without the send worker so the drop decisions are
	// deterministic and the queue can be inspected directly
	newIdleBucket := func() *LeakyBucket {
		l := &LeakyBucket{
			Base:     NewBase(logger.GetLogger(), nil),
			logger:   logger.GetLogger(),
			interval: 10 * time.Millisecond,
			bitrate:  100_000, // queue bound is 6,250 bytes
		}
		l.packets.SetBaseCap(512)
		return l
	}

	// frames of 5 packets, marker on the last packet of each frame
	enqueueFrame := func(l *LeakyBucket, seq *uint16) {
		for i := 0; i < 5; i++ {
			hdr := &rtp.Header{
				Version:        2,
				SequenceNumber: *seq,
				Marker:         i == 4,
				SSRC:           1,
			}
			*seq++
			l.Enqueue(&Packet{
				Header:     hdr,
				HeaderSize: hdr.MarshalSize(),
				Payload:    make([]byte, 500),
			})
		}
	}

	seqNumbers := func(l *LeakyBucket) []uint16 {
		out := make([]uint16, 0, l.packets.Len())
		for i := 0; i < l.packets.Len(); i++ {
			out = append(out, l.packets.At(i).Header.SequenceNumber)
		}
		return out
	}

	t.Run("drops whole frames from the front", func(t *testing.T) {
		l := newIdleBucket()

		seq := uint16(0)
		for i := 0; i < 4; i++ {
			enqueueFrame(l, &seq)
		}

		// frame 0 was fully dropped and frame 1 was dropped through its
		// marker, so the queue starts at frame 2 and holds two whole frames
		require.Equal(t, []uint16{10, 11, 12, 13, 14, 15, 16, 17, 18, 19}, seqNumbers(l))
		require.LessOrEqual(t, l.queuedBytes, l.maxQueueBytesLocked())
	})

	t.Run("oversized packet is retained", func(t *testing.T) {
		l := newIdleBucket()

		hdr := &rtp.Header{Version: 2, SequenceNumber: 1, SSRC: 1}
		l.Enqueue(&Packet{
			Header:     hdr,
			HeaderSize: hdr.MarshalSize(),
			Payload:    make([]byte, 10_000),
		})

		require.Equal(t, 1, l.packets.Len())
		require.Greater(t, l.queuedBytes, l.maxQueueBytesLocked())
	})
}

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

package buffer

import (
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	"github.com/livekit/mediatransportutil/pkg/bucket"
	"github.com/livekit/protocol/logger"
)

func newVideoFrameCacheTestBuffer(maxDuration time.Duration) *BufferBase {
	b := &BufferBase{
		codecType: webrtc.RTPCodecTypeVideo,
		clockRate: 90000,
		logger:    logger.GetLogger(),
	}
	b.bucket = bucket.NewBucket[uint64, uint16](256, bucket.RTPMaxPktSize, bucket.RTPSeqNumOffset)
	b.EnableVideoFrameCache(maxDuration)
	return b
}

// addBucketPacket marshals an RTP packet and stores it in the bucket keyed by ext sequence number.
func addBucketPacket(t *testing.T, b *BufferBase, extSN uint64, ts uint32) {
	p := rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: uint16(extSN), Timestamp: ts, SSRC: 123},
		Payload: []byte{1, 2, 3, 4},
	}
	raw, err := p.Marshal()
	require.NoError(t, err)
	_, err = b.bucket.AddPacketWithSequenceNumber(raw, extSN)
	require.NoError(t, err)
}

func videoFrameCacheMarkPkt(extSN, extTS uint64, keyFrame bool) *ExtPacket {
	return &ExtPacket{
		ExtSequenceNumber: extSN,
		ExtTimestamp:      extTS,
		IsKeyFrame:        keyFrame,
		Packet:            &rtp.Packet{Payload: []byte{1, 2, 3, 4}},
	}
}

func TestVideoFrameCacheReadsFromBucket(t *testing.T) {
	b := newVideoFrameCacheTestBuffer(0)

	// store SN 100..104 in the bucket
	for i := uint64(0); i < 5; i++ {
		addBucketPacket(t, b, 100+i, uint32(1000*(i+1)))
	}

	// no key frame marked yet -> not available
	_, ok := b.GetVideoFrameCache()
	require.False(t, ok)

	// mark: delta at 100, key frame at 101, deltas after
	b.markVideoFrameCacheLocked(videoFrameCacheMarkPkt(100, 1000, false))
	b.markVideoFrameCacheLocked(videoFrameCacheMarkPkt(101, 2000, true))
	b.markVideoFrameCacheLocked(videoFrameCacheMarkPkt(102, 3000, false))
	b.markVideoFrameCacheLocked(videoFrameCacheMarkPkt(103, 4000, false))
	b.markVideoFrameCacheLocked(videoFrameCacheMarkPkt(104, 5000, false))

	// video frame cache group is [key frame .. head] = 101..104
	pkts, ok := b.GetVideoFrameCache()
	require.True(t, ok)
	require.Len(t, pkts, 4)
	require.Equal(t, uint16(101), pkts[0].Packet.SequenceNumber)
	require.Equal(t, uint64(101), pkts[0].ExtSequenceNumber)
	require.True(t, pkts[0].IsKeyFrame) // first packet is at the key-frame timestamp
	require.False(t, pkts[1].IsKeyFrame)
	require.Equal(t, uint16(104), pkts[3].Packet.SequenceNumber)

	// a new key frame moves the boundary
	addBucketPacket(t, b, 105, 6000)
	b.markVideoFrameCacheLocked(videoFrameCacheMarkPkt(105, 6000, true))
	pkts, ok = b.GetVideoFrameCache()
	require.True(t, ok)
	require.Len(t, pkts, 1)
	require.Equal(t, uint16(105), pkts[0].Packet.SequenceNumber)
	require.True(t, pkts[0].IsKeyFrame)
}

func TestVideoFrameCacheGetPacketsAfter(t *testing.T) {
	b := newVideoFrameCacheTestBuffer(0)
	for i := uint64(0); i < 5; i++ {
		addBucketPacket(t, b, 100+i, uint32(1000*(i+1)))
	}
	b.markVideoFrameCacheLocked(videoFrameCacheMarkPkt(100, 1000, true))
	for i := uint64(1); i < 5; i++ {
		b.markVideoFrameCacheLocked(videoFrameCacheMarkPkt(100+i, 1000*(i+1), false))
	}

	// everything after 102 is 103, 104
	more, ok := b.GetPacketsAfter(102)
	require.True(t, ok)
	require.Len(t, more, 2)
	require.Equal(t, uint64(103), more[0].ExtSequenceNumber)
	require.Equal(t, uint64(104), more[1].ExtSequenceNumber)

	// caught up: nothing after the head
	_, ok = b.GetPacketsAfter(104)
	require.False(t, ok)
}

func TestVideoFrameCacheSkipsLostPackets(t *testing.T) {
	b := newVideoFrameCacheTestBuffer(0)

	// store the key frame and a later packet, leaving a gap at 101
	addBucketPacket(t, b, 100, 1000)
	addBucketPacket(t, b, 102, 3000)

	b.markVideoFrameCacheLocked(videoFrameCacheMarkPkt(100, 1000, true))
	b.markVideoFrameCacheLocked(videoFrameCacheMarkPkt(102, 3000, false))

	pkts, ok := b.GetVideoFrameCache()
	require.True(t, ok)
	require.Len(t, pkts, 2) // the missing 101 is skipped
	require.Equal(t, uint16(100), pkts[0].Packet.SequenceNumber)
	require.Equal(t, uint16(102), pkts[1].Packet.SequenceNumber)
}

func TestVideoFrameCacheKeyFrameEvicted(t *testing.T) {
	b := newVideoFrameCacheTestBuffer(0)

	// mark a key frame at a sequence number that is not in the bucket
	addBucketPacket(t, b, 200, 1000)
	b.markVideoFrameCacheLocked(videoFrameCacheMarkPkt(100, 500, true)) // 100 was never stored / evicted
	b.videoFrameCacheLatestTS = 1000

	_, ok := b.GetVideoFrameCache()
	require.False(t, ok)
}

func TestVideoFrameCacheDurationBound(t *testing.T) {
	// 2s cap at 90kHz -> 180000 ticks
	b := newVideoFrameCacheTestBuffer(2 * time.Second)
	addBucketPacket(t, b, 100, 1000)

	const kfTS = uint64(1_000_000)
	b.markVideoFrameCacheLocked(videoFrameCacheMarkPkt(100, kfTS, true))

	// within the bound
	b.videoFrameCacheLatestTS = kfTS + 90000 // +1s
	_, ok := b.GetVideoFrameCache()
	require.True(t, ok)

	// beyond the bound -> not served
	b.videoFrameCacheLatestTS = kfTS + 180001 // > 2s
	_, ok = b.GetVideoFrameCache()
	require.False(t, ok)
}

func TestVideoFrameCacheSpanUsesMaxTimestamp(t *testing.T) {
	b := newVideoFrameCacheTestBuffer(0)

	// key frame at 1000, then a later packet at 1200 advances the span
	b.markVideoFrameCacheLocked(videoFrameCacheMarkPkt(100, 1000, true))
	b.markVideoFrameCacheLocked(videoFrameCacheMarkPkt(101, 1200, false))
	require.Equal(t, uint64(1200), b.videoFrameCacheLatestTS)

	// an out-of-order, older packet arriving last must not shrink the measured span
	b.markVideoFrameCacheLocked(videoFrameCacheMarkPkt(102, 1100, false))
	require.Equal(t, uint64(1200), b.videoFrameCacheLatestTS)

	// a new key frame resets the span to itself (a stale packet cannot stretch the new video frame cache group)
	b.markVideoFrameCacheLocked(videoFrameCacheMarkPkt(103, 5000, true))
	require.Equal(t, uint64(5000), b.videoFrameCacheLatestTS)
	b.markVideoFrameCacheLocked(videoFrameCacheMarkPkt(104, 4000, false)) // stale, older than the new key frame
	require.Equal(t, uint64(5000), b.videoFrameCacheLatestTS)
}

func TestBucketGrowTarget(t *testing.T) {
	const pps = 600
	const maxPkts = 500 // default ~1s cap

	// video frame cache sizing off -> unchanged original behavior: target is ~1s (pps), cap stays maxPkts
	target, cap := bucketGrowTarget(pps, maxPkts, false, 0)
	require.Equal(t, pps, target)
	require.Equal(t, maxPkts, cap)

	// videoFrameCacheMaxDuration <= 0 is treated as not sizing even if the flag is set
	target, cap = bucketGrowTarget(pps, maxPkts, true, 0)
	require.Equal(t, pps, target)
	require.Equal(t, maxPkts, cap)

	// video frame cache sizing on, 2s: target is pps*2 + 0.5s margin, and the cap is raised to fit
	target, cap = bucketGrowTarget(pps, maxPkts, true, 2*time.Second)
	require.Equal(t, pps*2+pps/2, target) // 1500
	require.Equal(t, target, cap)         // raised above the 500 default
	require.Greater(t, cap, maxPkts)

	// video frame cache sizing on but the target fits within maxPkts -> cap is not lowered
	target, cap = bucketGrowTarget(100, 1000, true, 2*time.Second)
	require.Equal(t, 100*2+100/2, target) // 250
	require.Equal(t, 1000, cap)           // unchanged, target < maxPkts

	// scales with duration
	target1s, _ := bucketGrowTarget(pps, maxPkts, true, time.Second)
	target3s, _ := bucketGrowTarget(pps, maxPkts, true, 3*time.Second)
	require.Greater(t, target3s, target1s)
}

func TestVideoFrameCacheDisabled(t *testing.T) {
	b := &BufferBase{codecType: webrtc.RTPCodecTypeVideo, clockRate: 90000, logger: logger.GetLogger()}
	b.bucket = bucket.NewBucket[uint64, uint16](256, bucket.RTPMaxPktSize, bucket.RTPSeqNumOffset)
	addBucketPacket(t, b, 100, 1000)
	b.markVideoFrameCacheLocked(videoFrameCacheMarkPkt(100, 1000, true)) // marking is a no-op while disabled, but set fields anyway

	_, ok := b.GetVideoFrameCache()
	require.False(t, ok)

	// audio buffers never enable the cache
	a := &BufferBase{codecType: webrtc.RTPCodecTypeAudio, clockRate: 48000, logger: logger.GetLogger()}
	a.EnableVideoFrameCache(0)
	require.False(t, a.videoFrameCacheEnabled)
}

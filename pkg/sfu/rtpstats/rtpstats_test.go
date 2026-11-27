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

package rtpstats

import (
	"fmt"
	"math/rand"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"

	"github.com/livekit/mediatransportutil"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils/mono"
)

func getPacket(sn uint16, ts uint32, payloadSize int) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			SequenceNumber: sn,
			Timestamp:      ts,
		},
		Payload: make([]byte, payloadSize),
	}
}

func Test_RTPStatsReceiver_Update(t *testing.T) {
	clockRate := uint32(90000)
	r := NewRTPStatsReceiver(RTPStatsParams{})
	r.SetClockRate(clockRate)

	sequenceNumber := uint16(rand.Float64() * float64(1<<16))
	timestamp := uint32(rand.Float64() * float64(1<<32))
	packet := getPacket(sequenceNumber, timestamp, 1000)
	flowState := r.Update(
		time.Now().UnixNano(),
		packet.Header.SequenceNumber,
		packet.Header.Timestamp,
		packet.Header.Marker,
		packet.Header.MarshalSize(),
		len(packet.Payload),
		0,
	)
	require.True(t, r.initialized)
	require.Equal(t, sequenceNumber, r.sequenceNumber.GetHighest())
	require.Equal(t, sequenceNumber, uint16(r.sequenceNumber.GetExtendedHighest()))
	require.Equal(t, timestamp, r.timestamp.GetHighest())
	require.Equal(t, timestamp, uint32(r.timestamp.GetExtendedHighest()))

	// in-order, no loss
	sequenceNumber++
	timestamp += 3000
	packet = getPacket(sequenceNumber, timestamp, 1000)
	flowState = r.Update(
		time.Now().UnixNano(),
		packet.Header.SequenceNumber,
		packet.Header.Timestamp,
		packet.Header.Marker,
		packet.Header.MarshalSize(),
		len(packet.Payload),
		0,
	)
	require.Equal(t, sequenceNumber, r.sequenceNumber.GetHighest())
	require.Equal(t, sequenceNumber, uint16(r.sequenceNumber.GetExtendedHighest()))
	require.Equal(t, timestamp, r.timestamp.GetHighest())
	require.Equal(t, timestamp, uint32(r.timestamp.GetExtendedHighest()))

	// out-of-order, would cause a restart which is disallowed
	packet = getPacket(sequenceNumber-10, timestamp-30000, 1000)
	flowState = r.Update(
		time.Now().UnixNano(),
		packet.Header.SequenceNumber,
		packet.Header.Timestamp,
		packet.Header.Marker,
		packet.Header.MarshalSize(),
		len(packet.Payload),
		0,
	)
	require.Equal(t, RTPFlowUnhandledReasonPreStartTimestamp, flowState.UnhandledReason)
	require.Equal(t, sequenceNumber, r.sequenceNumber.GetHighest())
	require.Equal(t, sequenceNumber, uint16(r.sequenceNumber.GetExtendedHighest()))
	require.Equal(t, timestamp, r.timestamp.GetHighest())
	require.Equal(t, timestamp, uint32(r.timestamp.GetExtendedHighest()))
	require.Equal(t, uint64(0), r.packetsOutOfOrder)
	require.Equal(t, uint64(0), r.packetsDuplicate)

	// duplicate of the above out-of-order packet, but would not be handled as it causes a restart
	packet = getPacket(sequenceNumber-10, timestamp-30000, 1000)
	flowState = r.Update(
		time.Now().UnixNano(),
		packet.Header.SequenceNumber,
		packet.Header.Timestamp,
		packet.Header.Marker,
		packet.Header.MarshalSize(),
		len(packet.Payload),
		0,
	)
	require.Equal(t, RTPFlowUnhandledReasonPreStartTimestamp, flowState.UnhandledReason)
	require.Equal(t, sequenceNumber, r.sequenceNumber.GetHighest())
	require.Equal(t, sequenceNumber, uint16(r.sequenceNumber.GetExtendedHighest()))
	require.Equal(t, timestamp, r.timestamp.GetHighest())
	require.Equal(t, timestamp, uint32(r.timestamp.GetExtendedHighest()))
	require.Equal(t, uint64(0), r.packetsOutOfOrder)
	require.Equal(t, uint64(0), r.packetsDuplicate)

	// loss
	sequenceNumber += 10
	timestamp += 30000
	packet = getPacket(sequenceNumber, timestamp, 1000)
	flowState = r.Update(
		time.Now().UnixNano(),
		packet.Header.SequenceNumber,
		packet.Header.Timestamp,
		packet.Header.Marker,
		packet.Header.MarshalSize(),
		len(packet.Payload),
		0,
	)
	require.Equal(t, uint64(sequenceNumber-9), flowState.LossStartInclusive)
	require.Equal(t, uint64(sequenceNumber), flowState.LossEndExclusive)
	require.Equal(t, uint64(9), r.packetsLost)

	// out-of-order should decrement number of lost packets
	packet = getPacket(sequenceNumber-6, timestamp-18000, 1000)
	flowState = r.Update(
		time.Now().UnixNano(),
		packet.Header.SequenceNumber,
		packet.Header.Timestamp,
		packet.Header.Marker,
		packet.Header.MarshalSize(),
		len(packet.Payload),
		0,
	)
	require.Equal(t, sequenceNumber, r.sequenceNumber.GetHighest())
	require.Equal(t, sequenceNumber, uint16(r.sequenceNumber.GetExtendedHighest()))
	require.Equal(t, timestamp, r.timestamp.GetHighest())
	require.Equal(t, timestamp, uint32(r.timestamp.GetExtendedHighest()))
	require.Equal(t, uint64(1), r.packetsOutOfOrder)
	require.Equal(t, uint64(0), r.packetsDuplicate)
	require.Equal(t, uint64(8), r.packetsLost)

	// test sequence number history
	// with a gap
	sequenceNumber += 2
	timestamp += 6000
	packet = getPacket(sequenceNumber, timestamp, 1000)
	flowState = r.Update(
		time.Now().UnixNano(),
		packet.Header.SequenceNumber,
		packet.Header.Timestamp,
		packet.Header.Marker,
		packet.Header.MarshalSize(),
		len(packet.Payload),
		0,
	)
	require.Equal(t, uint64(sequenceNumber-1), flowState.LossStartInclusive)
	require.Equal(t, uint64(sequenceNumber), flowState.LossEndExclusive)
	require.Equal(t, uint64(9), r.packetsLost)
	require.False(t, r.history.IsSet(uint64(sequenceNumber)-1))

	// out-of-order
	sequenceNumber--
	timestamp -= 3000
	packet = getPacket(sequenceNumber, timestamp, 999)
	flowState = r.Update(
		time.Now().UnixNano(),
		packet.Header.SequenceNumber,
		packet.Header.Timestamp,
		packet.Header.Marker,
		packet.Header.MarshalSize(),
		len(packet.Payload),
		0,
	)
	require.Equal(t, uint64(8), r.packetsLost)
	require.Equal(t, uint64(2), r.packetsOutOfOrder)
	require.True(t, r.history.IsSet(uint64(sequenceNumber)))

	// padding only
	sequenceNumber += 2
	timestamp += 3000
	packet = getPacket(sequenceNumber, timestamp, 0)
	flowState = r.Update(
		time.Now().UnixNano(),
		packet.Header.SequenceNumber,
		packet.Header.Timestamp,
		packet.Header.Marker,
		packet.Header.MarshalSize(),
		len(packet.Payload),
		25,
	)
	require.Equal(t, uint64(8), r.packetsLost)
	require.Equal(t, uint64(2), r.packetsOutOfOrder)
	require.True(t, r.history.IsSet(uint64(sequenceNumber)))
	require.True(t, r.history.IsSet(uint64(sequenceNumber)-1))
	require.True(t, r.history.IsSet(uint64(sequenceNumber)-2))

	// old packet, but simulating increasing sequence number after roll over
	packet = getPacket(sequenceNumber+400, timestamp-6000, 300)
	flowState = r.Update(
		time.Now().UnixNano(),
		packet.Header.SequenceNumber,
		packet.Header.Timestamp,
		packet.Header.Marker,
		packet.Header.MarshalSize(),
		len(packet.Payload),
		0,
	)
	require.Equal(t, RTPFlowUnhandledReasonOldSequenceNumber, flowState.UnhandledReason)
	require.Equal(t, sequenceNumber, r.sequenceNumber.GetHighest())
	require.Equal(t, sequenceNumber, uint16(r.sequenceNumber.GetExtendedHighest()))
	require.Equal(t, timestamp, r.timestamp.GetHighest())
	require.Equal(t, timestamp, uint32(r.timestamp.GetExtendedHighest()))

	r.Stop()
}

func Test_RTPStatsReceiver_UpdateFEC(t *testing.T) {
	r := NewRTPStatsReceiver(RTPStatsParams{})
	r.SetClockRate(90000)
	snapshotID := r.NewSnapshotId()

	packet := getPacket(100, 3000, 1000)
	r.Update(
		time.Now().UnixNano(),
		packet.SequenceNumber,
		packet.Timestamp,
		packet.Marker,
		packet.Header.MarshalSize(),
		len(packet.Payload),
		0,
	)
	r.UpdateFEC(3, 900, 1, 2)

	stats := r.ToProto()
	require.EqualValues(t, 3, stats.FecPackets)
	require.EqualValues(t, 900, stats.FecBytes)
	require.EqualValues(t, 1, stats.FecPacketsDiscarded)
	require.EqualValues(t, 2, stats.FecPacketsRecovered)

	delta := r.DeltaInfo(snapshotID)
	require.EqualValues(t, 3, delta.FecPackets)
	require.EqualValues(t, 900, delta.FecBytes)
	require.EqualValues(t, 1, delta.FecPacketsDiscarded)
	require.EqualValues(t, 2, delta.FecPacketsRecovered)

	// FEC-only intervals are reported even if no primary packet arrived.
	r.UpdateFEC(2, 500, 1, 1)
	delta = r.DeltaInfo(snapshotID)
	require.EqualValues(t, 2, delta.FecPackets)
	require.EqualValues(t, 500, delta.FecBytes)
	require.EqualValues(t, 1, delta.FecPacketsDiscarded)
	require.EqualValues(t, 1, delta.FecPacketsRecovered)

	r.Stop()
	r.UpdateFEC(1, 100, 1, 1)
	stats = r.ToProto()
	require.EqualValues(t, 5, stats.FecPackets)
	require.EqualValues(t, 1400, stats.FecBytes)
	require.EqualValues(t, 2, stats.FecPacketsDiscarded)
	require.EqualValues(t, 3, stats.FecPacketsRecovered)
}

func Test_RTPStatsReceiver_ReceptionReportCountsRepairedAsLost(t *testing.T) {
	r := NewRTPStatsReceiver(RTPStatsParams{})
	r.SetClockRate(90000)
	snapshotID := r.NewSnapshotId()

	update := func(sn uint16) RTPFlowState {
		packet := getPacket(sn, 3000*uint32(sn), 1000)
		return r.Update(
			time.Now().UnixNano(),
			packet.SequenceNumber,
			packet.Timestamp,
			packet.Marker,
			packet.Header.MarshalSize(),
			len(packet.Payload),
			0,
		)
	}

	// 101-104 are lost
	update(100)
	for sn := uint16(105); sn < 110; sn++ {
		require.False(t, update(sn).IsLossFilled)
	}
	// 101 and 102 are repaired, 103 is only reordered
	for _, sn := range []uint16{101, 102} {
		require.True(t, update(sn).IsLossFilled)
		r.UpdateRepaired()
	}
	require.True(t, update(103).IsLossFilled)
	// a duplicate does not fill a loss
	require.False(t, update(102).IsLossFilled)
	for sn := uint16(110); sn < 120; sn++ {
		update(sn)
	}

	// 20 packets expected, 1 still lost and 2 repaired
	rr := r.GetRtcpReceptionReport(1234, 0, snapshotID)
	require.NotNil(t, rr)
	require.EqualValues(t, 38, rr.FractionLost) // 3/20 in Q8
	require.EqualValues(t, 3, rr.TotalLost)

	// a late repair does not take loss away from the next interval
	require.True(t, update(104).IsLossFilled)
	r.UpdateRepaired()
	for sn := uint16(120); sn < 130; sn++ {
		update(sn)
	}
	rr = r.GetRtcpReceptionReport(1234, 0, snapshotID)
	require.NotNil(t, rr)
	require.EqualValues(t, 0, rr.FractionLost)
	require.EqualValues(t, 3, rr.TotalLost)

	// the proxied loss still applies when it is higher
	for sn := uint16(130); sn < 140; sn++ {
		update(sn)
	}
	rr = r.GetRtcpReceptionReport(1234, 64, snapshotID)
	require.NotNil(t, rr)
	require.EqualValues(t, 64, rr.FractionLost)
}

func Test_RTPStatsReceiver_Restart(t *testing.T) {
	frame := int64(33 * time.Millisecond)
	// one packet per frame, returns the reason of each packet
	feed := func(r *RTPStatsReceiver, at *int64, sn uint16, ts uint32, n int) []RTPFlowUnhandledReason {
		reasons := make([]RTPFlowUnhandledReason, 0, n)
		for i := 0; i < n; i++ {
			fs := r.Update(*at, sn+uint16(i), ts+uint32(i)*3000, true, 12, 1000, 0)
			reasons = append(reasons, fs.UnhandledReason)
			*at += frame
		}
		return reasons
	}
	newReceiver := func() (*RTPStatsReceiver, *int64) {
		r := NewRTPStatsReceiver(RTPStatsParams{})
		r.SetClockRate(90000)
		t.Cleanup(r.Stop)
		at := time.Now().UnixNano()
		return r, &at
	}
	countRestarts := func(reasons []RTPFlowUnhandledReason) int {
		n := 0
		for _, reason := range reasons {
			if reason == RTPFlowUnhandledReasonRestart {
				n++
			}
		}
		return n
	}

	t.Run("late packets from before the start do not restart", func(t *testing.T) {
		r, at := newReceiver()
		feed(r, at, 7784, 1554058566, 1)
		for sn := uint16(7779); sn < 7784; sn++ {
			fs := r.Update(*at, sn, 1554058566, false, 12, 1000, 0)
			require.Equal(t, RTPFlowUnhandledReasonPreStartSequenceNumber, fs.UnhandledReason)
		}
		require.Zero(t, countRestarts(feed(r, at, 7785, 1554058566+3000, 60)))
		require.Zero(t, r.restartRun.count)
	})

	t.Run("timestamp jump with continuous sequence numbers restarts once", func(t *testing.T) {
		r, at := newReceiver()
		feed(r, at, 3411, 3099713139, 29)
		reasons := feed(r, at, 3440, 1127077245, 90)
		require.Equal(t, 1, countRestarts(reasons))
		// declared only after the run lasted restartMinDuration
		declaredAt := slices.Index(reasons, RTPFlowUnhandledReasonRestart)
		require.GreaterOrEqual(t, int64(declaredAt)*frame, restartMinDuration)
		require.Less(t, int64(declaredAt-1)*frame, restartMinDuration)
		require.True(t, r.restartRun.isDeclared)
		require.Equal(t, 90, r.restartRun.count)
	})

	t.Run("timestamp steps back after the start and moves forward from there", func(t *testing.T) {
		r, at := newReceiver()
		feed(r, at, 100, 500_000_000, 300)
		// sequence numbers continue, timestamp is behind the highest but ahead of the start
		fs := r.Update(*at, 400, 500_000_000+30_000, true, 12, 1000, 0)
		require.Equal(t, RTPFlowUnhandledReasonOldSequenceNumber, fs.UnhandledReason)
		require.Equal(t, 1, countRestarts(feed(r, at, 401, 500_000_000+33_000, 90)))
	})

	t.Run("loss inside the run still restarts", func(t *testing.T) {
		r, at := newReceiver()
		feed(r, at, 3411, 3099713139, 29)
		restarts := 0
		for sn, ts := uint16(3440), uint32(1127077245); sn < 3540; sn, ts = sn+3, ts+9000 {
			restarts += countRestarts(feed(r, at, sn, ts, 2))
		}
		require.Equal(t, 1, restarts)
	})

	t.Run("accepted packet ends the run", func(t *testing.T) {
		r, at := newReceiver()
		feed(r, at, 3411, 3099713139, 29)
		require.Zero(t, countRestarts(feed(r, at, 3440, 1127077245, 20)))
		require.Equal(t, 20, r.restartRun.count)
		// the stream returns to its timeline before the run lasted long enough
		require.Zero(t, countRestarts(feed(r, at, 3460, 3099713139+49*3000, 60)))
		require.Zero(t, r.restartRun.count)
	})

	t.Run("reordered and duplicate packets inside the run still restart", func(t *testing.T) {
		r, at := newReceiver()
		feed(r, at, 3411, 3099713139, 29)
		restarts := 0
		for sn, ts := uint16(3440), uint32(1127077245); sn < 3540; sn, ts = sn+10, ts+30000 {
			restarts += countRestarts(feed(r, at, sn, ts, 10))
			// a duplicate of the last packet and a packet from 3 back
			restarts += countRestarts(feed(r, at, sn+9, ts+9*3000, 1))
			restarts += countRestarts(feed(r, at, sn+6, ts+6*3000, 1))
		}
		require.Equal(t, 1, restarts)
	})

	t.Run("padding burst inside the run still restarts", func(t *testing.T) {
		r, at := newReceiver()
		feed(r, at, 3411, 3099713139, 29)
		restarts := 0
		sn, ts := uint16(3440), uint32(1127077245)
		for i := 0; i < 10; i++ {
			restarts += countRestarts(feed(r, at, sn, ts, 10))
			sn, ts = sn+10, ts+30000
			// padding burst longer than restartMaxSNGap at the timestamp of the last packet
			for j := 0; j < 2*restartMaxSNGap; j++ {
				fs := r.Update(*at, sn, ts-3000, false, 12, 0, 200)
				require.NotEqual(t, RTPFlowUnhandledReasonRestart, fs.UnhandledReason)
				sn++
			}
		}
		require.Equal(t, 1, restarts)
	})

	t.Run("skipped sequence numbers do not end the run", func(t *testing.T) {
		r, at := newReceiver()
		feed(r, at, 3411, 3099713139, 29)
		restarts := 0
		sn, ts := uint16(3440), uint32(1127077245)
		for i := 0; i < 10; i++ {
			// skipped sequence number is reported at the highest timestamp with no payload
			r.Update(*at, sn, r.HighestTimestamp(), false, 0, 0, 0)
			sn++
			restarts += countRestarts(feed(r, at, sn, ts, 10))
			sn, ts = sn+10, ts+30000
		}
		require.Equal(t, 1, restarts)
	})
}

func Test_RTPStatsSender_getIntervalStats(t *testing.T) {
	t.Run("packetsNotFoundMetadata should match lost packets", func(t *testing.T) {
		r := NewRTPStatsSender(RTPStatsParams{}, 1024)
		stats := r.getIntervalStats(0, 10000, 10000)
		require.EqualValues(t, 8977, stats.packetsNotFoundMetadata)
	})
}

func BenchmarkRTPStatsReceiver_Update(b *testing.B) {
	const clockRate = 90000
	const hdrSize = 12
	const payloadSize = 1200
	const paddingSize = 0
	const tsIncrement = 3000 // ~33ms at 90kHz

	r := NewRTPStatsReceiver(RTPStatsParams{})
	r.SetClockRate(clockRate)

	// initialize before running benchmark loop
	packetTime := time.Now().UnixNano()
	sn := uint16(0)
	ts := uint32(0)
	r.Update(packetTime, sn, ts, false, hdrSize, payloadSize, paddingSize)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sn++
		ts += tsIncrement
		packetTime += 33 * int64(time.Millisecond) // ~30fps
		marker := (i+1)%100 == 0                   // marker every 100 packets
		r.Update(packetTime, sn, ts, marker, hdrSize, payloadSize, paddingSize)
	}
}

func Test_RTPStatsSender_ReceiverReportCycles(t *testing.T) {
	newSender := func(startSN uint64, numPackets int) (*RTPStatsSender, uint64) {
		r := NewRTPStatsSender(RTPStatsParams{}, 128)
		r.SetClockRate(90000)
		t.Cleanup(r.Stop)
		at := time.Now().UnixNano()
		for i := 0; i < numPackets; i++ {
			r.Update(at, startSN+uint64(i), uint64(1000+i*3000), true, 12, 1000, 0, false)
			at += int64(time.Millisecond)
		}
		return r, startSN + uint64(numPackets) - 1
	}

	t.Run("cycle count one below the actual", func(t *testing.T) {
		// started at 1169, no roll over yet, remote reports cycles as 0xFFFF
		r, highest := newSender(1169, 2000)
		r.UpdateFromReceiverReport(rtcp.ReceptionReport{LastSequenceNumber: 0xFFFF_0000 | uint32(highest-10)})
		require.Equal(t, highest-10, r.extHighestSNFromRR+(r.extStartSN&0xFFFF_FFFF_FFFF_0000))

		// alignment holds for the next report
		r.UpdateFromReceiverReport(rtcp.ReceptionReport{LastSequenceNumber: 0xFFFF_0000 | uint32(highest)})
		require.Equal(t, highest, r.extHighestSNFromRR)
	})

	t.Run("missed roll over", func(t *testing.T) {
		r, highest := newSender(1169, 70000)
		r.UpdateFromReceiverReport(rtcp.ReceptionReport{LastSequenceNumber: uint32(highest & 0xFFFF)})
		require.Equal(t, highest, r.extHighestSNFromRR)
	})

	t.Run("report ahead of the highest sent is dropped", func(t *testing.T) {
		r, highest := newSender(1169, 2000)
		r.UpdateFromReceiverReport(rtcp.ReceptionReport{LastSequenceNumber: uint32(highest - 10)})
		r.UpdateFromReceiverReport(rtcp.ReceptionReport{LastSequenceNumber: uint32(highest + 3)})
		require.Equal(t, highest-10, r.extHighestSNFromRR)
	})

	t.Run("report ahead of the highest sent is dropped after a roll over", func(t *testing.T) {
		r, highest := newSender(1169, 70000)
		r.UpdateFromReceiverReport(rtcp.ReceptionReport{LastSequenceNumber: uint32(highest+3) & 0xFFFF, Jitter: 50})
		require.Zero(t, r.extHighestSNFromRR)
		require.Zero(t, r.jitterFromRR)
	})

	t.Run("repeated stale report does not replace a newer report", func(t *testing.T) {
		r, _ := newSender(1169, 2000)
		r.UpdateFromReceiverReport(rtcp.ReceptionReport{LastSequenceNumber: 3000, Jitter: 10, TotalLost: 5})
		r.UpdateFromReceiverReport(rtcp.ReceptionReport{LastSequenceNumber: 2900, Jitter: 99, TotalLost: 1})
		r.UpdateFromReceiverReport(rtcp.ReceptionReport{LastSequenceNumber: 2900, Jitter: 99, TotalLost: 1})
		require.EqualValues(t, 3000, r.extHighestSNFromRR)
		require.EqualValues(t, 10, r.jitterFromRR)
		require.EqualValues(t, 5, r.packetsLostFromRR)
	})

	t.Run("report from before the start does not block later reports", func(t *testing.T) {
		// start is above the first cycle, the first report is for a packet before the start
		r, highest := newSender(65540, 101)
		startCycles := uint64(65536)
		r.UpdateFromReceiverReport(rtcp.ReceptionReport{LastSequenceNumber: 0xFFFF_FFFE})
		require.Zero(t, r.extHighestSNFromRR)

		// remote extended sequence number wraps
		r.UpdateFromReceiverReport(rtcp.ReceptionReport{LastSequenceNumber: uint32(highest-10) & 0xFFFF})
		require.Equal(t, highest-10, r.extHighestSNFromRR+startCycles)
		r.UpdateFromReceiverReport(rtcp.ReceptionReport{LastSequenceNumber: uint32(highest) & 0xFFFF})
		require.Equal(t, highest, r.extHighestSNFromRR+startCycles)
	})

	t.Run("report that never changes does not move with the highest sent", func(t *testing.T) {
		r, highest := newSender(1169, 2000)
		r.UpdateFromReceiverReport(rtcp.ReceptionReport{LastSequenceNumber: uint32(highest - 10)})
		at := time.Now().UnixNano()
		for i := uint64(1); i <= 70000; i++ {
			r.Update(at, highest+i, 1000+i*3000, true, 12, 1000, 0, false)
		}
		r.UpdateFromReceiverReport(rtcp.ReceptionReport{LastSequenceNumber: uint32(highest - 10)})
		require.Equal(t, highest-10, r.extHighestSNFromRR)
	})
}

func Test_RTPStatsSender_PublisherClockRate(t *testing.T) {
	const clockRate = 48000
	// feeds publisher reports that are 3 seconds apart and advance at reportRate
	run := func(reportRate int, passThrough bool) []*rtcp.SenderReport {
		r := NewRTPStatsSender(RTPStatsParams{}, 128)
		r.SetClockRate(clockRate)
		t.Cleanup(r.Stop)
		r.Update(time.Now().UnixNano(), 100, 1000, true, 12, 100, 0, false)

		now := mono.UnixNano()
		var reports []*rtcp.SenderReport
		for i := 3; i >= 0; i-- {
			// each report is used as soon as it is received
			reports = append(reports, r.GetRtcpSenderReport(1234, &livekit.RTCPSenderReportState{
				NtpTimestamp:    uint64(mediatransportutil.ToNtpTime(time.Unix(0, now-int64(i)*int64(3*time.Second)))),
				RtpTimestampExt: 500_000 + uint64((3-i)*3*reportRate),
				At:              mono.UnixNano(),
				AtAdjusted:      mono.UnixNano(),
			}, 0, passThrough))
		}
		return reports
	}

	for _, passThrough := range []bool{true, false} {
		// rate is not known on the first report, the report is sent
		muted := run(8000, passThrough)
		require.NotNil(t, muted[0])
		require.Nil(t, muted[1])
		require.Nil(t, muted[2])
		require.Nil(t, muted[3])

		for _, report := range run(clockRate, passThrough) {
			require.NotNil(t, report)
		}
		// publisher time stamp does not advance at all
		stopped := run(0, passThrough)
		require.NotNil(t, stopped[0])
		require.Nil(t, stopped[1])
		require.Nil(t, stopped[3])
		// off by less than the factor
		for _, report := range run(clockRate*3/4, passThrough) {
			require.NotNil(t, report)
		}
		// a high rate is not skipped, the pair of reports across an unmute reads high
		for _, report := range run(clockRate*3, passThrough) {
			require.NotNil(t, report)
		}
	}

	// feeds one publisher report, ntpOffset is relative to a fixed base
	base := mono.UnixNano()
	newSender := func() *RTPStatsSender {
		r := NewRTPStatsSender(RTPStatsParams{}, 128)
		r.SetClockRate(clockRate)
		t.Cleanup(r.Stop)
		r.Update(time.Now().UnixNano(), 100, 1000, true, 12, 100, 0, false)
		return r
	}
	feed := func(r *RTPStatsSender, ntpOffset time.Duration, rtpExt uint64, tsOffset uint64) *rtcp.SenderReport {
		return r.GetRtcpSenderReport(1234, &livekit.RTCPSenderReportState{
			NtpTimestamp:    uint64(mediatransportutil.ToNtpTime(time.Unix(0, base+int64(ntpOffset)))),
			RtpTimestampExt: rtpExt,
			At:              mono.UnixNano(),
			AtAdjusted:      mono.UnixNano(),
		}, tsOffset, false)
	}

	t.Run("reports close together do not give a rate", func(t *testing.T) {
		r := newSender()
		require.NotNil(t, feed(r, 0, 500_000, 0))
		// 10 ms later with a mapping error of one video frame, rate would be far above the factor
		require.NotNil(t, feed(r, 10*time.Millisecond, 500_000+480+3000, 0))
		require.False(t, r.isPublisherClockRateKnown)
		// measured against the first report once far enough from it
		require.NotNil(t, feed(r, 3*time.Second, 500_000+3*clockRate, 0))
		require.True(t, r.isPublisherClockRateKnown)
	})

	t.Run("older report starts a new measurement", func(t *testing.T) {
		r := newSender()
		require.NotNil(t, feed(r, 10*time.Second, 500_000, 0))
		require.Nil(t, feed(r, 13*time.Second, 500_000+3*8000, 0))
		// rate is not known on the older report, the report is sent
		require.NotNil(t, feed(r, 0, 2_000_000, 0))
		require.False(t, r.isPublisherClockRateKnown)
		require.NotNil(t, feed(r, 3*time.Second, 2_000_000+3*clockRate, 0))
	})

	t.Run("reports from sources with different timestamp bases", func(t *testing.T) {
		r := newSender()
		// both sources map to the same output timeline through their offsets
		require.NotNil(t, feed(r, 0, 500_000, 400_000))
		require.NotNil(t, feed(r, 3*time.Second, 3_000_000_000, 3_000_000_000-100_000-3*clockRate))
		require.True(t, r.isPublisherClockRateKnown)
		require.InDelta(t, clockRate, r.publisherClockRate, 1)
	})
}

func BenchmarkRTPStatsSender_Update(b *testing.B) {
	r := NewRTPStatsSender(RTPStatsParams{}, 4096)
	r.SetClockRate(90000)
	packetTime := time.Now().UnixNano()
	esn := uint64(1000)
	ets := uint64(100000)
	r.Update(packetTime, esn, ets, false, 12, 1200, 0, false)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		esn++
		if i%5 == 0 {
			ets += 3000
			packetTime += 33 * int64(time.Millisecond)
		}
		r.Update(packetTime, esn, ets, i%5 == 4, 12, 1200, 0, false)
	}
}

// 5 packets per frame, like video
func BenchmarkRTPStatsReceiver_UpdateVideo(b *testing.B) {
	r := NewRTPStatsReceiver(RTPStatsParams{})
	r.SetClockRate(90000)
	packetTime := time.Now().UnixNano()
	sn := uint16(0)
	ts := uint32(0)
	r.Update(packetTime, sn, ts, false, 12, 1200, 0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sn++
		if i%5 == 0 {
			ts += 3000
			packetTime += 33 * int64(time.Millisecond)
		}
		r.Update(packetTime, sn, ts, i%5 == 4, 12, 1200, 0)
	}
}

// many streams round robin, the state of a stream is not in cache when its packet comes
func BenchmarkRTPStatsReceiver_UpdateManyStreams(b *testing.B) {
	const n = 20000
	rs := make([]*RTPStatsReceiver, n)
	packetTime := time.Now().UnixNano()
	for i := range rs {
		rs[i] = NewRTPStatsReceiver(RTPStatsParams{})
		rs[i].SetClockRate(90000)
		rs[i].Update(packetTime, 0, 0, false, 12, 1200, 0)
	}
	sn := uint16(0)
	ts := uint32(0)
	rand.New(rand.NewSource(1)).Shuffle(n, func(i, j int) { rs[i], rs[j] = rs[j], rs[i] })
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%n == 0 {
			sn++
			ts += 3000
			packetTime += 33 * int64(time.Millisecond)
		}
		rs[i%n].Update(packetTime, sn, ts, true, 12, 1200, 0)
	}
}

func BenchmarkRTPStatsSender_UpdateManyStreams(b *testing.B) {
	const n = 20000
	rs := make([]*RTPStatsSender, n)
	packetTime := time.Now().UnixNano()
	for i := range rs {
		rs[i] = NewRTPStatsSender(RTPStatsParams{}, 4096)
		rs[i].SetClockRate(90000)
		rs[i].Update(packetTime, 1000, 100000, false, 12, 1200, 0, false)
	}
	esn := uint64(1000)
	ets := uint64(100000)
	rand.New(rand.NewSource(1)).Shuffle(n, func(i, j int) { rs[i], rs[j] = rs[j], rs[i] })
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%n == 0 {
			esn++
			ets += 3000
			packetTime += 33 * int64(time.Millisecond)
		}
		rs[i%n].Update(packetTime, esn, ets, true, 12, 1200, 0, false)
	}
}

func Test_RTPStatsSender_ReceiverReportTotalLost(t *testing.T) {
	r := NewRTPStatsSender(RTPStatsParams{}, 1024)
	r.SetClockRate(90000)
	t.Cleanup(r.Stop)
	snapshotID := r.NewSenderSnapshotId()

	esn := uint64(1000)
	at := time.Now().UnixNano()
	send := func(numPackets int) {
		for i := 0; i < numPackets; i++ {
			esn++
			r.Update(at, esn, esn*3000, true, 12, 1000, 0, false)
			at += int64(time.Millisecond)
		}
	}
	report := func(totalLost uint32) *RTPDeltaInfo {
		r.UpdateFromReceiverReport(rtcp.ReceptionReport{LastSequenceNumber: uint32(esn), TotalLost: totalLost})
		_, receiverView := r.DeltaInfoSender(snapshotID)
		return receiverView
	}

	send(100)
	require.EqualValues(t, 3, report(3).PacketsLost)

	send(100)
	require.EqualValues(t, 7, report(10).PacketsLost)

	// a remote that received duplicates reports the cumulative loss as -1, that is not a loss
	send(100)
	require.EqualValues(t, 0, report(0xFF_FFFF).PacketsLost)
	require.Zero(t, r.packetsLostFromRR)

	send(100)
	require.EqualValues(t, 4, report(4).PacketsLost)
}

func Test_RTPStatsSender_ProcessedReceptionReportsLog(t *testing.T) {
	for _, numReports := range []int{3, cMaxProcessedReceptionReports + 6} {
		r := NewRTPStatsSender(RTPStatsParams{}, 1024)
		r.SetClockRate(90000)
		snapshotID := r.NewSenderSnapshotId()

		at := time.Now().UnixNano()
		for i := 1; i <= numReports; i++ {
			r.Update(at, uint64(1000+i), uint64(i*3000), true, 12, 1000, 0, false)
			r.UpdateFromReceiverReport(rtcp.ReceptionReport{LastSequenceNumber: uint32(1000 + i), TotalLost: uint32(i)})
		}

		// reports are logged oldest first
		enc := zapcore.NewMapObjectEncoder()
		view := &r.senderSnapshots[snapshotID-cFirstSnapshotID].receiverView
		require.NoError(t, wrappedReceptionReportsLogger{view}.MarshalLogObject(enc))
		numLogged := min(numReports, cMaxProcessedReceptionReports)
		require.Len(t, enc.Fields, numLogged)
		for i := 0; i < numLogged; i++ {
			rr := enc.Fields[fmt.Sprintf("%d", i)].(rtcp.ReceptionReport)
			require.EqualValues(t, numReports-numLogged+i+1, rr.TotalLost)
		}
		r.Stop()
	}
}

func Test_RTPStatsSender_Seed(t *testing.T) {
	newSender := func(startSN uint64) *RTPStatsSender {
		r := NewRTPStatsSender(RTPStatsParams{}, 128)
		r.SetClockRate(90000)
		at := time.Now().UnixNano()
		for i := range 10 {
			r.Update(at, startSN+uint64(i), uint64(1000+i*3000), true, 12, 1000, 0, false)
			at += int64(time.Millisecond)
		}
		return r
	}

	t.Run("copies state", func(t *testing.T) {
		from := newSender(100)
		from.UpdateFEC(2, 300, 0, 0)
		r := NewRTPStatsSender(RTPStatsParams{}, 128)
		r.Seed(from)
		require.Equal(t, from.extStartSN, r.extStartSN)
		require.Equal(t, from.extHighestSN, r.extHighestSN)
		require.Equal(t, from.bytes, r.bytes)
		require.EqualValues(t, 2, r.fecPackets)
		require.Equal(t, from.snInfos, r.snInfos)
		require.NotSame(t, &from.snInfos[0], &r.snInfos[0], "do not share the packet cache")

		// an initialized sender is not seeded again
		other := newSender(5000)
		r.Seed(other)
		require.Equal(t, from.extStartSN, r.extStartSN)
	})

	t.Run("mutual seeding does not deadlock", func(t *testing.T) {
		done := make(chan struct{})
		go func() {
			defer close(done)
			a, b := newSender(100), newSender(200)
			var wg sync.WaitGroup
			wg.Go(func() {
				for range 100_000 {
					b.Seed(a)
				}
			})
			for range 100_000 {
				a.Seed(b)
			}
			wg.Wait()
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("senders seeding from each other deadlocked")
		}
	})
}

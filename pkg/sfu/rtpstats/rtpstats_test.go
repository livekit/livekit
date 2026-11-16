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
	"math/rand"
	"slices"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"

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
	}
}

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
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
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

func Test_RTPStatsReceiver_Restart(t *testing.T) {
	clockRate := uint32(90000)
	r := NewRTPStatsReceiver(RTPStatsParams{})
	r.SetClockRate(clockRate)

	// should not restart till there are at least threshold packets
	require.False(t, r.maybeRestart(10, 20, 1000))
	require.False(t, r.maybeRestart(11, 20, 1000))
	require.False(t, r.maybeRestart(13, 20, 1000))
	require.False(t, r.maybeRestart(14, 20, 1000))
	// although adding 5th packet should have enough packets for a check,
	// still should not restart as there is a sequence number gap between 11 and 13
	require.False(t, r.maybeRestart(15, 20, 1000))
	require.False(t, r.maybeRestart(16, 19, 1000))
	// has enough packets, but still cannot restart because timestamps are not increasing
	require.False(t, r.maybeRestart(17, 21, 1000))
	require.False(t, r.maybeRestart(18, 21, 1000))
	require.False(t, r.maybeRestart(19, 21, 1000))
	// can restart as there are enough packets with proper sequencing
	require.True(t, r.maybeRestart(20, 21, 1000))
	require.Equal(t, restartThreshold, r.restartPacketsN)

	r.resetRestart()
	require.Zero(t, r.restartPacketsN)

	r.Stop()
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

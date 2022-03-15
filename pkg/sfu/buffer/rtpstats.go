package buffer

import (
	"fmt"
	"math/bits"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	GapHistogramNumBins = 101
	SequenceNumberMin   = uint16(0)
	SequenceNumberMax   = uint16(65535)
	NumSequenceNumbers  = 65536
)

func getPos(sn uint16) (uint16, uint16) {
	return sn >> 6, sn & 0x3f
}

type RTPFlowState struct {
	IsHighestSN        bool
	HasLoss            bool
	LossStartInclusive uint16
	LossEndExclusive   uint16
}

type RTPStatsParams struct {
	ClockRate uint32
}

type RTPStats struct {
	params RTPStatsParams

	lock sync.RWMutex

	initialized bool

	startTime time.Time
	endTime   time.Time

	extStartSN uint32
	highestSN  uint16
	cycles     uint16

	highestTS   uint32
	highestTime int64

	lastTransit uint32

	bytes            uint64
	bytesDuplicate   uint64
	bytesPadding     uint64
	packetsDuplicate uint32
	packetsPadding   uint32

	packetsOutOfOrder uint32

	packetsLost             uint32
	isPacketsLostOverridden bool
	packetsLostOverridden   uint32

	frames uint32

	jitter              float64
	maxJitter           float64
	isJitterOverridden  bool
	jitterOverridden    float64
	maxJitterOverridden float64

	seenSNs      [NumSequenceNumbers / 64]uint64
	gapHistogram [GapHistogramNumBins]uint32

	nacks      uint32
	nackMisses uint32

	plis    uint32
	lastPli time.Time

	firs    uint32
	lastFir time.Time

	rtt    uint32
	maxRtt uint32

	rtpSR     uint32
	ntpSR     NtpTime
	arrivalSR int64
}

func NewRTPStats(params RTPStatsParams) *RTPStats {
	return &RTPStats{
		params: params,
	}
}

func (r *RTPStats) Stop() {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.endTime = time.Now()
}

func (r *RTPStats) IsActive() bool {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.initialized && r.endTime.IsZero()
}

func (r *RTPStats) Update(rtph *rtp.Header, payloadSize int, paddingSize int, packetTime int64) (flowState RTPFlowState) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	if !r.initialized {
		r.initialized = true

		r.startTime = time.Now()

		r.highestSN = rtph.SequenceNumber - 1
		r.highestTS = rtph.Timestamp
		r.highestTime = packetTime

		r.extStartSN = uint32(rtph.SequenceNumber)
		r.cycles = 0
	}

	pktSize := uint64(rtph.MarshalSize() + payloadSize + paddingSize)
	if payloadSize == 0 {
		r.packetsPadding++
		r.bytesPadding += pktSize
	} else {
		r.bytes += pktSize
	}

	isDuplicate := false
	diff := rtph.SequenceNumber - r.highestSN
	switch {
	// duplicate or out-of-order
	case diff == 0 || diff > (1<<15):
		if diff != 0 {
			r.packetsOutOfOrder++
		}

		// adjust start to account for out-of-order packets before o cycle completes
		if !r.isCycleCompleted() && (rtph.SequenceNumber-uint16(r.extStartSN) > (1 << 15)) {
			// NOTE: current sequence number is counted as loss as it will be deducted in the duplicate check below
			r.packetsLost += uint32(uint16(r.extStartSN) - rtph.SequenceNumber)
			r.extStartSN = uint32(rtph.SequenceNumber)
		}

		if r.isSeenSN(rtph.SequenceNumber) {
			r.bytesDuplicate += pktSize
			r.packetsDuplicate++
			isDuplicate = true
		} else {
			r.packetsLost--
		}

	// in-order
	default:
		flowState.IsHighestSN = true
		if diff > 1 {
			flowState.HasLoss = true
			flowState.LossStartInclusive = r.highestSN + 1
			flowState.LossEndExclusive = rtph.SequenceNumber
		}

		// update gap histogram
		r.updateGapHistogram(int(diff))

		// update missing sequence numbers
		for lost := r.highestSN + 1; lost != rtph.SequenceNumber; lost++ {
			r.clearSeenSN(lost)
		}
		r.packetsLost += uint32(diff - 1)

		if rtph.SequenceNumber < r.highestSN {
			r.cycles++
		}
		r.highestSN = rtph.SequenceNumber
		r.highestTS = rtph.Timestamp
		r.highestTime = packetTime

		if rtph.Marker {
			r.frames++
		}
	}

	// set current sequence number in seen list
	r.setSeenSN(rtph.SequenceNumber)

	if !isDuplicate {
		r.updateJitter(rtph, packetTime)
	}

	return
}

func (r *RTPStats) GetTotalPackets() uint32 {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.getNumPacketsSeen() + r.packetsDuplicate + r.packetsPadding
}

func (r *RTPStats) GetTotalPacketsSansDuplicate() uint32 {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.getNumPacketsSeen() + r.packetsPadding
}

func (r *RTPStats) GetTotalBytes() uint64 {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.bytes + r.bytesDuplicate + r.bytesPadding
}

func (r *RTPStats) GetTotalBytesSansDuplicate() uint64 {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.bytes + r.bytesPadding
}

func (r *RTPStats) UpdatePacketsLost(packetsLost uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.isPacketsLostOverridden = true
	r.packetsLostOverridden = packetsLost
}

func (r *RTPStats) UpdateJitter(jitter float64) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.isJitterOverridden = true
	r.jitterOverridden = jitter
	if jitter > r.maxJitterOverridden {
		r.maxJitterOverridden = jitter
	}
}

func (r *RTPStats) UpdateNack(nackCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.nacks += nackCount
}

func (r *RTPStats) UpdateNackMiss(nackMissCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.nackMisses += nackMissCount
}

func (r *RTPStats) UpdatePliAndTime(pliCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.updatePliLocked(pliCount)
	r.updatePliTimeLocked()
}

func (r *RTPStats) UpdatePli(pliCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.updatePliLocked(pliCount)
}

func (r *RTPStats) updatePliLocked(pliCount uint32) {
	r.plis += pliCount
}

func (r *RTPStats) UpdatePliTime() {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.updatePliTimeLocked()
}

func (r *RTPStats) updatePliTimeLocked() {
	r.lastPli = time.Now()
}

func (r *RTPStats) LastPli() time.Time {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.lastPli
}

func (r *RTPStats) TimeSinceLastPli() int64 {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return time.Now().UnixNano() - r.lastPli.UnixNano()
}

func (r *RTPStats) UpdateFir(firCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.firs += firCount
}

func (r *RTPStats) UpdateFirTime() {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.lastFir = time.Now()
}

func (r *RTPStats) UpdateRtt(rtt uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.rtt = rtt
	if r.rtt > r.maxRtt {
		r.maxRtt = r.rtt
	}
}

func (r *RTPStats) GetRtt() uint32 {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.rtt
}

func (r *RTPStats) SetRtcpSenderReportData(rtpTS uint32, ntpTS NtpTime, arrival time.Time) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.rtpSR = rtpTS
	r.ntpSR = ntpTS
	r.arrivalSR = arrival.UnixNano()
}

// RAJA-REMOVE-START
func (r *RTPStats) GetRtcpSenderReportData() (rtpTS uint32, ntpTS NtpTime, arrivalTime int64) {
	r.lock.RLock()
	defer r.lock.RUnlock()

	now := time.Now()

	ntpSR := r.highestTime
	if r.ntpSR != 0 {
		ntpSR = r.ntpSR.Time().UnixNano()
	}

	rtpTS = r.highestTS
	if r.rtpSR != 0 {
		rtpTS = r.rtpSR
	}
	rtpTS += uint32((now.UnixNano() - ntpSR) * int64(r.params.ClockRate) / 1e9)

	ntpTS = ToNtpTime(now)

	arrivalTime = r.arrivalSR
	return
}

// RAJA-REMOVE-END

func (r *RTPStats) GetRtcpSenderReport(ssrc uint32) *rtcp.SenderReport {
	r.lock.RLock()
	defer r.lock.RUnlock()

	if !r.initialized {
		return nil
	}

	now := time.Now()
	nowNTP := ToNtpTime(now)
	nowRTP := r.highestTS + uint32((now.UnixNano()-r.highestTime)*int64(r.params.ClockRate)/1e9)

	return &rtcp.SenderReport{
		SSRC:        ssrc,
		NTPTime:     uint64(nowNTP),
		RTPTime:     nowRTP,
		PacketCount: r.getNumPacketsSeen() + r.packetsDuplicate + r.packetsPadding,
		OctetCount:  uint32(r.bytes + r.bytesDuplicate + r.bytesPadding),
	}
}

func (r *RTPStats) GetRtcpReceptionReport(ssrc uint32, proxyFracLost uint8) *rtcp.ReceptionReport {
	// RAJA-TODO-START
	// 1. Have to use snapshot or from beginning
	// 2. Set up next snapshot
	// RAJA-TODO-END
	r.lock.RLock()
	defer r.lock.RUnlock()

	if !r.initialized {
		return nil
	}

	extHighestSN := r.getExtHighestSN()
	packetsExpected := extHighestSN - r.extStartSN + 1
	if packetsExpected > NumSequenceNumbers {
		logger.Warnw(
			"too many packets expected in receiver report",
			fmt.Errorf("start: %d, end: %d, expected: %d", r.extStartSN, extHighestSN, packetsExpected),
		)
		return nil
	}
	if packetsExpected == 0 {
		return nil
	}

	packetsLost := r.numMissingSNs(uint16(r.extStartSN), uint16(extHighestSN))
	lossRate := float32(packetsLost) / float32(packetsExpected)
	fracLost := uint8(lossRate * 256.0)
	if proxyFracLost > fracLost {
		fracLost = proxyFracLost
	}

	var dlsr uint32
	if r.arrivalSR != 0 {
		delayMS := uint32((time.Now().UnixNano() - r.arrivalSR) / 1e6)
		dlsr = (delayMS / 1e3) << 16
		dlsr |= (delayMS % 1e3) * 65536 / 1000
	}

	return &rtcp.ReceptionReport{
		SSRC:               ssrc,
		FractionLost:       fracLost,
		TotalLost:          r.packetsLost,
		LastSequenceNumber: extHighestSN,
		Jitter:             uint32(r.jitter),
		LastSenderReport:   uint32(r.ntpSR >> 16),
		Delay:              dlsr,
	}
}

func (r *RTPStats) ToString() string {
	p := r.ToProto()
	if p == nil {
		return ""
	}

	expectedPackets := p.Packets + p.PacketsLost
	expectedPacketRate := float64(expectedPackets) / p.Duration

	str := fmt.Sprintf("t: %+v|%+v|%.2fs", p.StartTime.AsTime().Format(time.UnixDate), p.EndTime.AsTime().Format(time.UnixDate), p.Duration)

	str += fmt.Sprintf(", ep: %d|%.2f/s", expectedPackets, expectedPacketRate)
	str += fmt.Sprintf(", p: %d|%.2f/s", p.Packets, p.PacketRate)
	str += fmt.Sprintf(", l: %d|%.1f/s|%.2f%%", p.PacketsLost, p.PacketLossRate, p.PacketLossPercentage)
	str += fmt.Sprintf(", b: %d|%.1fbps", p.Bytes, p.Bitrate)
	str += fmt.Sprintf(", f: %d|%.1f/s", p.Frames, p.FrameRate)

	str += fmt.Sprintf(", d: %d|%.2f/s", p.PacketsDuplicate, p.PacketDuplicateRate)
	str += fmt.Sprintf(", bd: %d|%.1fbps", p.BytesDuplicate, p.BitrateDuplicate)

	str += fmt.Sprintf(", pp: %d|%.2f/s", p.PacketsPadding, p.PacketPaddingRate)
	str += fmt.Sprintf(", bp: %d|%.1fbps", p.BytesPadding, p.BitratePadding)

	str += fmt.Sprintf(", o: %d", p.PacketsOutOfOrder)

	jitter := r.jitter
	maxJitter := r.maxJitter
	if r.isJitterOverridden {
		jitter = r.jitterOverridden
		maxJitter = r.maxJitterOverridden
	}
	str += fmt.Sprintf(", c: %d, j: %d(%.1fus)|%d(%.1fus)", r.params.ClockRate, uint32(jitter), p.JitterCurrent, uint32(maxJitter), p.JitterMax)

	if len(p.GapHistogram) != 0 {
		first := true
		str += ", gh:["
		for burst, count := range p.GapHistogram {
			if !first {
				str += ", "
			}
			first = false
			str += fmt.Sprintf("%d:%d", burst, count)
		}
		str += "]"
	}

	str += ", n:"
	str += fmt.Sprintf("%d|%d", p.Nacks, p.NackMisses)

	str += ", pli:"
	str += fmt.Sprintf("%d|%+v", p.Plis, p.LastPli.AsTime().Format(time.UnixDate))

	str += ", fir:"
	str += fmt.Sprintf("%d|%+v", p.Firs, p.LastFir.AsTime().Format(time.UnixDate))

	str += ", rtt(ms):"
	str += fmt.Sprintf("%d|%d", p.RttCurrent, p.RttMax)

	return str
}

func (r *RTPStats) ToProto() *livekit.RTPStats {
	r.lock.RLock()
	defer r.lock.RUnlock()

	if r.startTime.IsZero() {
		return nil
	}

	endTime := r.endTime
	if endTime.IsZero() {
		endTime = time.Now()
	}
	elapsed := endTime.Sub(r.startTime).Seconds()
	if elapsed == 0.0 {
		return nil
	}

	packetsExpected := r.getExtHighestSN() - r.extStartSN

	packets := packetsExpected
	if r.packetsLost < packets {
		packets -= r.packetsLost
	}
	packetRate := float64(packets) / elapsed
	bitrate := float64(r.bytes) * 8.0 / elapsed

	frameRate := float64(r.frames) / elapsed

	packetsLost := r.packetsLost
	if r.isPacketsLostOverridden {
		packetsLost = r.packetsLostOverridden
	}
	packetLostRate := float64(packetsLost) / elapsed
	packetLostPercentage := float32(packetsLost) / float32(packetsExpected) * 100.0

	packetDuplicateRate := float64(r.packetsDuplicate) / elapsed
	bitrateDuplicate := float64(r.bytesDuplicate) * 8.0 / elapsed

	packetPaddingRate := float64(r.packetsPadding) / elapsed
	bitratePadding := float64(r.bytesPadding) * 8.0 / elapsed

	jitter := r.jitter
	maxJitter := r.maxJitter
	if r.isJitterOverridden {
		jitter = r.jitterOverridden
		maxJitter = r.maxJitterOverridden
	}
	jitterTime := jitter / float64(r.params.ClockRate) * 1e6
	maxJitterTime := maxJitter / float64(r.params.ClockRate) * 1e6

	p := &livekit.RTPStats{
		StartTime:            timestamppb.New(r.startTime),
		EndTime:              timestamppb.New(endTime),
		Duration:             elapsed,
		Packets:              r.getNumPacketsSeen(),
		PacketRate:           packetRate,
		Bytes:                r.bytes,
		Bitrate:              bitrate,
		PacketsLost:          r.packetsLost,
		PacketLossRate:       packetLostRate,
		PacketLossPercentage: packetLostPercentage,
		PacketsDuplicate:     r.packetsDuplicate,
		PacketDuplicateRate:  packetDuplicateRate,
		BytesDuplicate:       r.bytesDuplicate,
		BitrateDuplicate:     bitrateDuplicate,
		PacketsPadding:       r.packetsPadding,
		PacketPaddingRate:    packetPaddingRate,
		BytesPadding:         r.bytesPadding,
		BitratePadding:       bitratePadding,
		PacketsOutOfOrder:    r.packetsOutOfOrder,
		Frames:               r.frames,
		FrameRate:            frameRate,
		JitterCurrent:        jitterTime,
		JitterMax:            maxJitterTime,
		Nacks:                r.nacks,
		NackMisses:           r.nackMisses,
		Plis:                 r.plis,
		LastPli:              timestamppb.New(r.lastPli),
		Firs:                 r.firs,
		LastFir:              timestamppb.New(r.lastFir),
		RttCurrent:           r.rtt,
		RttMax:               r.maxRtt,
	}

	gapsPresent := false
	for i := 0; i < len(r.gapHistogram); i++ {
		if r.gapHistogram[i] == 0 {
			continue
		}

		gapsPresent = true
		break
	}

	if gapsPresent {
		p.GapHistogram = make(map[int32]uint32, GapHistogramNumBins)
		for i := 0; i < len(r.gapHistogram); i++ {
			if r.gapHistogram[i] == 0 {
				continue
			}

			p.GapHistogram[int32(i+1)] = r.gapHistogram[i]
		}
	}

	return p
}

func (r *RTPStats) getExtHighestSN() uint32 {
	return (uint32(r.cycles) << 16) | uint32(r.highestSN)
}

func (r *RTPStats) isCycleCompleted() bool {
	return (r.getExtHighestSN() - r.extStartSN) >= NumSequenceNumbers
}

func (r *RTPStats) getNumPacketsSeen() uint32 {
	packetsExpected := r.getExtHighestSN() - r.extStartSN + 1
	if r.packetsLost > packetsExpected {
		// should not happen
		return 0
	}

	return packetsExpected - r.packetsLost
}

func (r *RTPStats) setSeenSN(sn uint16) {
	idx, rem := getPos(sn)
	r.seenSNs[idx] |= (1 << rem)
}

func (r *RTPStats) clearSeenSN(sn uint16) {
	idx, rem := getPos(sn)
	r.seenSNs[idx] &^= (1 << rem)
}

func (r *RTPStats) isSeenSN(sn uint16) bool {
	idx, rem := getPos(sn)
	return (r.seenSNs[idx] & (1 << rem)) != 0
}

func (r *RTPStats) numMissingSNs(startInclusive uint16, endInclusive uint16) uint32 {
	startIdx, startRem := getPos(startInclusive)
	endIdx, endRem := getPos(endInclusive + 1)

	seen := uint32(0)
	idx := startIdx
	for idx != endIdx+1 {
		mask := uint64((1 << 64) - 1)
		if idx == startIdx {
			mask &^= uint64((1 << startRem) - 1)
		}
		if idx == endIdx {
			mask &= uint64((1 << endRem) - 1)
		}

		seen += uint32(bits.OnesCount64(r.seenSNs[idx] & mask))

		idx++
		if idx == uint16(len(r.seenSNs)) {
			idx = 0
		}
	}

	return uint32(endInclusive-startInclusive+1) - seen
}

func (r *RTPStats) updateJitter(rtph *rtp.Header, packetTime int64) {
	packetTimeRTP := uint32(packetTime / 1e6 * int64(r.params.ClockRate/1e3))
	transit := packetTimeRTP - rtph.Timestamp

	if r.lastTransit != 0 {
		d := int32(transit - r.lastTransit)
		if d < 0 {
			d = -d
		}
		r.jitter += (float64(d) - r.jitter) / 16
		if r.jitter > r.maxJitter {
			r.maxJitter = r.jitter
		}
	}

	r.lastTransit = transit
}

func (r *RTPStats) updateGapHistogram(gap int) {
	if gap < 2 {
		return
	}

	missing := gap - 1
	if missing > len(r.gapHistogram) {
		r.gapHistogram[len(r.gapHistogram)-1]++
	} else {
		r.gapHistogram[missing-1]++
	}
}

// ----------------------------------

func AggregateRTPStats(statses []*livekit.RTPStats) *livekit.RTPStats {
	startTime := time.Time{}
	endTime := time.Time{}

	packets := uint32(0)
	bytes := uint64(0)
	packetsLost := uint32(0)
	packetsDuplicate := uint32(0)
	bytesDuplicate := uint64(0)
	packetsPadding := uint32(0)
	bytesPadding := uint64(0)
	packetsOutOfOrder := uint32(0)
	frames := uint32(0)
	jitter := float64(0.0)
	maxJitter := float64(0)
	gapHistogram := make(map[int32]uint32, GapHistogramNumBins)
	nacks := uint32(0)
	nackMisses := uint32(0)
	plis := uint32(0)
	lastPli := time.Time{}
	firs := uint32(0)
	lastFir := time.Time{}
	rtt := uint32(0)
	maxRtt := uint32(0)

	for _, stats := range statses {
		if startTime.IsZero() || startTime.After(stats.StartTime.AsTime()) {
			startTime = stats.StartTime.AsTime()
		}

		if endTime.IsZero() || endTime.Before(stats.EndTime.AsTime()) {
			endTime = stats.EndTime.AsTime()
		}

		packets += stats.Packets
		bytes += stats.Bytes

		packetsLost += stats.PacketsLost

		packetsDuplicate += stats.PacketsDuplicate
		bytesDuplicate += stats.BytesDuplicate

		packetsPadding += stats.PacketsPadding
		bytesPadding += stats.BytesPadding

		packetsOutOfOrder += stats.PacketsOutOfOrder

		frames += stats.Frames

		jitter += stats.JitterCurrent
		if stats.JitterMax > maxJitter {
			maxJitter = stats.JitterMax
		}

		for burst, count := range stats.GapHistogram {
			gapHistogram[burst] += count
		}

		nacks += stats.Nacks
		nackMisses += stats.NackMisses

		plis += stats.Plis
		if lastPli.IsZero() || lastPli.Before(stats.LastPli.AsTime()) {
			lastPli = stats.LastPli.AsTime()
		}

		firs += stats.Firs
		if lastFir.IsZero() || lastPli.Before(stats.LastFir.AsTime()) {
			lastFir = stats.LastFir.AsTime()
		}

		rtt += stats.RttCurrent
		if stats.RttMax > maxRtt {
			maxRtt = stats.RttMax
		}
	}

	if endTime.IsZero() {
		endTime = time.Now()
	}
	elapsed := endTime.Sub(startTime).Seconds()

	packetLostRate := float64(packetsLost) / elapsed
	packetLostPercentage := float32(packetsLost) / (float32(packets) + float32(packetsLost)) * 100.0

	packetRate := float64(packets) / elapsed
	packetDuplicateRate := float64(packetsDuplicate) / elapsed
	packetPaddingRate := float64(packetsPadding) / elapsed

	bitrate := float64(bytes) * 8.0 / elapsed
	bitrateDuplicate := float64(bytesDuplicate) * 8.0 / elapsed
	bitratePadding := float64(bytesPadding) * 8.0 / elapsed

	frameRate := float64(frames) / elapsed

	return &livekit.RTPStats{
		StartTime:            timestamppb.New(startTime),
		EndTime:              timestamppb.New(endTime),
		Duration:             elapsed,
		Packets:              packets,
		PacketRate:           packetRate,
		Bytes:                bytes,
		Bitrate:              bitrate,
		PacketsLost:          packetsLost,
		PacketLossRate:       packetLostRate,
		PacketLossPercentage: packetLostPercentage,
		PacketsDuplicate:     packetsDuplicate,
		PacketDuplicateRate:  packetDuplicateRate,
		BytesDuplicate:       bytesDuplicate,
		BitrateDuplicate:     bitrateDuplicate,
		PacketsPadding:       packetsPadding,
		PacketPaddingRate:    packetPaddingRate,
		BytesPadding:         bytesPadding,
		BitratePadding:       bitratePadding,
		PacketsOutOfOrder:    packetsOutOfOrder,
		Frames:               frames,
		FrameRate:            frameRate,
		JitterCurrent:        jitter / float64(len(statses)),
		JitterMax:            maxJitter,
		GapHistogram:         gapHistogram,
		Nacks:                nacks,
		NackMisses:           nackMisses,
		Plis:                 plis,
		LastPli:              timestamppb.New(lastPli),
		Firs:                 firs,
		LastFir:              timestamppb.New(lastFir),
		RttCurrent:           rtt / uint32(len(statses)),
		RttMax:               maxRtt,
	}
}

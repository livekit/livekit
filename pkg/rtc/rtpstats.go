package rtc

import (
	"fmt"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/pion/rtp"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	GapHistogramNumBins = 101
)

func getPos(sn uint16) (uint16, uint16) {
	return sn >> 6, sn & 0x3f
}

type RTPStatsParams struct {
	ClockRate uint32
}

type RTPStats struct {
	params RTPStatsParams

	lock sync.RWMutex

	stats Stats
}

func NewRTPStats(params RTPStatsParams) *RTPStats {
	return &RTPStats{
		params: params,
	}
}

func (r *RTPStats) Stop() {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.stats.Close(time.Now())
}

func (r *RTPStats) Update(rtp *rtp.Packet, packetTime int64) (bool, bool, uint16, uint16) {
	r.lock.Lock()
	defer r.lock.Unlock()

	return r.stats.Update(time.Now(), rtp, packetTime)
}

func (r *RTPStats) UpdateNack(nackCount int, nackMissCount int) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.stats.UpdateNack(nackCount, nackMissCount)
}

func (r *RTPStats) UpdatePli() {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.stats.UpdatePli(time.Now())
}

func (r *RTPStats) TimeSinceLastPli() int64 {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.stats.TimeSinceLastPli(time.Now())
}

func (r *RTPStats) UpdateFir() {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.stats.UpdateFir(time.Now())
}

func (r *RTPStats) ToString() string {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.stats.ToString()
}

func (r *RTPStats) ToProto() *livekit.RTPStats {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.stats.ToProto()
}

// ------------------------------------------------------------

type Stats struct {
	clockRate uint32

	initialized bool

	startTime time.Time
	endTime   time.Time

	highestSN   uint16
	highestTS   uint32
	highestTime int64
	extStartSN  uint32
	cycles      uint16

	lastTransit uint32

	bytes             uint64
	bytesDuplicate    uint64
	bytesPadding      uint64
	packetsDuplicate  uint32
	packetsPadding    uint32
	packetsOutOfOrder uint32
	packetsLost       uint32
	frames            uint32

	jitter    float64
	maxJitter float64

	missingSNs   [65536 / 64]uint64
	gapHistogram [GapHistogramNumBins]uint32

	nacks      uint32
	nackMisses uint32

	plis    uint32
	lastPli time.Time

	firs    uint32
	lastFir time.Time
}

func newStats(clockRate uint32) *Stats {
	return &Stats{
		clockRate: clockRate,
	}
}

func (s *Stats) Close(now time.Time) {
	s.endTime = now
}

func (s *Stats) Update(now time.Time, rtp *rtp.Packet, packetTime int64) (isHighestSN bool, hasLoss bool, lossStartInclusive uint16, lossEndExclusive uint16) {
	if !s.endTime.IsZero() {
		return
	}

	if !s.initialized {
		s.initialized = true

		s.startTime = now

		s.highestSN = rtp.SequenceNumber - 1
		s.highestTS = rtp.Timestamp
		s.highestTime = packetTime

		s.extStartSN = uint32(rtp.SequenceNumber)
		s.cycles = 0
	}

	isDuplicate := false

	pktSize := uint64(rtp.MarshalSize())
	s.bytes += pktSize

	diff := rtp.SequenceNumber - s.highestSN
	switch {
	// duplicate
	case diff == 0:
		s.bytesDuplicate += pktSize
		s.packetsDuplicate++
		isDuplicate = true

	// out-of-order
	case diff > (1 << 15):
		s.packetsOutOfOrder++
		if !s.isMissingSN(rtp.SequenceNumber) {
			s.bytesDuplicate += pktSize
			s.packetsDuplicate++
			isDuplicate = true
		}

	// in-order
	default:
		isHighestSN = true
		if int(diff) > 1 {
			hasLoss = true
			lossStartInclusive = s.highestSN + 1
			lossEndExclusive = rtp.SequenceNumber
		}

		// update gap histogram
		s.updateGapHistogram(int(diff))

		// update missing sequence numbers
		for lost := s.highestSN + 1; lost != rtp.SequenceNumber; lost++ {
			s.setMissingSN(lost)
		}

		if rtp.SequenceNumber < s.highestSN {
			s.cycles++
		}
		s.highestSN = rtp.SequenceNumber
		s.highestTS = rtp.Timestamp
		s.highestTime = packetTime

		if rtp.Marker {
			s.frames++
		}
	}

	// clear received sequence number from missing list
	if s.isMissingSN(rtp.SequenceNumber) {
		s.packetsLost++
	}
	s.clearMissingSN(rtp.SequenceNumber)

	if !isDuplicate {
		s.updateJitter(rtp, packetTime)
	}

	return
}

func (s *Stats) UpdateNack(nackCount int, nackMissCount int) {
	if !s.endTime.IsZero() {
		return
	}

	s.nacks += uint32(nackCount)
	s.nackMisses += uint32(nackMissCount)
}

func (s *Stats) UpdatePli(now time.Time) {
	if !s.endTime.IsZero() {
		return
	}

	s.plis++
	s.lastPli = now
}

func (s *Stats) TimeSinceLastPli(now time.Time) int64 {
	return now.UnixNano() - s.lastPli.UnixNano()
}

func (s *Stats) UpdateFir(now time.Time) {
	if !s.endTime.IsZero() {
		return
	}

	s.firs++
	s.lastFir = now
}

func (s *Stats) ToString() string {
	p := s.ToProto()
	if p == nil {
		return ""
	}

	str := fmt.Sprintf("t: %+v|%+v|%.2fs", p.StartTime.AsTime().Format(time.UnixDate), p.EndTime.AsTime().Format(time.UnixDate), p.Duration)

	str += fmt.Sprintf(", p: %d|%.2f/s", p.Packets, p.PacketRate)
	str += fmt.Sprintf(", l: %d|%.1f/s|%.2f%%", p.PacketsLost, p.PacketLossRate, p.PacketLossPercentage)
	str += fmt.Sprintf(", b: %d|%.1fbps", p.Bytes, p.Bitrate)
	str += fmt.Sprintf(", f: %d|%.1f/s", p.Frames, p.FrameRate)

	str += fmt.Sprintf(", d: %d|%.2f/s", p.PacketsDuplicate, p.PacketDuplicateRate)
	str += fmt.Sprintf(", bd: %d|%.1fbps", p.BytesDuplicate, p.BitrateDuplicate)

	str += fmt.Sprintf(", pp: %d|%.2f/s", p.PacketsPadding, p.PacketPaddingRate)
	str += fmt.Sprintf(", bp: %d|%.1fbps", p.BytesPadding, p.BitratePadding)

	str += fmt.Sprintf(", o: %d", p.PacketsOutOfOrder)

	str += fmt.Sprintf(", c: %d, j: %d(%.1fus)|%d(%.1fus)", s.clockRate, uint32(s.jitter), p.JitterCurrent, uint32(s.maxJitter), p.JitterMax)

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

	return str
}

func (s *Stats) ToProto() *livekit.RTPStats {
	if s.startTime.IsZero() {
		return nil
	}

	endTime := s.endTime
	if endTime.IsZero() {
		endTime = time.Now()
	}
	elapsed := endTime.Sub(s.startTime).Seconds()

	extHighestSN := uint32(s.cycles)<<16 | uint32(s.highestSN)
	packetsExpected := extHighestSN - s.extStartSN

	packetsLost := s.packetsLost
	for idx := range s.missingSNs {
		n := s.missingSNs[idx]
		for n != 0 {
			packetsLost++
			n &= (n - 1)
		}
	}
	packetLostRate := float64(packetsLost) / elapsed
	packetLostPercentage := float32(packetsLost) / float32(packetsExpected) * 100.0

	packets := packetsExpected - packetsLost
	packetRate := float64(packets) / elapsed
	packetDuplicateRate := float64(s.packetsDuplicate) / elapsed
	packetPaddingRate := float64(s.packetsPadding) / elapsed

	bitrate := float64(s.bytes) * 8.0 / elapsed
	bitrateDuplicate := float64(s.bytesDuplicate) * 8.0 / elapsed
	bitratePadding := float64(s.bytesPadding) * 8.0 / elapsed

	frameRate := float64(s.frames) / elapsed

	jitterTime := s.jitter / float64(s.clockRate) * 1e6
	maxJitterTime := s.maxJitter / float64(s.clockRate) * 1e6

	p := &livekit.RTPStats{
		StartTime:            timestamppb.New(s.startTime),
		EndTime:              timestamppb.New(endTime),
		Duration:             elapsed,
		Packets:              packets,
		PacketRate:           packetRate,
		Bytes:                s.bytes,
		Bitrate:              bitrate,
		PacketsLost:          packetsLost,
		PacketLossRate:       packetLostRate,
		PacketLossPercentage: packetLostPercentage,
		PacketsDuplicate:     s.packetsDuplicate,
		PacketDuplicateRate:  packetDuplicateRate,
		BytesDuplicate:       s.bytesDuplicate,
		BitrateDuplicate:     bitrateDuplicate,
		PacketsPadding:       s.packetsPadding,
		PacketPaddingRate:    packetPaddingRate,
		BytesPadding:         s.bytesPadding,
		BitratePadding:       bitratePadding,
		PacketsOutOfOrder:    s.packetsOutOfOrder,
		Frames:               s.frames,
		FrameRate:            frameRate,
		JitterCurrent:        jitterTime,
		JitterMax:            maxJitterTime,
		Nacks:                s.nacks,
		NackMisses:           s.nackMisses,
		Plis:                 s.plis,
		LastPli:              timestamppb.New(s.lastPli),
		Firs:                 s.firs,
		LastFir:              timestamppb.New(s.lastFir),
	}

	gapsPresent := false
	for i := 0; i < len(s.gapHistogram); i++ {
		if s.gapHistogram[i] == 0 {
			continue
		}

		gapsPresent = true
		break
	}

	if gapsPresent {
		p.GapHistogram = make(map[int32]uint32, GapHistogramNumBins)
		for i := 0; i < len(s.gapHistogram); i++ {
			if s.gapHistogram[i] == 0 {
				continue
			}

			p.GapHistogram[int32(i+1)] = s.gapHistogram[i]
		}
	}

	return p
}

func (s *Stats) setMissingSN(sn uint16) {
	idx, rem := getPos(sn)
	s.missingSNs[idx] |= (1 << rem)
}

func (s *Stats) clearMissingSN(sn uint16) {
	idx, rem := getPos(sn)
	s.missingSNs[idx] &^= (1 << rem)
}

func (s *Stats) isMissingSN(sn uint16) bool {
	idx, rem := getPos(sn)
	return (s.missingSNs[idx] & (1 << rem)) != 0
}

func (s *Stats) updateJitter(rtp *rtp.Packet, packetTime int64) {
	packetTimeRTP := uint32(packetTime / 1e6 * int64(s.clockRate/1e3))
	transit := packetTimeRTP - rtp.Timestamp

	if s.lastTransit != 0 {
		d := int32(transit - s.lastTransit)
		if d < 0 {
			d = -d
		}
		s.jitter += (float64(d) - s.jitter) / 16
		if s.jitter > s.maxJitter {
			s.maxJitter = s.jitter
		}
	}

	s.lastTransit = transit
}

func (s *Stats) updateGapHistogram(gap int) {
	if gap < 2 {
		return
	}

	missing := gap - 1
	if missing > len(s.gapHistogram) {
		s.gapHistogram[len(s.gapHistogram)-1]++
	} else {
		s.gapHistogram[missing-1]++
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
	}
}

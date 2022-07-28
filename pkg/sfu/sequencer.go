package sfu

import (
	"math"
	"sync"
	"time"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

const (
	maxPadding           = 2000
	defaultRtt           = 70
	ignoreRetransmission = 100 // Ignore packet retransmission after ignoreRetransmission milliseconds
)

func btoi(b bool) int {
	if b {
		return 1
	}

	return 0
}

func itob(i int) bool {
	return i != 0
}

type packetMeta struct {
	// Original sequence number from stream.
	// The original sequence number is used to find the original
	// packet from publisher
	sourceSeqNo uint16
	// Modified sequence number after offset.
	// This sequence number is used for the associated
	// down track, is modified according the offsets, and
	// must not be shared
	targetSeqNo uint16
	// Modified timestamp for current associated
	// down track.
	timestamp uint32
	// The last time this packet was nack requested.
	// Sometimes clients request the same packet more than once, so keep
	// track of the requested packets helps to avoid writing multiple times
	// the same packet.
	// The resolution is 1 ms counting after the sequencer start time.
	lastNack uint32
	// number of NACKs this packet has received
	nacked uint8
	// Spatial layer of packet
	layer int8
	// Information that differs depending on the codec
	misc uint64
	// Dependency Descriptor of packet
	ddBytes []byte
}

func (p *packetMeta) packVP8(vp8 *buffer.VP8) {
	p.misc = uint64(vp8.FirstByte)<<56 |
		uint64(vp8.PictureIDPresent&0x1)<<55 |
		uint64(vp8.TL0PICIDXPresent&0x1)<<54 |
		uint64(vp8.TIDPresent&0x1)<<53 |
		uint64(vp8.KEYIDXPresent&0x1)<<52 |
		uint64(btoi(vp8.MBit)&0x1)<<51 |
		uint64(btoi(vp8.IsKeyFrame)&0x1)<<50 |
		uint64(vp8.PictureID&0x7FFF)<<32 |
		uint64(vp8.TL0PICIDX&0xFF)<<24 |
		uint64(vp8.TID&0x3)<<22 |
		uint64(vp8.Y&0x1)<<21 |
		uint64(vp8.KEYIDX&0x1F)<<16 |
		uint64(vp8.HeaderSize&0xFF)<<8
}

func (p *packetMeta) unpackVP8() *buffer.VP8 {
	return &buffer.VP8{
		FirstByte:        byte(p.misc >> 56),
		PictureIDPresent: int((p.misc >> 55) & 0x1),
		PictureID:        uint16((p.misc >> 32) & 0x7FFF),
		MBit:             itob(int((p.misc >> 51) & 0x1)),
		TL0PICIDXPresent: int((p.misc >> 54) & 0x1),
		TL0PICIDX:        uint8((p.misc >> 24) & 0xFF),
		TIDPresent:       int((p.misc >> 53) & 0x1),
		TID:              uint8((p.misc >> 22) & 0x3),
		Y:                uint8((p.misc >> 21) & 0x1),
		KEYIDXPresent:    int((p.misc >> 52) & 0x1),
		KEYIDX:           uint8((p.misc >> 16) & 0x1F),
		HeaderSize:       int((p.misc >> 8) & 0xFF),
		IsKeyFrame:       itob(int((p.misc >> 50) & 0x1)),
	}
}

// Sequencer stores the packet sequence received by the down track
type sequencer struct {
	sync.Mutex
	init      bool
	max       int
	seq       []packetMeta
	step      int
	headSN    uint16
	startTime int64
	rtt       uint32
	logger    logger.Logger
}

func newSequencer(maxTrack int, logger logger.Logger) *sequencer {
	return &sequencer{
		startTime: time.Now().UnixNano() / 1e6,
		max:       maxTrack + maxPadding,
		seq:       make([]packetMeta, maxTrack+maxPadding),
		rtt:       defaultRtt,
		logger:    logger,
	}
}

func (s *sequencer) setRTT(rtt uint32) {
	s.Lock()
	defer s.Unlock()

	if rtt == 0 {
		s.rtt = defaultRtt
	} else {
		s.rtt = rtt
	}
}

func (s *sequencer) push(sn, offSn uint16, timeStamp uint32, layer int8) *packetMeta {
	s.Lock()
	defer s.Unlock()

	if !s.init {
		s.headSN = offSn - 1
		s.init = true
	}

	diff := offSn - s.headSN
	if diff == 0 {
		// duplicate
		return nil
	}

	slot := 0
	if diff > (1 << 15) {
		// out-of-order
		back := int(s.headSN - offSn)
		if back >= s.max {
			s.logger.Debugw("old packet, can not be sequenced", "head", sn, "received", offSn)
			return nil
		}
		slot = s.step - back - 1
	} else {
		slot = s.step + int(diff) - 1

		s.headSN = offSn
		// for next packet
		s.step = s.wrap(s.step + int(diff))
	}

	slot = s.wrap(slot)
	s.seq[slot] = packetMeta{
		sourceSeqNo: sn,
		targetSeqNo: offSn,
		timestamp:   timeStamp,
		layer:       layer,
	}
	return &s.seq[slot]
}

func (s *sequencer) getPacketsMeta(seqNo []uint16) []packetMeta {
	s.Lock()
	defer s.Unlock()

	meta := make([]packetMeta, 0, len(seqNo))
	refTime := uint32(time.Now().UnixNano()/1e6 - s.startTime)
	for _, sn := range seqNo {
		diff := s.headSN - sn
		if diff > (1<<15) || int(diff) >= s.max {
			// out-of-order from head (should not happen) or too old
			continue
		}

		slot := s.wrap(s.step - int(diff) - 1)
		seq := &s.seq[slot]
		if seq.targetSeqNo != sn {
			continue
		}

		if seq.lastNack == 0 || refTime-seq.lastNack > uint32(math.Min(float64(ignoreRetransmission), float64(2*s.rtt))) {
			seq.nacked++
			seq.lastNack = refTime
			meta = append(meta, *seq)
		}
	}

	return meta
}

func (s *sequencer) wrap(slot int) int {
	for slot < 0 {
		slot += s.max
	}

	for slot >= s.max {
		slot -= s.max
	}

	return slot
}

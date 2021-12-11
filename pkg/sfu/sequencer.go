package sfu

import (
	"sync"
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

const (
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
	// Spatial layer of packet
	layer uint8
	// Information that differs depending the codec
	misc uint64
}

func (p *packetMeta) setVP8PayloadMeta(tlz0Idx uint8, picID uint16) {
	p.misc = uint64(tlz0Idx)<<16 | uint64(picID)
}

func (p *packetMeta) getVP8PayloadMeta() (uint8, uint16) {
	return uint8(p.misc >> 16), uint16(p.misc)
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
}

func newSequencer(maxTrack int) *sequencer {
	return &sequencer{
		startTime: time.Now().UnixNano() / 1e6,
		max:       maxTrack,
		seq:       make([]packetMeta, maxTrack),
	}
}

func (n *sequencer) push(sn, offSn uint16, timeStamp uint32, layer uint8, head bool) *packetMeta {
	n.Lock()
	defer n.Unlock()
	if !n.init {
		n.headSN = offSn
		n.init = true
	}

	step := 0
	if head {
		inc := offSn - n.headSN
		for i := uint16(1); i < inc; i++ {
			n.step++
			if n.step >= n.max {
				n.step = 0
			}
		}
		step = n.step
		n.headSN = offSn
	} else {
		step = n.step - int(n.headSN-offSn)
		if step < 0 {
			if step*-1 >= n.max {
				Logger.V(0).Info("Old packet received, can not be sequenced", "head", sn, "received", offSn)
				return nil
			}
			step = n.max + step
		}
	}
	n.seq[n.step] = packetMeta{
		sourceSeqNo: sn,
		targetSeqNo: offSn,
		timestamp:   timeStamp,
		layer:       layer,
	}
	pm := &n.seq[n.step]
	n.step++
	if n.step >= n.max {
		n.step = 0
	}
	return pm
}

func (n *sequencer) getSeqNoPairs(seqNo []uint16) []packetMeta {
	n.Lock()
	meta := make([]packetMeta, 0, 17)
	refTime := uint32(time.Now().UnixNano()/1e6 - n.startTime)
	for _, sn := range seqNo {
		step := n.step - int(n.headSN-sn) - 1
		if step < 0 {
			if step*-1 >= n.max {
				continue
			}
			step = n.max + step
		}
		seq := &n.seq[step]
		if seq.targetSeqNo == sn {
			if seq.lastNack == 0 || refTime-seq.lastNack > ignoreRetransmission {
				seq.lastNack = refTime
				meta = append(meta, *seq)
			}
		}
	}
	n.Unlock()
	return meta
}

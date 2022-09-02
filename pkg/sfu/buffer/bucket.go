package buffer

import (
	"encoding/binary"
	"fmt"
	"math"
)

const (
	maxPktSize     = 1500
	pktSizeHeader  = 2
	seqNumOffset   = 2
	seqNumSize     = 2
	invalidPktSize = uint16(65535)
)

type Bucket struct {
	buf []byte
	src *[]byte

	init               bool
	resyncOnNextPacket bool
	step               int
	headSN             uint16
	maxSteps           int
}

func NewBucket(buf *[]byte) *Bucket {
	b := &Bucket{
		src:      buf,
		buf:      *buf,
		maxSteps: int(math.Floor(float64(len(*buf)) / float64(maxPktSize))),
	}

	b.invalidate(0, b.maxSteps)
	return b
}

func (b *Bucket) ResyncOnNextPacket() {
	b.resyncOnNextPacket = true
}

func (b *Bucket) AddPacket(pkt []byte) ([]byte, error) {
	sn := binary.BigEndian.Uint16(pkt[seqNumOffset : seqNumOffset+seqNumSize])
	if !b.init {
		b.headSN = sn - 1
		b.init = true
	}

	if b.resyncOnNextPacket {
		b.resyncOnNextPacket = false

		b.headSN = sn - 1
		b.invalidate(0, b.maxSteps)
	}

	diff := sn - b.headSN
	if diff == 0 || diff > (1<<15) {
		// duplicate of last packet or out-of-order
		return b.set(sn, pkt)
	}

	return b.push(sn, pkt)
}

func (b *Bucket) GetPacket(buf []byte, sn uint16) (i int, err error) {
	p := b.get(sn)
	if p == nil {
		err = ErrPacketNotFound
		return
	}
	i = len(p)
	if cap(buf) < i {
		err = ErrBufferTooSmall
		return
	}
	if len(buf) < i {
		buf = buf[:i]
	}
	copy(buf, p)
	return
}

func (b *Bucket) push(sn uint16, pkt []byte) ([]byte, error) {
	diff := int(sn-b.headSN) - 1
	b.headSN = sn

	// invalidate slots if there is a gap in the sequence number
	b.invalidate(b.step, diff)

	// store headSN packet
	off := b.offset(b.step + diff)
	storedPkt := b.store(off, pkt)

	// for next packet
	b.step = b.wrap(b.step + diff + 1)

	return storedPkt, nil
}

func (b *Bucket) get(sn uint16) []byte {
	diff := b.headSN - sn
	if int(diff) >= b.maxSteps {
		// too old or asking for something ahead of headSN (which is effectively too old with wrap around)
		return nil
	}

	off := b.offset(b.step - int(diff) - 1)
	if binary.BigEndian.Uint16(b.buf[off+pktSizeHeader+seqNumOffset:off+pktSizeHeader+seqNumOffset+seqNumSize]) != sn {
		return nil
	}

	sz := binary.BigEndian.Uint16(b.buf[off : off+pktSizeHeader])
	if sz == invalidPktSize {
		return nil
	}

	off += pktSizeHeader
	return b.buf[off : off+int(sz)]
}

func (b *Bucket) set(sn uint16, pkt []byte) ([]byte, error) {
	diff := b.headSN - sn
	if int(diff) >= b.maxSteps {
		return nil, fmt.Errorf("%w, headSN %d, sn %d", ErrPacketTooOld, b.headSN, sn)
	}

	off := b.offset(b.step - int(diff) - 1)

	// Do not overwrite if duplicate
	if binary.BigEndian.Uint16(b.buf[off+pktSizeHeader+seqNumOffset:off+pktSizeHeader+seqNumOffset+seqNumSize]) == sn {
		return nil, ErrRTXPacket
	}

	return b.store(off, pkt), nil
}

func (b *Bucket) store(off int, pkt []byte) []byte {
	// store packet size
	binary.BigEndian.PutUint16(b.buf[off:], uint16(len(pkt)))

	// store packet
	off += pktSizeHeader
	copy(b.buf[off:], pkt)

	return b.buf[off : off+len(pkt)]
}

func (b *Bucket) wrap(slot int) int {
	for slot < 0 {
		slot += b.maxSteps
	}

	for slot >= b.maxSteps {
		slot -= b.maxSteps
	}

	return slot
}

func (b *Bucket) offset(slot int) int {
	return b.wrap(slot) * maxPktSize
}

func (b *Bucket) invalidate(startSlot int, numSlots int) {
	if numSlots > b.maxSteps {
		numSlots = b.maxSteps
	}

	for i := 0; i < numSlots; i++ {
		off := b.offset(startSlot + i)
		binary.BigEndian.PutUint16(b.buf[off:], invalidPktSize)
	}
}

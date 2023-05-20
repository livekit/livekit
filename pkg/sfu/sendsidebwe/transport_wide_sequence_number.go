package sendsidebwe

import (
	"math/rand"

	"go.uber.org/atomic"
)

type TransportWideSequenceNumber struct {
	seq *atomic.Uint64
}

func NewTransportWideSequenceNumber() *TransportWideSequenceNumber {
	return &TransportWideSequenceNumber{
		seq: atomic.NewUint64(uint64(rand.Intn(1<<14)) + uint64(1<<15)), // a random number in third quartile of sequence number space
	}
}

func (t *TransportWideSequenceNumber) GetNext() uint16 {
	return uint16(t.seq.Inc())
}

// ------------------------------------------------

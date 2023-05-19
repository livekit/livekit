package sendsidebwe

import (
	"math/rand"

	"github.com/pion/rtp"
	"go.uber.org/atomic"
)

type TransportWideExtension struct {
	seq *atomic.Uint64
}

func NewTransportWideExtension() *TransportWideExtension {
	return &TransportWideExtension{
		seq: atomic.NewUint64(uint64(rand.Intn(1<<14)) + uint64(1<<15)), // a random number in third quartile of sequence number space
	}
}

func (t *TransportWideExtension) Get() rtp.TransportCCExtension {
	return rtp.TransportCCExtension{
		TransportSequence: uint16(t.seq.Inc()),
	}
}

// ------------------------------------------------

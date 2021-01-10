package rtc

import (
	"sync"

	"github.com/pion/ion-sfu/pkg/buffer"
)

const (
	rtpPacketMaxSize = 1460
)

// package level factories for buffers
var (
	bufferFactory = buffer.NewBufferFactory()
	packetFactory = &sync.Pool{
		New: func() interface{} {
			return make([]byte, rtpPacketMaxSize)
		},
	}
)

package sfu

import (
	"sync"
)

var (
	PacketFactory *sync.Pool
)

func init() {
	// Init packet factory
	PacketFactory = &sync.Pool{
		New: func() interface{} {
			b := make([]byte, 1460)
			return &b
		},
	}
}

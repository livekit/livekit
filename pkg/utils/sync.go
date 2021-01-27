package utils

import (
	"errors"
	"sync/atomic"
)

var (
	ErrChannelClosed = errors.New("cannot write to closed channel")
)

type AtomicFlag struct {
	val uint32
}

// set flag to value if existing flag is different, otherwise return
func (b *AtomicFlag) TrySet(bVal bool) bool {
	var v uint32
	if bVal {
		v = 1
	}
	old := b.val
	// value is the same, nochanges
	if old == v {
		return false
	}
	return atomic.CompareAndSwapUint32(&b.val, old, v)
}

func (b *AtomicFlag) Get() bool {
	return b.val == 1
}

// a channel that ignores writes when it closes instead of panic
type CalmChannel struct {
	channel  chan interface{}
	isClosed AtomicFlag
}

func NewCalmChannel(size int) *CalmChannel {
	return &CalmChannel{
		channel: make(chan interface{}, size),
	}
}

func (c *CalmChannel) Close() {
	if !c.isClosed.TrySet(true) {
		return
	}
	close(c.channel)
}

func (c *CalmChannel) ReadChan() <-chan interface{} {
	return c.channel
}

func (c *CalmChannel) Write(o interface{}) error {
	if c.isClosed.Get() {
		return ErrChannelClosed
	}
	c.channel <- o
	return nil
}

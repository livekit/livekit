package utils

import (
	"errors"
	"sync/atomic"
)

var (
	ErrChannelClosed = errors.New("cannot write to closed channel")
)

type AtomicFlag struct {
	val int32
}

// set flag to value if existing flag is different, otherwise return
func (b *AtomicFlag) TrySet(bVal bool) bool {
	var v int32
	if bVal {
		v = 1
	}
	prev := atomic.SwapInt32(&b.val, v)
	// already set. unsuccessful
	if prev == v {
		return false
	}
	return true
}

func (b *AtomicFlag) Get() bool {
	return atomic.LoadInt32(&b.val) == 1
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

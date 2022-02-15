package sfu

import "sync/atomic"

type atomicBool int32

func (a *atomicBool) set(value bool) (swapped bool) {
	if value {
		return atomic.SwapInt32((*int32)(a), 1) == 0
	}
	return atomic.SwapInt32((*int32)(a), 0) == 1
}

func (a *atomicBool) get() bool {
	return atomic.LoadInt32((*int32)(a)) != 0
}

type atomicUint8 uint32

func (a *atomicUint8) set(value uint8) {
	atomic.StoreUint32((*uint32)(a), uint32(value))
}

func (a *atomicUint8) get() uint8 {
	return uint8(atomic.LoadUint32((*uint32)(a)))
}

type atomicUint32 uint32

func (a *atomicUint32) set(value uint32) {
	atomic.StoreUint32((*uint32)(a), value)
}

func (a *atomicUint32) add(value uint32) {
	atomic.AddUint32((*uint32)(a), value)
}

func (a *atomicUint32) get() uint32 {
	return atomic.LoadUint32((*uint32)(a))
}

type atomicInt64 int64

func (a *atomicInt64) set(value int64) {
	atomic.StoreInt64((*int64)(a), value)
}

func (a *atomicInt64) get() int64 {
	return atomic.LoadInt64((*int64)(a))
}

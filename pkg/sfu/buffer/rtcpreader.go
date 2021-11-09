package buffer

import (
	"io"
	"sync/atomic"
)

type RTCPReader struct {
	ssrc     uint32
	closed   atomicBool
	onPacket atomic.Value //func([]byte)
	onClose  func()
}

func NewRTCPReader(ssrc uint32) *RTCPReader {
	return &RTCPReader{ssrc: ssrc}
}

func (r *RTCPReader) Write(p []byte) (n int, err error) {
	if r.closed.get() {
		err = io.EOF
		return
	}
	if f, ok := r.onPacket.Load().(func([]byte)); ok {
		f(p)
	}
	return
}

func (r *RTCPReader) OnClose(fn func()) {
	r.onClose = fn
}

func (r *RTCPReader) Close() error {
	r.closed.set(true)
	r.onClose()
	return nil
}

func (r *RTCPReader) OnPacket(f func([]byte)) {
	r.onPacket.Store(f)
}

func (r *RTCPReader) Read(_ []byte) (n int, err error) { return }

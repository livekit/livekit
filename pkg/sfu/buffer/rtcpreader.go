// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package buffer

import (
	"io"

	"go.uber.org/atomic"
)

type RTCPReader struct {
	ssrc     uint32
	closed   atomic.Bool
	onPacket atomic.Value // func([]byte)
	onClose  func()
}

func NewRTCPReader(ssrc uint32) *RTCPReader {
	return &RTCPReader{ssrc: ssrc}
}

func (r *RTCPReader) Write(p []byte) (n int, err error) {
	if r.closed.Load() {
		err = io.EOF
		return
	}
	if f, ok := r.onPacket.Load().(func([]byte)); ok && f != nil {
		f(p)
	}
	return
}

func (r *RTCPReader) OnClose(fn func()) {
	r.onClose = fn
}

func (r *RTCPReader) Close() error {
	if r.closed.Swap(true) {
		return nil
	}

	r.onClose()
	return nil
}

func (r *RTCPReader) OnPacket(f func([]byte)) {
	r.onPacket.Store(f)
}

func (r *RTCPReader) Read(_ []byte) (n int, err error) { return }

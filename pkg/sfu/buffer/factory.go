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
	"sync"

	"github.com/pion/transport/v3/packetio"
)

type FactoryOfBufferFactory struct {
	trackingPacketsVideo int
	trackingPacketsAudio int
}

func NewFactoryOfBufferFactory(trackingPacketsVideo int, trackingPacketsAudio int) *FactoryOfBufferFactory {
	return &FactoryOfBufferFactory{
		trackingPacketsVideo: trackingPacketsVideo,
		trackingPacketsAudio: trackingPacketsAudio,
	}
}

func (f *FactoryOfBufferFactory) CreateBufferFactory() *Factory {
	return &Factory{
		trackingPacketsVideo: f.trackingPacketsVideo,
		trackingPacketsAudio: f.trackingPacketsAudio,
		rtpBuffers:           make(map[uint32]*Buffer),
		rtcpReaders:          make(map[uint32]*RTCPReader),
		rtxPair:              make(map[uint32]uint32),
	}
}

type Factory struct {
	sync.RWMutex
	trackingPacketsVideo int
	trackingPacketsAudio int
	rtpBuffers           map[uint32]*Buffer
	rtcpReaders          map[uint32]*RTCPReader
	rtxPair              map[uint32]uint32 // repair -> base
}

func (f *Factory) GetOrNew(packetType packetio.BufferPacketType, ssrc uint32) io.ReadWriteCloser {
	f.Lock()
	defer f.Unlock()
	switch packetType {
	case packetio.RTCPBufferPacket:
		if reader, ok := f.rtcpReaders[ssrc]; ok {
			return reader
		}
		reader := NewRTCPReader(ssrc)
		f.rtcpReaders[ssrc] = reader
		reader.OnClose(func() {
			f.Lock()
			delete(f.rtcpReaders, ssrc)
			f.Unlock()
		})
		return reader
	case packetio.RTPBufferPacket:
		if reader, ok := f.rtpBuffers[ssrc]; ok {
			return reader
		}
		buffer := NewBuffer(ssrc, f.trackingPacketsVideo, f.trackingPacketsAudio)
		f.rtpBuffers[ssrc] = buffer
		for repair, base := range f.rtxPair {
			if repair == ssrc {
				baseBuffer, ok := f.rtpBuffers[base]
				if ok {
					buffer.SetPrimaryBufferForRTX(baseBuffer)
				}
				break
			} else if base == ssrc {
				repairBuffer, ok := f.rtpBuffers[repair]
				if ok {
					repairBuffer.SetPrimaryBufferForRTX(buffer)
				}
				break
			}
		}
		buffer.OnClose(func() {
			f.Lock()
			delete(f.rtpBuffers, ssrc)
			delete(f.rtxPair, ssrc)
			f.Unlock()
		})
		return buffer
	}
	return nil
}

func (f *Factory) GetBufferPair(ssrc uint32) (*Buffer, *RTCPReader) {
	f.RLock()
	defer f.RUnlock()
	return f.rtpBuffers[ssrc], f.rtcpReaders[ssrc]
}

func (f *Factory) GetBuffer(ssrc uint32) *Buffer {
	f.RLock()
	defer f.RUnlock()
	return f.rtpBuffers[ssrc]
}

func (f *Factory) GetRTCPReader(ssrc uint32) *RTCPReader {
	f.RLock()
	defer f.RUnlock()
	return f.rtcpReaders[ssrc]
}

func (f *Factory) SetRTXPair(repair, base uint32) {
	f.Lock()
	repairBuffer, baseBuffer := f.rtpBuffers[repair], f.rtpBuffers[base]
	if repairBuffer == nil || baseBuffer == nil {
		f.rtxPair[repair] = base
	}
	f.Unlock()
	if repairBuffer != nil && baseBuffer != nil {
		repairBuffer.SetPrimaryBufferForRTX(baseBuffer)
	}
}

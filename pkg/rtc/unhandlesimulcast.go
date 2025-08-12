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

package rtc

import (
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"github.com/livekit/livekit-server/pkg/sfu/utils"
)

const (
	simulcastProbeCount = 10
)

type UnhandleSimulcastOption func(r *UnhandleSimulcastInterceptor) error

func UnhandleSimulcastTracks(tracks map[uint32]SimulcastTrackInfo) UnhandleSimulcastOption {
	return func(r *UnhandleSimulcastInterceptor) error {
		r.simTracks = tracks
		return nil
	}
}

type UnhandleSimulcastInterceptorFactory struct {
	opts []UnhandleSimulcastOption
}

func (f *UnhandleSimulcastInterceptorFactory) NewInterceptor(id string) (interceptor.Interceptor, error) {
	i := &UnhandleSimulcastInterceptor{simTracks: map[uint32]SimulcastTrackInfo{}}
	for _, o := range f.opts {
		if err := o(i); err != nil {
			return nil, err
		}
	}
	return i, nil
}

func NewUnhandleSimulcastInterceptorFactory(opts ...UnhandleSimulcastOption) (*UnhandleSimulcastInterceptorFactory, error) {
	return &UnhandleSimulcastInterceptorFactory{opts: opts}, nil
}

type unhandleSimulcastRTPReader struct {
	SimulcastTrackInfo
	tryTimes            int
	reader              interceptor.RTPReader
	midExtensionID      uint8
	streamIDExtensionID uint8
}

func (r *unhandleSimulcastRTPReader) Read(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
	n, a, err := r.reader.Read(b, a)
	if r.tryTimes < 0 || err != nil {
		return n, a, err
	}

	header := rtp.Header{}
	hsize, err := header.Unmarshal(b[:n])
	if err != nil {
		return n, a, nil
	}
	var mid, rid string
	if payload := header.GetExtension(r.midExtensionID); payload != nil {
		mid = string(payload)
	}

	if payload := header.GetExtension(r.streamIDExtensionID); payload != nil {
		rid = string(payload)
	}

	if mid != "" && rid != "" {
		r.tryTimes = -1
		return n, a, nil
	}

	r.tryTimes--

	if mid == "" {
		header.SetExtension(r.midExtensionID, []byte(r.Mid))
	}
	if rid == "" {
		header.SetExtension(r.streamIDExtensionID, []byte(r.Rid))
	}

	hsize2 := header.MarshalSize()

	if hsize2-hsize+n > len(b) { // no enough buf to set extension
		return n, a, nil
	}
	copy(b[hsize2:], b[hsize:n])
	header.MarshalTo(b)
	return hsize2 - hsize + n, a, nil
}

type UnhandleSimulcastInterceptor struct {
	interceptor.NoOp
	simTracks map[uint32]SimulcastTrackInfo
}

func (u *UnhandleSimulcastInterceptor) BindRemoteStream(info *interceptor.StreamInfo, reader interceptor.RTPReader) interceptor.RTPReader {
	if t, ok := u.simTracks[info.SSRC]; ok {
		// if we support fec for simulcast streams at future, should get rsid extensions
		midExtensionID := utils.GetHeaderExtensionID(info.RTPHeaderExtensions, webrtc.RTPHeaderExtensionCapability{URI: sdp.SDESMidURI})
		streamIDExtensionID := utils.GetHeaderExtensionID(info.RTPHeaderExtensions, webrtc.RTPHeaderExtensionCapability{URI: sdp.SDESRTPStreamIDURI})
		if midExtensionID == 0 || streamIDExtensionID == 0 {
			return reader
		}

		return &unhandleSimulcastRTPReader{
			SimulcastTrackInfo:  t,
			reader:              reader,
			tryTimes:            simulcastProbeCount,
			midExtensionID:      uint8(midExtensionID),
			streamIDExtensionID: uint8(streamIDExtensionID),
		}
	}
	return reader
}

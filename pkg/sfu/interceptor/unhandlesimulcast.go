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

package interceptor

import (
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
	"go.uber.org/zap/zapcore"

	"github.com/livekit/livekit-server/pkg/sfu/utils"
	"github.com/livekit/protocol/logger"
)

const (
	simulcastProbeCount = 10
)

type SimulcastTrackInfo struct {
	Mid            string
	StreamID       string
	RepairSSRC     uint32 // set only when `IsRepairStream: false`, i. e. RTX SSRC for the primary stream
	IsRepairStream bool
}

func (s *SimulcastTrackInfo) MarshalLogObject(e zapcore.ObjectEncoder) error {
	e.AddString("Mid", s.Mid)
	e.AddString("StreamID", s.StreamID)
	e.AddUint32("RepairSSRC", s.RepairSSRC)
	e.AddBool("IsRepairStream", s.IsRepairStream)
	return nil
}

// -------------------------------------------------------------------

type UnhandleSimulcastOption func(u *UnhandleSimulcastInterceptor) error

func UnhandleSimulcastTracks(logger logger.Logger, tracks map[uint32]SimulcastTrackInfo) UnhandleSimulcastOption {
	return func(u *UnhandleSimulcastInterceptor) error {
		u.logger = logger
		u.simTracks = tracks
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
	logger    logger.Logger
	tryTimes  int
	reader    interceptor.RTPReader
	midExtID  uint8
	ridExtID  uint8
	rsidExtID uint8
}

func (u *unhandleSimulcastRTPReader) Read(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
	n, a, err := u.reader.Read(b, a)
	if u.tryTimes < 0 || err != nil {
		return n, a, err
	}

	header := rtp.Header{}
	hsize, err := header.Unmarshal(b[:n])
	if err != nil {
		return n, a, nil
	}
	var mid, rid, rsid string
	if payload := header.GetExtension(u.midExtID); payload != nil {
		mid = string(payload)
	}

	if payload := header.GetExtension(u.ridExtID); payload != nil {
		rid = string(payload)
	}

	if payload := header.GetExtension(u.rsidExtID); payload != nil {
		rid = string(payload)
	}

	if mid != "" && (rid != "" || rsid != "") {
		u.logger.Debugw(
			"unhandle stream found",
			"mid", mid,
			"rid", rid,
			"rsid", rsid,
			"ssrc", header.SSRC,
			"simulcastTrackInfo", u.SimulcastTrackInfo,
		)
		u.tryTimes = -1
		return n, a, nil
	} else {
		// ignore padding only packet for probe count
		if !(header.Padding && n-header.MarshalSize()-int(b[n-1]) == 0) {
			u.tryTimes--
		}
	}

	if mid == "" {
		header.SetExtension(u.midExtID, []byte(u.Mid))
	}
	if rid == "" && !u.IsRepairStream {
		header.SetExtension(u.ridExtID, []byte(u.StreamID))
	}
	if rsid == "" && u.IsRepairStream {
		header.SetExtension(u.rsidExtID, []byte(u.StreamID))
	}

	hsize2 := header.MarshalSize()

	if hsize2-hsize+n > len(b) { // no enough buf to set extension
		return n, a, nil
	}
	copy(b[hsize2:], b[hsize:n])
	header.MarshalTo(b)
	u.logger.Debugw(
		"unhandle stream injecting",
		"mid", mid,
		"rid", rid,
		"rsid", rsid,
		"ssrc", header.SSRC,
		"simulcastTrackInfo", u.SimulcastTrackInfo,
	)
	return hsize2 - hsize + n, a, nil
}

type UnhandleSimulcastInterceptor struct {
	interceptor.NoOp
	logger    logger.Logger
	simTracks map[uint32]SimulcastTrackInfo
}

func (u *UnhandleSimulcastInterceptor) BindRemoteStream(info *interceptor.StreamInfo, reader interceptor.RTPReader) interceptor.RTPReader {
	if t, ok := u.simTracks[info.SSRC]; ok {
		midExtensionID := utils.GetHeaderExtensionID(info.RTPHeaderExtensions, webrtc.RTPHeaderExtensionCapability{URI: sdp.SDESMidURI})
		streamIDExtensionID := utils.GetHeaderExtensionID(info.RTPHeaderExtensions, webrtc.RTPHeaderExtensionCapability{URI: sdp.SDESRTPStreamIDURI})
		repairStreamIDExtensionID := utils.GetHeaderExtensionID(info.RTPHeaderExtensions, webrtc.RTPHeaderExtensionCapability{URI: sdp.SDESRepairRTPStreamIDURI})
		if midExtensionID == 0 || streamIDExtensionID == 0 || repairStreamIDExtensionID == 0 {
			return reader
		}

		return &unhandleSimulcastRTPReader{
			SimulcastTrackInfo: t,
			logger:             u.logger,
			reader:             reader,
			tryTimes:           simulcastProbeCount,
			midExtID:           uint8(midExtensionID),
			ridExtID:           uint8(streamIDExtensionID),
			rsidExtID:          uint8(repairStreamIDExtensionID),
		}
	}
	return reader
}

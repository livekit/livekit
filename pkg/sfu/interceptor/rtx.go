// Copyright 2024 LiveKit, Inc.
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
	"sync"

	"github.com/pion/interceptor"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"github.com/livekit/livekit-server/pkg/sfu/utils"
	"github.com/livekit/protocol/logger"
)

const (
	SDESRepairRTPStreamIDURI = "urn:ietf:params:rtp-hdrext:sdes:repaired-rtp-stream-id"

	rtxProbeCount = 10
)

type streamInfo struct {
	mid  string
	rid  string
	rsid string
}

type RTXInfoExtractorFactory struct {
	onStreamFound  func(*interceptor.StreamInfo)
	onRTXPairFound func(repair, base uint32)
	lock           sync.Mutex
	streams        map[uint32]streamInfo
	logger         logger.Logger
}

func NewRTXInfoExtractorFactory(onStreamFound func(*interceptor.StreamInfo), onRTXPairFound func(repair, base uint32), logger logger.Logger) *RTXInfoExtractorFactory {
	return &RTXInfoExtractorFactory{
		onStreamFound:  onStreamFound,
		onRTXPairFound: onRTXPairFound,
		streams:        make(map[uint32]streamInfo),
		logger:         logger,
	}
}

func (f *RTXInfoExtractorFactory) NewInterceptor(id string) (interceptor.Interceptor, error) {
	return &RTXInfoExtractor{
		factory: f,
		logger:  f.logger,
	}, nil
}

func (f *RTXInfoExtractorFactory) setStreamInfo(ssrc uint32, mid, rid, rsid string) {
	var repairSsrc, baseSsrc uint32
	f.lock.Lock()

	if rsid != "" {
		// repair stream found, find base stream
		for base, info := range f.streams {
			if info.mid == mid && info.rid == rsid {
				repairSsrc = ssrc
				baseSsrc = base
				delete(f.streams, base)
				break
			}
		}
	} else {
		// base stream found, find repair stream
		for repair, info := range f.streams {
			if info.mid == mid && info.rsid == rid {
				repairSsrc = repair
				baseSsrc = ssrc
				delete(f.streams, repair)
				break
			}
		}
	}

	// no rtx pair found, save it for later
	if repairSsrc == 0 || baseSsrc == 0 {
		f.streams[ssrc] = streamInfo{
			mid:  mid,
			rid:  rid,
			rsid: rsid,
		}
	}

	f.lock.Unlock()

	if repairSsrc != 0 && baseSsrc != 0 {
		f.onRTXPairFound(repairSsrc, baseSsrc)
	}
}

type RTXInfoExtractor struct {
	interceptor.NoOp

	factory *RTXInfoExtractorFactory
	logger  logger.Logger
}

func (u *RTXInfoExtractor) BindRemoteStream(info *interceptor.StreamInfo, reader interceptor.RTPReader) interceptor.RTPReader {
	u.factory.onStreamFound(info)

	midExtensionID := utils.GetHeaderExtensionID(info.RTPHeaderExtensions, webrtc.RTPHeaderExtensionCapability{URI: sdp.SDESMidURI})
	streamIDExtensionID := utils.GetHeaderExtensionID(info.RTPHeaderExtensions, webrtc.RTPHeaderExtensionCapability{URI: sdp.SDESRTPStreamIDURI})
	repairStreamIDExtensionID := utils.GetHeaderExtensionID(info.RTPHeaderExtensions, webrtc.RTPHeaderExtensionCapability{URI: SDESRepairRTPStreamIDURI})
	if midExtensionID == 0 || streamIDExtensionID == 0 || repairStreamIDExtensionID == 0 {
		return reader
	}

	return &rtxInfoReader{
		tryTimes:  rtxProbeCount,
		reader:    reader,
		midExtID:  uint8(midExtensionID),
		ridExtID:  uint8(streamIDExtensionID),
		rsidExtID: uint8(repairStreamIDExtensionID),
		factory:   u.factory,
		logger:    u.logger,
	}
}

type rtxInfoReader struct {
	tryTimes  int
	reader    interceptor.RTPReader
	midExtID  uint8
	ridExtID  uint8
	rsidExtID uint8
	factory   *RTXInfoExtractorFactory
	logger    logger.Logger
}

func (r *rtxInfoReader) Read(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
	n, a, err := r.reader.Read(b, a)
	if r.tryTimes < 0 || err != nil {
		return n, a, err
	}

	if a == nil {
		a = make(interceptor.Attributes)
	}
	header, err := a.GetRTPHeader(b[:n])
	if err != nil {
		return n, a, nil
	}

	var mid, rid, rsid string
	if payload := header.GetExtension(r.midExtID); payload != nil {
		mid = string(payload)
	}

	if payload := header.GetExtension(r.ridExtID); payload != nil {
		rid = string(payload)
	}

	if payload := header.GetExtension(r.rsidExtID); payload != nil {
		rsid = string(payload)
	}

	if mid != "" && (rid != "" || rsid != "") {
		r.logger.Debugw("stream found", "mid", mid, "rid", rid, "rsid", rsid, "ssrc", header.SSRC)
		r.tryTimes = -1
		go r.factory.setStreamInfo(header.SSRC, mid, rid, rsid)
	} else {
		// ignore padding only packet for probe count
		if !(header.Padding && n-header.MarshalSize()-int(b[n-1]) == 0) {
			r.tryTimes--
		}
	}
	return n, a, nil
}

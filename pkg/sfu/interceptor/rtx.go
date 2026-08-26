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

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/utils"
	"github.com/livekit/protocol/logger"
)

const (
	SDESRepairRTPStreamIDURI = "urn:ietf:params:rtp-hdrext:sdes:repaired-rtp-stream-id"

	rtxProbeCount = 10
)

// StreamInfoProber installs a bounded probe that identifies a remote stream from the
// mid/rid/rsid header extensions of its packets. Implemented by buffer.Factory.
type StreamInfoProber interface {
	SetStreamInfoProbe(ssrc uint32, probe *buffer.StreamInfoProbe) bool
}

type streamInfo struct {
	mid  string
	rid  string
	rsid string
}

type RTXInfoExtractorFactory struct {
	onStreamFound  func(*interceptor.StreamInfo)
	onRTXPairFound func(repair, base uint32, rsid string)
	prober         StreamInfoProber
	lock           sync.Mutex
	streams        map[uint32]streamInfo
	paired         map[uint32]struct{}
	logger         logger.Logger
}

func NewRTXInfoExtractorFactory(
	onStreamFound func(*interceptor.StreamInfo),
	onRTXPairFound func(repair, base uint32, rsid string),
	prober StreamInfoProber,
	simTracks map[uint32]SimulcastTrackInfo,
	logger logger.Logger,
) *RTXInfoExtractorFactory {
	f := &RTXInfoExtractorFactory{
		onStreamFound:  onStreamFound,
		onRTXPairFound: onRTXPairFound,
		prober:         prober,
		streams:        make(map[uint32]streamInfo),
		paired:         make(map[uint32]struct{}),
		logger:         logger,
	}
	f.seedSimulcastTracks(simTracks)
	return f
}

// seedSimulcastTracks pairs migrated streams from the migration info. A migrated client
// is mid-stream and stops sending rid/rsid, so the extensions never appear on the wire
// and the pairing has to come from what is already known about the tracks.
func (f *RTXInfoExtractorFactory) seedSimulcastTracks(simTracks map[uint32]SimulcastTrackInfo) {
	for ssrc, info := range simTracks {
		if info.Mid == "" || info.StreamID == "" {
			continue
		}

		if info.IsRepairStream {
			f.SetStreamInfo(ssrc, info.Mid, "", info.StreamID)
			continue
		}

		f.SetStreamInfo(ssrc, info.Mid, info.StreamID, "")
		if info.RepairSSRC != 0 {
			f.SetStreamInfo(info.RepairSSRC, info.Mid, "", info.StreamID)
		}
	}
}

func (f *RTXInfoExtractorFactory) NewInterceptor(id string) (interceptor.Interceptor, error) {
	return &RTXInfoExtractor{
		factory: f,
		logger:  f.logger,
	}, nil
}

func (f *RTXInfoExtractorFactory) SetStreamInfo(ssrc uint32, mid, rid, rsid string) {
	var repairSsrc, baseSsrc uint32
	var repairSid string
	f.lock.Lock()

	if mid == "" || (rid == "" && rsid == "") {
		f.lock.Unlock()
		return
	}

	// the same stream can be reported by both the packet probe and the migration info
	if _, ok := f.paired[ssrc]; ok {
		f.lock.Unlock()
		return
	}

	if rsid != "" {
		// repair stream found, find base stream
		for base, info := range f.streams {
			if info.mid == mid && info.rid == rsid {
				repairSsrc = ssrc
				baseSsrc = base
				repairSid = rsid
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
				repairSid = rid
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
		f.lock.Unlock()
		return
	}

	f.paired[repairSsrc] = struct{}{}
	f.paired[baseSsrc] = struct{}{}
	f.lock.Unlock()

	f.onRTXPairFound(repairSsrc, baseSsrc, repairSid)
}

// ------------------------------------------

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

	// Probe on the buffer write path rather than by wrapping this reader. Remote streams
	// are consumed through SettingEngine.BufferFactory, so nothing here reads the
	// interceptor chain. pion used to drive the repair stream reader, but since
	// pion/webrtc#3470 it only does so when the application reads the TrackRemote or
	// when no BufferFactory is set, neither of which holds.
	ok := u.factory.prober.SetStreamInfoProbe(info.SSRC, &buffer.StreamInfoProbe{
		MidExtID:  uint8(midExtensionID),
		RidExtID:  uint8(streamIDExtensionID),
		RsidExtID: uint8(repairStreamIDExtensionID),
		Tries:     rtxProbeCount,
		OnFound:   u.factory.SetStreamInfo,
	})
	if !ok {
		u.logger.Warnw(
			"could not install stream info probe, rtx pairing will not work", nil,
			"ssrc", info.SSRC,
			"mime", info.MimeType,
		)
	}

	return reader
}

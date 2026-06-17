// Copyright 2026 LiveKit, Inc.
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

package service

import (
	"bytes"

	"github.com/pion/rtp/codecs"
)

var annexBStartCode = []byte{0x00, 0x00, 0x00, 0x01}

type h264AccessUnit struct {
	Payload           []byte
	HasIDR            bool
	HasSPS            bool
	HasPPS            bool
	RTPTime           uint32
	CompletedByMarker bool
}

type h264AccessUnitAssembler struct {
	depacketizer codecs.H264Packet
	current      []byte
	currentTime  uint32
	started      bool
	sps          []byte
	pps          []byte
}

func newH264AccessUnitAssembler() *h264AccessUnitAssembler {
	return &h264AccessUnitAssembler{}
}

func (a *h264AccessUnitAssembler) Push(payload []byte, rtpTime uint32, marker bool) (*h264AccessUnit, error) {
	if a.started && a.currentTime != rtpTime && len(a.current) != 0 {
		a.current = nil
		a.started = false
	}

	chunk, err := a.depacketizer.Unmarshal(payload)
	if err != nil {
		return nil, err
	}
	if len(chunk) != 0 {
		if !a.started {
			a.currentTime = rtpTime
			a.started = true
		}
		a.current = append(a.current, chunk...)
	}

	if !marker || len(a.current) == 0 {
		return nil, nil
	}

	auPayload := a.current
	a.current = nil
	a.started = false

	au := &h264AccessUnit{
		Payload:           auPayload,
		RTPTime:           rtpTime,
		CompletedByMarker: true,
	}
	for _, nalu := range splitAnnexBNALUs(auPayload) {
		if len(nalu) == 0 {
			continue
		}
		switch nalu[0] & 0x1f {
		case 5:
			au.HasIDR = true
		case 7:
			au.HasSPS = true
			a.sps = cloneBytes(nalu)
		case 8:
			au.HasPPS = true
			a.pps = cloneBytes(nalu)
		}
	}

	if au.HasIDR && (!au.HasSPS || !au.HasPPS) {
		au.Payload = a.prependParameterSets(au.Payload, au.HasSPS, au.HasPPS)
		au.HasSPS = au.HasSPS || len(a.sps) != 0
		au.HasPPS = au.HasPPS || len(a.pps) != 0
	}

	return au, nil
}

func (a *h264AccessUnitAssembler) prependParameterSets(payload []byte, hasSPS bool, hasPPS bool) []byte {
	if (hasSPS || len(a.sps) == 0) && (hasPPS || len(a.pps) == 0) {
		return payload
	}

	prepended := make([]byte, 0, len(payload)+len(a.sps)+len(a.pps)+2*len(annexBStartCode))
	if !hasSPS && len(a.sps) != 0 {
		prepended = append(prepended, annexBStartCode...)
		prepended = append(prepended, a.sps...)
	}
	if !hasPPS && len(a.pps) != 0 {
		prepended = append(prepended, annexBStartCode...)
		prepended = append(prepended, a.pps...)
	}
	prepended = append(prepended, payload...)
	return prepended
}

func splitAnnexBNALUs(payload []byte) [][]byte {
	var nalus [][]byte
	for {
		start := bytes.Index(payload, annexBStartCode)
		if start == -1 {
			break
		}
		payload = payload[start+len(annexBStartCode):]
		next := bytes.Index(payload, annexBStartCode)
		if next == -1 {
			nalus = append(nalus, payload)
			break
		}
		nalus = append(nalus, payload[:next])
		payload = payload[next:]
	}
	return nalus
}

func cloneBytes(in []byte) []byte {
	if len(in) == 0 {
		return nil
	}
	out := make([]byte, len(in))
	copy(out, in)
	return out
}

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

package playoutdelay

import (
	"encoding/binary"
	"errors"
)

const (
	PlayoutDelayURI        = "http://www.webrtc.org/experiments/rtp-hdrext/playout-delay"
	MaxPlayoutDelayDefault = 10000            // 10s, equal to chrome's default max playout delay
	PlayoutDelayMaxValue   = 10 * (1<<12 - 1) // max value for playout delay can be represented

	playoutDelayExtensionSize = 3
)

var (
	errPlayoutDelayOverflow = errors.New("playout delay overflow")
	errTooSmall             = errors.New("buffer too small")
)

//  0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
// +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
// |  ID   | len=2 |       MIN delay       |       MAX delay       |
// +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
// The wired MIN/MAX delay is in 10ms unit

type PlayOutDelay struct {
	Min, Max uint16 // delay in ms
}

func PlayoutDelayFromValue(min, max uint16) PlayOutDelay {
	if min > PlayoutDelayMaxValue {
		min = PlayoutDelayMaxValue
	}
	if max > PlayoutDelayMaxValue {
		max = PlayoutDelayMaxValue
	}
	return PlayOutDelay{Min: min, Max: max}
}

func (p PlayOutDelay) Marshal() ([]byte, error) {
	min, max := p.Min/10, p.Max/10
	if min >= 1<<12 || max >= 1<<12 {
		return nil, errPlayoutDelayOverflow
	}

	return []byte{byte(min >> 4), byte(min<<4) | byte(max>>8), byte(max)}, nil
}

func (p *PlayOutDelay) Unmarshal(rawData []byte) error {
	if len(rawData) < playoutDelayExtensionSize {
		return errTooSmall
	}

	p.Min = (binary.BigEndian.Uint16(rawData) >> 4) * 10
	p.Max = (binary.BigEndian.Uint16(rawData[1:]) & 0x0FFF) * 10
	return nil
}

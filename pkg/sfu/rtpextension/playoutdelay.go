package rtpextension

import (
	"encoding/binary"
	"errors"
)

const (
	PlayoutDelayURI = "http://www.webrtc.org/experiments/rtp-hdrext/playout-delay"

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

type PlayOutDelay struct {
	Min, Max uint16 // delay in 10ms
}

func (p PlayOutDelay) Marshal() ([]byte, error) {
	if p.Min >= 1<<12 || p.Max >= 1<<12 {
		return nil, errPlayoutDelayOverflow
	}

	return []byte{byte(p.Min >> 4), byte(p.Min<<4) | byte(p.Max>>8), byte(p.Max)}, nil
}

func (p *PlayOutDelay) Unmarshal(rawData []byte) error {
	if len(rawData) < playoutDelayExtensionSize {
		return errTooSmall
	}

	p.Min = binary.BigEndian.Uint16(rawData) >> 4
	p.Max = binary.BigEndian.Uint16(rawData[1:]) & 0x0FFF
	return nil
}

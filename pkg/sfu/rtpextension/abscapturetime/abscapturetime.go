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

package abscapturetime

import (
	"encoding/binary"
	"errors"
	"time"

	"github.com/livekit/mediatransportutil"
)

const (
	AbsCaptureTimeURI = "http://www.webrtc.org/experiments/rtp-hdrext/abs-capture-time"
)

var (
	errInvalidData = errors.New("invalid data")
	errTooSmall    = errors.New("buffer too small")
)

// Reference: https://webrtc.googlesource.com/src/+/refs/heads/main/docs/native-code/rtp-hdrext/abs-capture-time/
//
// Data layout of the shortened version of abs-capture-time with a 1-byte header + 8 bytes of data:
//
//  0                   1                   2                   3
//  0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
// +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
// |  ID   | len=7 |     absolute capture timestamp (bit 0-23)     |
// +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
// |             absolute capture timestamp (bit 24-55)            |
// +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
// |  ... (56-63)  |
// +-+-+-+-+-+-+-+-+
//
//Data layout of the extended version of abs-capture-time with a 1-byte header + 16 bytes of data:
//
//  0                   1                   2                   3
//  0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
// +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
// |  ID   | len=15|     absolute capture timestamp (bit 0-23)     |
// +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
// |             absolute capture timestamp (bit 24-55)            |
// +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
// |  ... (56-63)  |   estimated capture clock offset (bit 0-23)   |
// +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
// |           estimated capture clock offset (bit 24-55)          |
// +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
// |  ... (56-63)  |
// +-+-+-+-+-+-+-+-+

type AbsCaptureTime struct {
	absoluteCaptureTimestamp    mediatransportutil.NtpTime
	estimatedCaptureClockOffset int64
}

func AbsCaptureTimeFromValue(absoluteCaptureTimestamp uint64, estimatedCaptureClockOffset int64) *AbsCaptureTime {
	return &AbsCaptureTime{
		absoluteCaptureTimestamp:    mediatransportutil.NtpTime(absoluteCaptureTimestamp),
		estimatedCaptureClockOffset: estimatedCaptureClockOffset,
	}
}

func (a *AbsCaptureTime) Rewrite(offset time.Duration) error {
	if a.absoluteCaptureTimestamp == 0 {
		return errInvalidData
	}

	capturedAt := a.absoluteCaptureTimestamp.Time().Add(offset)
	a.absoluteCaptureTimestamp = mediatransportutil.ToNtpTime(capturedAt)
	a.estimatedCaptureClockOffset = 0
	return nil
}

func (a *AbsCaptureTime) Marshal() ([]byte, error) {
	if a.absoluteCaptureTimestamp == 0 {
		return nil, errInvalidData
	}

	size := 8
	if a.estimatedCaptureClockOffset != 0 {
		size += 8
	}
	marshalled := make([]byte, size)
	binary.BigEndian.PutUint64(marshalled, uint64(a.absoluteCaptureTimestamp))
	if a.estimatedCaptureClockOffset != 0 {
		binary.BigEndian.PutUint64(marshalled[8:], uint64(a.estimatedCaptureClockOffset))
	}
	return marshalled, nil
}

func (a *AbsCaptureTime) Unmarshal(marshalled []byte) error {
	if len(marshalled) < 8 {
		return errTooSmall
	}

	a.absoluteCaptureTimestamp = mediatransportutil.NtpTime(binary.BigEndian.Uint64(marshalled))
	if len(marshalled) >= 16 {
		a.estimatedCaptureClockOffset = int64(binary.BigEndian.Uint64(marshalled[8:]))
	}
	return nil
}

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

package utils

import (
	"errors"
	"fmt"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/livekit/livekit-server/pkg/sfu/mime"
	"github.com/livekit/protocol/livekit"
)

// Do a fuzzy find for a codec in the list of codecs
// Used for lookup up a codec in an existing list to find a match
func CodecParametersFuzzySearch(needle webrtc.RTPCodecParameters, haystack []webrtc.RTPCodecParameters) (webrtc.RTPCodecParameters, error) {
	// First attempt to match on MimeType + SDPFmtpLine
	for _, c := range haystack {
		if mime.IsMimeTypeStringEqual(c.RTPCodecCapability.MimeType, needle.RTPCodecCapability.MimeType) &&
			c.RTPCodecCapability.SDPFmtpLine == needle.RTPCodecCapability.SDPFmtpLine {
			return c, nil
		}
	}

	// Fallback to just MimeType
	for _, c := range haystack {
		if mime.IsMimeTypeStringEqual(c.RTPCodecCapability.MimeType, needle.RTPCodecCapability.MimeType) {
			return c, nil
		}
	}

	return webrtc.RTPCodecParameters{}, webrtc.ErrCodecNotFound
}

// Given a CodecParameters find the RTX CodecParameters if one exists
func FindRTXPayloadType(needle webrtc.PayloadType, haystack []webrtc.RTPCodecParameters) webrtc.PayloadType {
	aptStr := fmt.Sprintf("apt=%d", needle)
	for _, c := range haystack {
		if aptStr == c.SDPFmtpLine {
			return c.PayloadType
		}
	}

	return webrtc.PayloadType(0)
}

// GetHeaderExtensionID returns the ID of a header extension, or 0 if not found
func GetHeaderExtensionID(extensions []interceptor.RTPHeaderExtension, extension webrtc.RTPHeaderExtensionCapability) int {
	for _, h := range extensions {
		if extension.URI == h.URI {
			return h.ID
		}
	}
	return 0
}

var (
	ErrInvalidRTPVersion      = errors.New("invalid RTP version")
	ErrRTPPayloadTypeMismatch = errors.New("RTP payload type mismatch")
	ErrRTPSSRCMismatch        = errors.New("RTP SSRC mismatch")
)

// ValidateRTPPacket checks for a valid RTP packet and returns an error if fields are incorrect
func ValidateRTPPacket(pkt *rtp.Packet, expectedPayloadType uint8, expectedSSRC uint32) error {
	if pkt.Version != 2 {
		return fmt.Errorf("%w, expected: 2, actual: %d", ErrInvalidRTPVersion, pkt.Version)
	}

	if expectedPayloadType != 0 && pkt.PayloadType != expectedPayloadType {
		return fmt.Errorf("%w, expected: %d, actual: %d", ErrRTPPayloadTypeMismatch, expectedPayloadType, pkt.PayloadType)
	}

	if expectedSSRC != 0 && pkt.SSRC != expectedSSRC {
		return fmt.Errorf("%w, expected: %d, actual: %d", ErrRTPSSRCMismatch, expectedSSRC, pkt.SSRC)
	}

	return nil
}

func IsSimulcastMode(m livekit.VideoLayer_Mode) bool {
	return m == livekit.VideoLayer_ONE_SPATIAL_LAYER_PER_STREAM || m == livekit.VideoLayer_ONE_SPATIAL_LAYER_PER_STREAM_INCOMPLETE_RTCP_SR
}

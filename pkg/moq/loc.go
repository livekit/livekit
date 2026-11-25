// Copyright 2026 LiveKit, Inc.
// SPDX-License-Identifier: Apache-2.0

package moq

import (
	"errors"
	"math/bits"

	"github.com/trickstercache/trickster/v2/pkg/moq/message"
	"github.com/trickstercache/trickster/v2/pkg/moq/wire"

	"github.com/livekit/livekit-server/pkg/moq/media"
)

// LOC public properties, draft-ietf-moq-loc-05 sections 2.3.1–2.3.3.
const (
	timescaleProperty    = 0x08
	frameMarkingProperty = 0x09
	audioLevelProperty   = 0x0c
	timestampProperty    = 0x10
)

// frameProperties describes one elementary sample using public LOC properties.
func frameProperties(timestamp uint64, f media.Frame, video bool) (message.Properties, error) {
	var props []wire.KV
	if video {
		// Both start and end are set for an entire coded frame (RFC 9626).
		mark := byte(0xc0)
		if f.Keyframe {
			mark |= 0x20
		}
		if f.TemporalID >= 0 {
			mark |= byte(f.TemporalID) & 7
		}
		props = append(props, wire.KV{Type: frameMarkingProperty, Bytes: []byte{mark}})
	}
	if f.AudioLevel != nil {
		v := f.AudioLevel.Level & 0x7f
		if f.AudioLevel.Voice {
			v |= 0x80
		}
		props = append(props, wire.KV{Type: audioLevelProperty, Uint: uint64(v)})
	}
	props = append(props, wire.KV{Type: timestampProperty, Uint: timestamp})
	return wire.AppendKVs(nil, props)
}

// readFrameProperties converts a LOC timestamp to microseconds and exposes
// dependency metadata to the RTP packetizer. The payload remains the codec bitstream.
func readFrameProperties(props message.Properties, payload []byte) (media.IngestFrame, error) {
	timestamp, ok := props.Uint(timestampProperty)
	if !ok {
		return media.IngestFrame{}, errors.New("missing LOC timestamp")
	}
	if scale, found := props.Uint(timescaleProperty); found {
		hi, lo := bits.Mul64(timestamp, 1_000_000)
		if scale == 0 || hi >= scale {
			return media.IngestFrame{}, errors.New("invalid LOC timescale or timestamp")
		}
		timestamp, _ = bits.Div64(hi, lo, scale)
	}
	f := media.IngestFrame{CaptureUs: timestamp, Data: payload}
	if kv, found := props.Find(frameMarkingProperty); found {
		if len(kv.Bytes) < 1 || len(kv.Bytes) > 4 {
			return media.IngestFrame{}, errors.New("invalid LOC frame marking")
		}
		f.Keyframe = kv.Bytes[0]&0x20 != 0
		f.TemporalID, f.HasTemporalID = kv.Bytes[0]&7, true
	}
	if n, found := props.Uint(audioLevelProperty); found {
		if n > 255 {
			return media.IngestFrame{}, errors.New("invalid LOC audio level")
		}
		f.AudioLevel = &media.AudioLevel{Level: uint8(n) & 0x7f, Voice: n&0x80 != 0}
	}
	return f, nil
}

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

package media

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/bits"
	"strconv"
	"strings"

	"github.com/livekit/protocol/codecs/mime"
)

// MSF (draft-ietf-moq-msf-02) catalog. LiveKit specifics ride in "livekit"
// extension members, which MSF readers keep or ignore.
const (
	CatalogTrackName = "catalog"
	CatalogVersion   = "draft-02"

	PackagingLOC = "loc"

	RoleVideo = "video"
	RoleAudio = "audio"
)

type Catalog struct {
	Version string         `json:"version"`
	Tracks  []CatalogTrack `json:"tracks"`
}

type CatalogTrack struct {
	Name              string `json:"name"`
	PackagingMetadata string `json:"packagingMetadata,omitempty"`
	Packaging         string `json:"packaging"`
	IsLive            bool   `json:"isLive"`
	Role              string `json:"role,omitempty"`

	Codec         string  `json:"codec,omitempty"`
	Width         int     `json:"width,omitempty"`
	Height        int     `json:"height,omitempty"`
	Framerate     float64 `json:"framerate,omitempty"`
	Bitrate       int     `json:"bitrate,omitempty"`
	Samplerate    int     `json:"samplerate,omitempty"`
	ChannelConfig string  `json:"channelConfig,omitempty"`

	LiveKit *TrackExt `json:"livekit,omitempty"`
}

// TrackExt associates a standard catalog rendition with its LiveKit publication.
type TrackExt struct {
	PublisherSID string `json:"publisherSid"`
	Sid          string `json:"sid"`
}

func (c *Catalog) Marshal() ([]byte, error) {
	if c.Version == "" {
		c.Version = CatalogVersion
	}
	if c.Tracks == nil {
		c.Tracks = []CatalogTrack{}
	}
	return json.Marshal(c)
}

// ParseCatalog decodes the room media catalog.
func ParseCatalog(b []byte) (*Catalog, error) {
	var c Catalog
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// CodecShortName returns the track-name component for a mime type.
func CodecShortName(m mime.MimeType) string {
	switch m {
	case mime.MimeTypeH264:
		return "h264"
	case mime.MimeTypeH265:
		return "h265"
	case mime.MimeTypeVP8:
		return "vp8"
	case mime.MimeTypeVP9:
		return "vp9"
	case mime.MimeTypeAV1:
		return "av1"
	case mime.MimeTypeOpus, mime.MimeTypeRED:
		return "opus"
	case mime.MimeTypePCMU:
		return "pcmu"
	case mime.MimeTypePCMA:
		return "pcma"
	}
	return strings.ToLower(strings.TrimPrefix(m.String(), "video/"))
}

// WebCodecsString returns a WebCodecs codec string for an RTP codec.
func WebCodecsString(m mime.MimeType, fmtp string) string {
	switch m {
	case mime.MimeTypeH264:
		// Annex-B with in-band parameter sets is "avc3"
		if pli := fmtpValue(fmtp, "profile-level-id"); len(pli) == 6 {
			if _, err := hex.DecodeString(pli); err != nil {
				return ""
			}
			return "avc3." + strings.ToLower(pli)
		}
		return "avc3.42e01f"
	case mime.MimeTypeH265:
		// RFC 7798 section 7.1 bounds and defaults for profile_tier_level.
		profile := fmtpNumber(fmtp, "profile-id", 1, 31)
		space := [...]string{"", "A", "B", "C"}[fmtpNumber(fmtp, "profile-space", 0, 3)]
		tier := [...]string{"L", "H"}[fmtpNumber(fmtp, "tier-flag", 0, 1)]
		level := fmtpNumber(fmtp, "level-id", 93, 255)
		compat := uint32(1) << profile
		if raw := fmtpValue(fmtp, "profile-compatibility-indicator"); len(raw) == 8 {
			if n, err := strconv.ParseUint(raw, 16, 32); err == nil {
				compat = bits.Reverse32(uint32(n))
			}
		}
		constraints := []byte{0xb0}
		if raw := fmtpValue(fmtp, "interop-constraints"); len(raw) == 12 {
			if b, err := hex.DecodeString(raw); err == nil {
				constraints = bytes.TrimRight(b, "\x00")
			}
		}
		codec := fmt.Sprintf("hev1.%s%d.%X.%s%d", space, profile, compat, tier, level)
		for _, b := range constraints {
			codec += fmt.Sprintf(".%02X", b)
		}
		return codec
	case mime.MimeTypeVP8:
		return "vp8"
	case mime.MimeTypeVP9:
		// LiveKit negotiates profiles 0 and 1. RTP does not signal a VP9
		// level. The WebM codec binding defines level 00 for that case.
		profile := fmtpNumber(fmtp, "profile-id", 0, 3)
		if profile > 1 {
			return ""
		}
		return fmt.Sprintf("vp09.%02d.00.08", profile)
	case mime.MimeTypeAV1:
		profile := fmtpNumber(fmtp, "profile", 0, 2)
		level := fmtpNumber(fmtp, "level-idx", 5, 31)
		tier := [...]string{"M", "H"}[fmtpNumber(fmtp, "tier", 0, 1)]
		return fmt.Sprintf("av01.%d.%02d%s.08", profile, level, tier)
	case mime.MimeTypeOpus, mime.MimeTypeRED:
		return "opus"
	case mime.MimeTypePCMU:
		return "ulaw"
	case mime.MimeTypePCMA:
		return "alaw"
	}
	return ""
}

// MimeFromCodecString maps a WebCodecs codec string back to a mime type.
func MimeFromCodecString(codec string) (mime.MimeType, bool) {
	c := strings.ToLower(codec)
	switch {
	case c == "h264", strings.HasPrefix(c, "avc1"), strings.HasPrefix(c, "avc3"):
		return mime.MimeTypeH264, true
	case c == "h265", strings.HasPrefix(c, "hev1"), strings.HasPrefix(c, "hvc1"):
		return mime.MimeTypeH265, true
	case c == "vp8":
		return mime.MimeTypeVP8, true
	case strings.HasPrefix(c, "vp09"), c == "vp9":
		return mime.MimeTypeVP9, true
	case strings.HasPrefix(c, "av01"), c == "av1":
		return mime.MimeTypeAV1, true
	case c == "opus":
		return mime.MimeTypeOpus, true
	case c == "ulaw":
		return mime.MimeTypePCMU, true
	case c == "alaw":
		return mime.MimeTypePCMA, true
	}
	return mime.MimeTypeUnknown, false
}

// H264ProfileLevelID extracts profile-level-id from an avc1/avc3 codec string.
func H264ProfileLevelID(codec string) string {
	if i := strings.IndexByte(codec, '.'); i >= 0 && len(codec)-i-1 >= 6 {
		return strings.ToLower(codec[i+1 : i+7])
	}
	return "42e01f"
}

func fmtpValue(fmtp, key string) string {
	for _, kv := range strings.Split(fmtp, ";") {
		kv = strings.TrimSpace(kv)
		if k, v, ok := strings.Cut(kv, "="); ok && strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

func fmtpNumber(fmtp, key string, fallback, maximum int) int {
	value, err := strconv.Atoi(fmtpValue(fmtp, key))
	if err != nil || value < 0 || value > maximum {
		return fallback
	}
	return value
}

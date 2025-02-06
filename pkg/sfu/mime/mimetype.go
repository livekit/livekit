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

package mime

import (
	"strings"

	"github.com/pion/webrtc/v4"
)

/* RAJA-REMOVE
type MimeTypeNumber int

const (
	MimeTypeNumberUnknown MimeTypeNumber = iota
	MimeTypeNumberVP8
	MimeTypeNumberVP9
	MimeTypeNumberH264
	MimeTypeNumberAV1
)

func MatchMimeType(mimeType string) MimeTypeNumber {
	switch len(mimeType) {
	case 9:
		switch mimeType[0] {
		case 'v', 'V':
			switch mimeType[1] {
			case 'i', 'I':
				switch mimeType[2] {
				case 'd', 'D':
					switch mimeType[3] {
					case 'e', 'E':
						switch mimeType[4] {
						case 'o', 'O':
							switch mimeType[5] {
							case '/':
								switch mimeType[6] {
								case 'v', 'V':
									switch mimeType[7] {
									case 'p', 'P':
										switch mimeType[8] {
										case '8':
											return MimeTypeNumberVP8
										case '9':
											return MimeTypeNumberVP9
										}
									}
								case 'a', 'A':
									switch mimeType[7] {
									case 'v', 'V':
										switch mimeType[8] {
										case '1':
											return MimeTypeNumberAV1
										}
									}
								}
							}
						}
					}
				}
			}
		}
	case 10:
		switch mimeType[0] {
		case 'v', 'V':
			switch mimeType[1] {
			case 'i', 'I':
				switch mimeType[2] {
				case 'd', 'D':
					switch mimeType[3] {
					case 'e', 'E':
						switch mimeType[4] {
						case 'o', 'O':
							switch mimeType[5] {
							case '/':
								switch mimeType[6] {
								case 'h', 'H':
									switch mimeType[7] {
									case '2':
										switch mimeType[8] {
										case '6':
											switch mimeType[9] {
											case '4':
												return MimeTypeNumberH264
											}
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
	return MimeTypeNumberUnknown
}
*/

const (
	MimeTypePrefixAudio = "audio/"
	MimeTypePrefixVideo = "video/"
)

type MimeTypeCodec string

const (
	MimeTypeCodecUnknown MimeTypeCodec = "MimeTypeCodecUnknown"
	MimeTypeCodecH264    MimeTypeCodec = "H264"
	MimeTypeCodecH265    MimeTypeCodec = "H265"
	MimeTypeCodecOpus    MimeTypeCodec = "opus"
	MimeTypeCodecRED     MimeTypeCodec = "red"
	MimeTypeCodecVP8     MimeTypeCodec = "VP8"
	MimeTypeCodecVP9     MimeTypeCodec = "VP9"
	MimeTypeCodecAV1     MimeTypeCodec = "AV1"
	MimeTypeCodecG722    MimeTypeCodec = "G722"
	MimeTypeCodecPCMU    MimeTypeCodec = "PCMU"
	MimeTypeCodecPCMA    MimeTypeCodec = "PCMA"
	MimeTypeCodecRTX     MimeTypeCodec = "rtx"
	MimeTypeCodecFlexFEC MimeTypeCodec = "flexfec"
)

func (m MimeTypeCodec) String() string {
	switch m {
	case MimeTypeCodecUnknown:
		return "MimeTypeCodecUnknown"
	case MimeTypeCodecH264:
		return "H264"
	case MimeTypeCodecH265:
		return "H265"
	case MimeTypeCodecOpus:
		return "opus"
	case MimeTypeCodecRED:
		return "red"
	case MimeTypeCodecVP8:
		return "VP8"
	case MimeTypeCodecVP9:
		return "VP9"
	case MimeTypeCodecAV1:
		return "AV1"
	case MimeTypeCodecG722:
		return "G722"
	case MimeTypeCodecPCMU:
		return "PCMU"
	case MimeTypeCodecPCMA:
		return "PCMA"
	case MimeTypeCodecRTX:
		return "rtx"
	case MimeTypeCodecFlexFEC:
		return "flexfec"
	}

	return string(m)
}

func NormalizeMimeTypeCodec(codec string) MimeTypeCodec {
	switch {
	case strings.EqualFold(codec, "h264"):
		return MimeTypeCodecH264
	case strings.EqualFold(codec, "h265"):
		return MimeTypeCodecH265
	case strings.EqualFold(codec, "opus"):
		return MimeTypeCodecOpus
	case strings.EqualFold(codec, "red"):
		return MimeTypeCodecRED
	case strings.EqualFold(codec, "vp8"):
		return MimeTypeCodecVP8
	case strings.EqualFold(codec, "vp9"):
		return MimeTypeCodecVP9
	case strings.EqualFold(codec, "av1"):
		return MimeTypeCodecAV1
	case strings.EqualFold(codec, "g722"):
		return MimeTypeCodecG722
	case strings.EqualFold(codec, "pcmu"):
		return MimeTypeCodecPCMU
	case strings.EqualFold(codec, "pcma"):
		return MimeTypeCodecPCMA
	case strings.EqualFold(codec, "rtx"):
		return MimeTypeCodecRTX
	case strings.EqualFold(codec, "flexfec"):
		return MimeTypeCodecFlexFEC
	}

	return MimeTypeCodecUnknown
}

func IsMimeTypeCodecStringOpus(codec string) bool {
	return NormalizeMimeTypeCodec(codec) == MimeTypeCodecOpus
}

func IsMimeTypeCodecStringRED(codec string) bool {
	return NormalizeMimeTypeCodec(codec) == MimeTypeCodecRED
}

func IsMimeTypeCodecStringH264(codec string) bool {
	return NormalizeMimeTypeCodec(codec) == MimeTypeCodecH264
}

type MimeType string

const (
	MimeTypeUnknown MimeType = "MimeTypeUnknown"
	MimeTypeH264    MimeType = MimeTypePrefixVideo + MimeType(MimeTypeCodecH264)
	MimeTypeH265    MimeType = MimeTypePrefixVideo + MimeType(MimeTypeCodecH265)
	MimeTypeOpus    MimeType = MimeTypePrefixAudio + MimeType(MimeTypeCodecOpus)
	MimeTypeRED     MimeType = MimeTypePrefixAudio + MimeType(MimeTypeCodecRED)
	MimeTypeVP8     MimeType = MimeTypePrefixVideo + MimeType(MimeTypeCodecVP8)
	MimeTypeVP9     MimeType = MimeTypePrefixVideo + MimeType(MimeTypeCodecVP9)
	MimeTypeAV1     MimeType = MimeTypePrefixVideo + MimeType(MimeTypeCodecAV1)
	MimeTypeG722    MimeType = MimeTypePrefixAudio + MimeType(MimeTypeCodecG722)
	MimeTypePCMU    MimeType = MimeTypePrefixAudio + MimeType(MimeTypeCodecPCMU)
	MimeTypePCMA    MimeType = MimeTypePrefixAudio + MimeType(MimeTypeCodecPCMA)
	MimeTypeRTX     MimeType = MimeTypePrefixVideo + MimeType(MimeTypeCodecRTX)
	MimeTypeFlexFEC MimeType = MimeTypePrefixVideo + MimeType(MimeTypeCodecFlexFEC)
)

func (m MimeType) String() string {
	switch m {
	case MimeTypeUnknown:
		return "MimeTypeUnknown"
	case MimeTypeH264:
		return webrtc.MimeTypeH264
	case MimeTypeH265:
		return webrtc.MimeTypeH265
	case MimeTypeOpus:
		return webrtc.MimeTypeOpus
	case MimeTypeRED:
		return "audio/red"
	case MimeTypeVP8:
		return webrtc.MimeTypeVP8
	case MimeTypeVP9:
		return webrtc.MimeTypeVP9
	case MimeTypeAV1:
		return webrtc.MimeTypeAV1
	case MimeTypeG722:
		return webrtc.MimeTypeG722
	case MimeTypePCMU:
		return webrtc.MimeTypePCMU
	case MimeTypePCMA:
		return webrtc.MimeTypePCMA
	case MimeTypeRTX:
		return webrtc.MimeTypeRTX
	case MimeTypeFlexFEC:
		return webrtc.MimeTypeFlexFEC
	}

	return string(m)
}

func NormalizeMimeType(mime string) MimeType {
	switch {
	case strings.EqualFold(mime, webrtc.MimeTypeH264):
		return MimeTypeH264
	case strings.EqualFold(mime, webrtc.MimeTypeH265):
		return MimeTypeH265
	case strings.EqualFold(mime, webrtc.MimeTypeOpus):
		return MimeTypeOpus
	case strings.EqualFold(mime, "audio/red"):
		return MimeTypeRED
	case strings.EqualFold(mime, webrtc.MimeTypeVP8):
		return MimeTypeVP8
	case strings.EqualFold(mime, webrtc.MimeTypeVP9):
		return MimeTypeVP9
	case strings.EqualFold(mime, webrtc.MimeTypeAV1):
		return MimeTypeAV1
	case strings.EqualFold(mime, webrtc.MimeTypeG722):
		return MimeTypeG722
	case strings.EqualFold(mime, webrtc.MimeTypePCMU):
		return MimeTypePCMU
	case strings.EqualFold(mime, webrtc.MimeTypePCMA):
		return MimeTypePCMA
	case strings.EqualFold(mime, webrtc.MimeTypeRTX):
		return MimeTypeRTX
	case strings.EqualFold(mime, webrtc.MimeTypeFlexFEC):
		return MimeTypeFlexFEC
	}

	return MimeTypeUnknown
}

func IsMimeTypeStringEqual(mime1 string, mime2 string) bool {
	return NormalizeMimeType(mime1) == NormalizeMimeType(mime2)
}

func IsMimeTypeStringContains(haystack string, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

func IsMimeTypeStringAudio(mime string) bool {
	return strings.HasPrefix(mime, MimeTypePrefixAudio)
}

func IsMimeTypeAudio(mimeType MimeType) bool {
	return strings.HasPrefix(string(mimeType), MimeTypePrefixAudio)
}

func IsMimeTypeStringVideo(mime string) bool {
	return strings.HasPrefix(mime, MimeTypePrefixVideo)
}

func IsMimeTypeVideo(mimeType MimeType) bool {
	return strings.HasPrefix(string(mimeType), MimeTypePrefixVideo)
}

// SVC-TODO: Have to use more conditions to differentiate between
// SVC-TODO: SVC and non-SVC (could be single layer or simulcast).
// SVC-TODO: May only need to differentiate between simulcast and non-simulcast
// SVC-TODO: i. e. may be possible to treat single layer as SVC to get proper/intended functionality.
func IsMimeTypeSVC(mimeType MimeType) bool {
	switch mimeType {
	case MimeTypeAV1, MimeTypeVP9:
		return true
	}
	return false
}

func IsMimeTypeStringSVC(mime string) bool {
	return IsMimeTypeSVC(NormalizeMimeType(mime))
}

func IsMimeTypeStringRED(mime string) bool {
	return NormalizeMimeType(mime) == MimeTypeRED
}

func IsMimeTypeStringOpus(mime string) bool {
	return NormalizeMimeType(mime) == MimeTypeOpus
}

func IsMimeTypeStringRTX(mime string) bool {
	return NormalizeMimeType(mime) == MimeTypeRTX
}

func IsMimeTypeStringVP8(mime string) bool {
	return NormalizeMimeType(mime) == MimeTypeVP8
}

func IsMimeTypeStringH264(mime string) bool {
	return NormalizeMimeType(mime) == MimeTypeH264
}

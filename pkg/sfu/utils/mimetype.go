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
	"strings"
)

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

const (
	MimeTypePrefixAudio = "audio/"
	MimeTypePrefixVideo = "video/"
	MimeTypeSuffixRTX   = "rtx"

	MimeTypeH264     = MimeTypePrefixVideo + "H264"
	MimeTypeH265     = MimeTypePrefixVideo + "H265"
	MimeTypeOpus     = MimeTypePrefixAudio + "opus"
	MimeTypeAudioRed = MimeTypePrefixAudio + "red"
	MimeTypeVP8      = MimeTypePrefixVideo + "VP8"
	MimeTypeVP9      = MimeTypePrefixVideo + "VP9"
	MimeTypeAV1      = MimeTypePrefixVideo + "AV1"
	MimeTypeG722     = MimeTypePrefixAudio + "G722"
	MimeTypePCMU     = MimeTypePrefixAudio + "PCMU"
	MimeTypePCMA     = MimeTypePrefixAudio + "PCMA"
	MimeTypeRTX      = MimeTypePrefixVideo + "rtx"
	MimeTypeFlexFEC  = MimeTypePrefixVideo + "flexfec"
)

func IsMimeTypeAudio(mime string) bool {
	return strings.HasPrefix(NormalizeMimeType(mime), MimeTypePrefixAudio)
}

func IsMimeTypeVideo(mime string) bool {
	return strings.HasPrefix(NormalizeMimeType(mime), MimeTypePrefixVideo)
}

func NormalizeMimeType(mime string) string {
	switch {
	case strings.EqualFold(mime, MimeTypeH264):
		return MimeTypeH264
	case strings.EqualFold(mime, MimeTypeH265):
		return MimeTypeH265
	case strings.EqualFold(mime, MimeTypeOpus):
		return MimeTypeOpus
	case strings.EqualFold(mime, MimeTypeAudioRed):
		return MimeTypeAudioRed
	case strings.EqualFold(mime, MimeTypeVP8):
		return MimeTypeVP8
	case strings.EqualFold(mime, MimeTypeVP9):
		return MimeTypeVP9
	case strings.EqualFold(mime, MimeTypeAV1):
		return MimeTypeAV1
	case strings.EqualFold(mime, MimeTypeG722):
		return MimeTypeG722
	case strings.EqualFold(mime, MimeTypePCMU):
		return MimeTypePCMU
	case strings.EqualFold(mime, MimeTypePCMA):
		return MimeTypePCMA
	case strings.EqualFold(mime, MimeTypeRTX):
		return MimeTypeRTX
	case strings.EqualFold(mime, MimeTypeFlexFEC):
		return MimeTypeFlexFEC
	}

	return strings.ToLower(mime)
}

// SVC-TODO: Have to use more conditions to differentiate between
// SVC-TODO: SVC and non-SVC (could be single layer or simulcast).
// SVC-TODO: May only need to differentiate between simulcast and non-simulcast
// SVC-TODO: i. e. may be possible to treat single layer as SVC to get proper/intended functionality.
func IsSvcCodec(mime string) bool {
	switch MatchMimeType(mime) {
	case MimeTypeNumberAV1, MimeTypeNumberVP9:
		return true
	}
	return false
}

func IsRedCodec(mime string) bool {
	return NormalizeMimeType(mime) == MimeTypeAudioRed
}

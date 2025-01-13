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

type MimeType int

const (
	MimeTypeUnknown MimeType = iota
	MimeTypeVP8
	MimeTypeVP9
	MimeTypeH264
	MimeTypeAV1
)

func MatchMimeType(mimeType string) MimeType {
	switch len(mimeType) {
	default:
		return MimeTypeUnknown
	case 9:
		switch mimeType[0] {
		default:
			return MimeTypeUnknown
		case 'v', 'V':
			switch mimeType[1] {
			default:
				return MimeTypeUnknown
			case 'i', 'I':
				switch mimeType[2] {
				default:
					return MimeTypeUnknown
				case 'd', 'D':
					switch mimeType[3] {
					default:
						return MimeTypeUnknown
					case 'e', 'E':
						switch mimeType[4] {
						default:
							return MimeTypeUnknown
						case 'o', 'O':
							switch mimeType[5] {
							default:
								return MimeTypeUnknown
							case '/':
								switch mimeType[6] {
								default:
									return MimeTypeUnknown
								case 'v', 'V':
									switch mimeType[7] {
									default:
										return MimeTypeUnknown
									case 'p', 'P':
										switch mimeType[8] {
										default:
											return MimeTypeUnknown
										case '8':
											return MimeTypeVP8
										case '9':
											return MimeTypeVP9
										}
									}
								case 'a', 'A':
									switch mimeType[7] {
									default:
										return MimeTypeUnknown
									case 'v', 'V':
										switch mimeType[8] {
										default:
											return MimeTypeUnknown
										case '1':
											return MimeTypeAV1
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
		default:
			return MimeTypeUnknown
		case 'v', 'V':
			switch mimeType[1] {
			default:
				return MimeTypeUnknown
			case 'i', 'I':
				switch mimeType[2] {
				default:
					return MimeTypeUnknown
				case 'd', 'D':
					switch mimeType[3] {
					default:
						return MimeTypeUnknown
					case 'e', 'E':
						switch mimeType[4] {
						default:
							return MimeTypeUnknown
						case 'o', 'O':
							switch mimeType[5] {
							default:
								return MimeTypeUnknown
							case '/':
								switch mimeType[6] {
								default:
									return MimeTypeUnknown
								case 'h', 'H':
									switch mimeType[7] {
									default:
										return MimeTypeUnknown
									case '2':
										switch mimeType[8] {
										default:
											return MimeTypeUnknown
										case '6':
											switch mimeType[9] {
											default:
												return MimeTypeUnknown
											case '4':
												return MimeTypeH264
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
}

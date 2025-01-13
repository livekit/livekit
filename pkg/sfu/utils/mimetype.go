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
											return MimeTypeVP8
										case '9':
											return MimeTypeVP9
										}
									}
								case 'a', 'A':
									switch mimeType[7] {
									case 'v', 'V':
										switch mimeType[8] {
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
	return MimeTypeUnknown
}

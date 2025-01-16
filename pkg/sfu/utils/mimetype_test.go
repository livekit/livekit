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
	"testing"

	"github.com/stretchr/testify/require"
)

func toLowerSwitch(mimeType string) MimeType {
	switch strings.ToLower(mimeType) {
	case "video/vp8":
		return MimeTypeVP8
	case "video/vp9":
		return MimeTypeVP9
	case "video/h264":
		return MimeTypeH264
	case "video/av1":
		return MimeTypeAV1
	default:
		return MimeTypeUnknown
	}
}

func TestMimeTypeMatch(t *testing.T) {
	require.Equal(t, MimeTypeVP8, MatchMimeType("VIDEO/VP8"), "VIDEO/VP8")
	require.Equal(t, MimeTypeVP9, MatchMimeType("VIDEO/VP9"), "VIDEO/VP9")
	require.Equal(t, MimeTypeH264, MatchMimeType("VIDEO/H264"), "VIDEO/H264")
	require.Equal(t, MimeTypeAV1, MatchMimeType("VIDEO/AV1"), "VIDEO/AV1")
}

func BenchmarkMimeTypeMatch(b *testing.B) {
	mimeTypes := []string{
		"video/VP8",
		"video/VP9",
		"video/H264",
		"video/AV1",
	}

	b.Run("ToLower/switch", func(b *testing.B) {
		for i := range b.N {
			_ = toLowerSwitch(mimeTypes[i%len(mimeTypes)])
		}
	})

	b.Run("MatchMimeType", func(b *testing.B) {
		for i := range b.N {
			_ = MatchMimeType(mimeTypes[i%len(mimeTypes)])
		}
	})
}

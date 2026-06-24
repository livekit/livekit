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

package service

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/quic-go/quic-go/quicvarint"
	"github.com/stretchr/testify/require"
)

func TestReadMoQLitePublishGroup(t *testing.T) {
	payload := []byte{0, 0, 0, 1, 0x65, 0x88}

	var data []byte
	data = quicvarint.Append(data, uint64(moqLiteStreamGroup))
	var group []byte
	group = quicvarint.Append(group, 0)
	group = quicvarint.Append(group, 42)
	data = appendTestMoQLiteMessage(data, group)
	data = quicvarint.Append(data, uint64(len(payload)))
	data = append(data, payload...)

	result, err := readMoQLitePublishGroup(bytes.NewReader(data), 1024)
	require.NoError(t, err)
	require.Equal(t, uint64(42), result.sequence)
	require.Equal(t, payload, result.payload)
}

func TestReadMoQLitePublishGroupRejectsOversizePayload(t *testing.T) {
	var data []byte
	data = quicvarint.Append(data, uint64(moqLiteStreamGroup))
	var group []byte
	group = quicvarint.Append(group, 0)
	group = quicvarint.Append(group, 42)
	data = appendTestMoQLiteMessage(data, group)
	data = quicvarint.Append(data, 3)
	data = append(data, []byte{1, 2, 3}...)

	_, err := readMoQLitePublishGroup(bytes.NewReader(data), 2)
	require.ErrorContains(t, err, "payload too large")
}

func TestParseMoQIngestPublishParamsDefaults(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "/moq/v1?role=publish", nil)
	require.NoError(t, err)

	params, err := parseMoQIngestPublishParams(req)
	require.NoError(t, err)
	require.Equal(t, "camera", params.TrackName)
	require.Equal(t, uint32(640), params.Width)
	require.Equal(t, uint32(480), params.Height)
	require.Equal(t, uint32(30), params.FPS)
}

func TestParseMoQIngestPublishParamsRejectsMalformedMetadata(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "/moq/v1?width=wide", nil)
	require.NoError(t, err)

	_, err = parseMoQIngestPublishParams(req)
	require.ErrorContains(t, err, "invalid width")
}

func TestH264AnnexBHasIDR(t *testing.T) {
	require.True(t, h264AnnexBHasIDR([]byte{0, 0, 0, 1, 0x65, 0x88}))
	require.False(t, h264AnnexBHasIDR([]byte{0, 0, 0, 1, 0x61, 0x88}))
}

func appendTestMoQLiteMessage(dst []byte, payload []byte) []byte {
	dst = quicvarint.Append(dst, uint64(len(payload)))
	return append(dst, payload...)
}

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
	"testing"
	"time"

	"github.com/quic-go/quic-go/quicvarint"
	"github.com/stretchr/testify/require"
)

func TestMoQLiteReadSubscribe(t *testing.T) {
	var payload []byte
	payload = quicvarint.Append(payload, 7)
	payload = appendMoQLiteMessage(payload, []byte("moq-local-video"))
	payload = appendMoQLiteMessage(payload, []byte("camera"))
	payload = append(payload, 3)
	payload = append(payload, 1)
	payload = quicvarint.Append(payload, 250)
	payload = quicvarint.Append(payload, 10)
	payload = quicvarint.Append(payload, 20)

	data := appendMoQLiteMessage(nil, payload)
	subscribe, err := readMoQLiteSubscribe(quicvarint.NewReader(bytes.NewReader(data)))
	require.NoError(t, err)
	require.Equal(t, uint64(7), subscribe.ID)
	require.Equal(t, "moq-local-video", subscribe.Broadcast)
	require.Equal(t, "camera", subscribe.Track)
	require.Equal(t, uint8(3), subscribe.Priority)
	require.True(t, subscribe.Ordered)
	require.Equal(t, uint64(250), subscribe.MaxLatency)
	require.True(t, subscribe.HasStart)
	require.Equal(t, uint64(9), subscribe.StartGroup)
	require.True(t, subscribe.HasEnd)
	require.Equal(t, uint64(19), subscribe.EndGroup)
}

func TestMoQLiteWriteGroup(t *testing.T) {
	w := &moqLiteTestWriter{}
	err := writeMoQLiteGroup(w, time.Second, 7, 42, []byte{0x00, 0x00, 0x01, 0x65})
	require.NoError(t, err)

	reader := quicvarint.NewReader(bytes.NewReader(w.Bytes()))
	streamType, err := reader.ReadByte()
	require.NoError(t, err)
	require.Equal(t, moqLiteStreamGroup, streamType)

	groupData, err := readMoQLiteMessage(reader)
	require.NoError(t, err)
	groupReader := quicvarint.NewReader(bytes.NewReader(groupData))
	subscribeID, err := quicvarint.Read(groupReader)
	require.NoError(t, err)
	require.Equal(t, uint64(7), subscribeID)
	sequence, err := quicvarint.Read(groupReader)
	require.NoError(t, err)
	require.Equal(t, uint64(42), sequence)

	frame, err := readMoQLiteMessage(reader)
	require.NoError(t, err)
	require.Equal(t, []byte{0x00, 0x00, 0x01, 0x65}, frame)
}

type moqLiteTestWriter struct {
	bytes.Buffer
}

func (w *moqLiteTestWriter) SetWriteDeadline(time.Time) error {
	return nil
}

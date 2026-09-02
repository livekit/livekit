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

package pacer

import (
	"testing"
	"time"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/mono"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

type testTrackLocalWriter struct{}

func (testTrackLocalWriter) WriteRTP(header *rtp.Header, payload []byte) (int, error) {
	return header.MarshalSize() + len(payload), nil
}

func (testTrackLocalWriter) Write(b []byte) (int, error) {
	return len(b), nil
}

func TestBaseSendPacketTracksActivityWithoutHeaderExtensions(t *testing.T) {
	b := NewBase(logger.GetLogger(), nil)
	b.lastPacketSentAt.Store(mono.UnixNano() - int64(time.Second))

	p := &Packet{
		Header:      &rtp.Header{Version: 2},
		HeaderSize: 12,
		Payload:     []byte{1, 2, 3, 4},
		WriteStream: testTrackLocalWriter{},
	}

	_, err := b.SendPacket(p)
	require.NoError(t, err)
	require.Less(t, b.TimeSinceLastSentPacket(), 100*time.Millisecond)
}

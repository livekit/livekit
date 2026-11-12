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

package signalling

import (
	"testing"

	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestSignalTrackFECProtection(t *testing.T) {
	for _, level := range []*livekit.FECProtection{nil, livekit.FECProtection_FEC_NONE.Enum(), livekit.FECProtection_FEC_LOW.Enum(), livekit.FECProtection_FEC_MEDIUM.Enum(), livekit.FECProtection_FEC_HIGH.Enum()} {
		participant := &typesfakes.FakeLocalParticipant{}
		handler := NewSignalHandler(SignalHandlerParams{Participant: participant, Logger: logger.GetLogger()})
		signal := &livekit.SignalRequest{Message: &livekit.SignalRequest_TrackSetting{TrackSetting: &livekit.UpdateTrackSettings{
			TrackSids: []string{"track-a", "track-b"}, Fec: level,
		}}}
		wire, err := proto.Marshal(signal)
		require.NoError(t, err)
		decoded := &livekit.SignalRequest{}
		require.NoError(t, proto.Unmarshal(wire, decoded))
		require.NoError(t, handler.HandleMessage(decoded))
		require.Equal(t, 2, participant.UpdateSubscribedTrackSettingsCallCount())
		for i, sid := range []livekit.TrackID{"track-a", "track-b"} {
			actualSID, settings := participant.UpdateSubscribedTrackSettingsArgsForCall(i)
			require.Equal(t, sid, actualSID)
			require.Equal(t, level, settings.Fec)
		}
	}
}

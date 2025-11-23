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

package datatrack

import (
	"testing"

	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtensionParticipantSid(t *testing.T) {
	longTestParticipantID := livekit.ParticipantID(make([]byte, 65536))
	extParticipantSid, err := NewExtensionParticipantSid(longTestParticipantID)
	require.Error(t, err)

	testParticipantID := livekit.ParticipantID("test")
	extParticipantSid, err = NewExtensionParticipantSid(testParticipantID)
	require.NoError(t, err)

	expectedExt := Extension{
		id:   uint16(livekit.DataTrackExtensionID_DTEI_PARTICIPANT_SID),
		data: []byte{'t', 'e', 's', 't'},
	}
	ext, err := extParticipantSid.Marshal()
	require.NoError(t, err)
	require.Equal(t, expectedExt, ext)

	var unmarshaled ExtensionParticipantSid
	err = unmarshaled.Unmarshal(ext)
	require.NoError(t, err)
	assert.Equal(t, testParticipantID, unmarshaled.ParticipantID())
}

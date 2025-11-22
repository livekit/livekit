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
	"errors"

	"github.com/livekit/protocol/livekit"
)

type ExtensionParticipantSid struct {
	participantID livekit.ParticipantID
}

func NewExtensionParticipantSid(participantID livekit.ParticipantID) (*ExtensionParticipantSid, error) {
	if len(participantID) >= 65536 {
		return nil, errors.New("participantID too long")
	}

	return &ExtensionParticipantSid{participantID}, nil
}

func (e *ExtensionParticipantSid) ParticipantID() livekit.ParticipantID {
	return e.participantID
}

func (e *ExtensionParticipantSid) Marshal() (Extension, error) {
	data := make([]byte, len(e.participantID))
	copy(data, e.participantID)
	return Extension{
		id:   uint16(livekit.DataTrackExtensionID_DTEI_PARTICIPANT_SID),
		data: data,
	}, nil
}

func (e *ExtensionParticipantSid) Unmarshal(ext Extension) error {
	if ext.id != uint16(livekit.DataTrackExtensionID_DTEI_PARTICIPANT_SID) {
		return errors.New("invalid extension ID")
	}

	if len(ext.data) == 0 {
		return errors.New("empty extension data")
	}

	e.participantID = livekit.ParticipantID(ext.data)
	return nil
}

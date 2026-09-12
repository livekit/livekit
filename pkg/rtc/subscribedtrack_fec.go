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

package rtc

import (
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils"
)

// mergeSubscriberSettings retains an explicitly requested FEC level across
// legacy clients' quality/mute updates, which omit the new optional field. Clone
// both the incoming message and inherited value so cached settings are immutable.
func mergeSubscriberSettings(previous, next *livekit.UpdateTrackSettings) *livekit.UpdateTrackSettings {
	settings := utils.CloneProto(next)
	fec := settings.Fec
	if fec == nil && previous != nil {
		fec = previous.Fec
	}
	if fec != nil {
		level := *fec
		switch level {
		case livekit.FECProtection_FEC_NONE, livekit.FECProtection_FEC_LOW, livekit.FECProtection_FEC_MEDIUM, livekit.FECProtection_FEC_HIGH:
		default:
			level = livekit.FECProtection_FEC_NONE
		}
		settings.Fec = level.Enum()
	}
	return settings
}

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

// mergeSubscriberSettings retains the FEC protection across updates that omit
// the optional field, such as legacy clients' quality/mute updates. Clone both
// the incoming message and inherited value so cached settings are immutable.
// Unknown modes and levels are kept as sent; the DownTrack interprets them,
// see sfu.DownTrack.SetFECProtection.
func mergeSubscriberSettings(previous, next *livekit.UpdateTrackSettings) *livekit.UpdateTrackSettings {
	settings := utils.CloneProto(next)
	if settings.Fec == nil && previous.GetFec() != nil {
		settings.Fec = utils.CloneProto(previous.Fec)
	}
	return settings
}

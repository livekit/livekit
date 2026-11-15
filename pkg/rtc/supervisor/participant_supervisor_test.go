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

package supervisor

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

// newSupervisorForTest skips the ticker goroutine, tests call checkPublications directly
func newSupervisorForTest() *ParticipantSupervisor {
	s := &ParticipantSupervisor{
		params:       ParticipantSupervisorParams{Logger: logger.GetLogger()},
		publications: make(map[livekit.TrackID]*trackMonitor),
	}
	s.SetPublisherPeerConnectionConnected(true)
	return s
}

func addUnmutedPublication(s *ParticipantSupervisor, trackID livekit.TrackID) {
	s.AddPublication(trackID)
	s.SetPublicationMute(trackID, false)
}

// agePublication moves the publish window start back past the publish wait
func agePublication(s *ParticipantSupervisor, trackID livekit.TrackID) {
	pm := s.publications[trackID].opMon.(*PublicationMonitor)
	pm.lock.Lock()
	pm.unmutedAt = pm.unmutedAt.Add(-2 * publishWaitDuration)
	pm.lock.Unlock()
}

func TestTimedOutPublications(t *testing.T) {
	const trackID = livekit.TrackID("TR_test")

	t.Run("abandoned publication is reported and can be removed", func(t *testing.T) {
		s := newSupervisorForTest()
		addUnmutedPublication(s, trackID)

		s.checkPublications()
		require.Empty(t, s.TimedOutPublications())

		agePublication(s, trackID)
		s.checkPublications()
		require.Equal(t, []livekit.TrackID{trackID}, s.TimedOutPublications())

		s.RemovePublication(trackID)
		require.Empty(t, s.TimedOutPublications())
		require.Empty(t, s.publications)
	})

	t.Run("muted publication is not reported", func(t *testing.T) {
		s := newSupervisorForTest()
		addUnmutedPublication(s, trackID)
		s.SetPublicationMute(trackID, true)

		agePublication(s, trackID)
		s.checkPublications()
		require.Empty(t, s.TimedOutPublications())
	})

	t.Run("stale timeout is not reported once the track publishes", func(t *testing.T) {
		s := newSupervisorForTest()
		addUnmutedPublication(s, trackID)
		addUnmutedPublication(s, trackID)

		agePublication(s, trackID)
		s.checkPublications()
		require.Equal(t, []livekit.TrackID{trackID}, s.TimedOutPublications())

		// media arrives late, before the next tick clears the cached error
		s.SetPublishedTrack(trackID, &typesfakes.FakeLocalMediaTrack{})
		require.Empty(t, s.TimedOutPublications())
	})

	t.Run("publication with a published track is not removed", func(t *testing.T) {
		s := newSupervisorForTest()
		addUnmutedPublication(s, trackID)
		s.SetPublishedTrack(trackID, &typesfakes.FakeLocalMediaTrack{})

		s.RemovePublication(trackID)
		require.Contains(t, s.publications, trackID)
	})

	t.Run("live publication with a queued successor is not reported", func(t *testing.T) {
		s := newSupervisorForTest()
		addUnmutedPublication(s, trackID)
		addUnmutedPublication(s, trackID) // queued successor shares the track id
		s.SetPublishedTrack(trackID, &typesfakes.FakeLocalMediaTrack{})

		agePublication(s, trackID)
		s.checkPublications()
		require.Empty(t, s.TimedOutPublications())
	})
}

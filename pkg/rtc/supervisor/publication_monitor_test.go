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
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
	"github.com/livekit/protocol/logger"
)

func newMonitorForTest() *PublicationMonitor {
	return NewPublicationMonitor(PublicationMonitorParams{
		TrackID:                   "TR_test",
		IsPeerConnectionConnected: true,
		Logger:                    logger.GetLogger(),
	})
}

// addUnmuted adds a pending publish the way the participant does: add, then set mute
func addUnmuted(m *PublicationMonitor) {
	m.PostEvent(types.OperationMonitorEventAddPendingPublication, nil)
	m.PostEvent(types.OperationMonitorEventSetPublicationMute, false)
}

// age moves the publish window start back past the publish wait and returns it
func age(m *PublicationMonitor) time.Time {
	m.lock.Lock()
	defer m.lock.Unlock()
	m.unmutedAt = m.unmutedAt.Add(-2 * publishWaitDuration)
	return m.unmutedAt
}

func getUnmutedAt(m *PublicationMonitor) time.Time {
	m.lock.RLock()
	defer m.lock.RUnlock()
	return m.unmutedAt
}

// Each subtest is one row of the re-arm case table: the event, and whether the
// publish timer restarts because a queued start reached the front.
func TestPublicationMonitorRearm(t *testing.T) {
	t.Run("first add: no re-arm", func(t *testing.T) {
		m := newMonitorForTest()
		m.PostEvent(types.OperationMonitorEventAddPendingPublication, nil)
		require.True(t, getUnmutedAt(m).IsZero())
	})

	t.Run("successor queued before the track publishes: no re-arm", func(t *testing.T) {
		m := newMonitorForTest()
		addUnmuted(m)
		aged := age(m)

		m.PostEvent(types.OperationMonitorEventAddPendingPublication, nil)
		require.Equal(t, aged, getUnmutedAt(m))
		require.ErrorIs(t, m.Check(), errPublishTimeout)
	})

	t.Run("track publishes: no re-arm", func(t *testing.T) {
		m := newMonitorForTest()
		addUnmuted(m)
		addUnmuted(m)
		aged := age(m)

		m.PostEvent(types.OperationMonitorEventSetPublishedTrack, &typesfakes.FakeLocalMediaTrack{})
		require.Equal(t, aged, getUnmutedAt(m))
		require.NoError(t, m.Check()) // front is the live track's end
	})

	t.Run("successor queued while the track is live: no re-arm", func(t *testing.T) {
		m := newMonitorForTest()
		addUnmuted(m)
		m.PostEvent(types.OperationMonitorEventSetPublishedTrack, &typesfakes.FakeLocalMediaTrack{})
		aged := age(m)

		m.PostEvent(types.OperationMonitorEventAddPendingPublication, nil)
		require.Equal(t, aged, getUnmutedAt(m))
		require.NoError(t, m.Check())
	})

	t.Run("track closes with a successor queued: re-arm", func(t *testing.T) {
		m := newMonitorForTest()
		addUnmuted(m)
		addUnmuted(m)
		track := &typesfakes.FakeLocalMediaTrack{}
		m.PostEvent(types.OperationMonitorEventSetPublishedTrack, track)
		age(m)

		m.PostEvent(types.OperationMonitorEventClearPublishedTrack, track)
		require.Less(t, time.Since(getUnmutedAt(m)), publishWaitDuration)
		require.NoError(t, m.Check())

		// the successor still times out once its own window passes without media
		age(m)
		require.ErrorIs(t, m.Check(), errPublishTimeout)
	})

	t.Run("track closes with a muted successor queued: clock cleared", func(t *testing.T) {
		m := newMonitorForTest()
		addUnmuted(m)
		addUnmuted(m)
		track := &typesfakes.FakeLocalMediaTrack{}
		m.PostEvent(types.OperationMonitorEventSetPublishedTrack, track)
		m.PostEvent(types.OperationMonitorEventSetPublicationMute, true)
		age(m)

		m.PostEvent(types.OperationMonitorEventClearPublishedTrack, track)
		require.True(t, getUnmutedAt(m).IsZero())

		// the clock starts on unmute
		m.PostEvent(types.OperationMonitorEventSetPublicationMute, false)
		require.Less(t, time.Since(getUnmutedAt(m)), publishWaitDuration)
	})

	t.Run("track closes with nothing queued: no re-arm", func(t *testing.T) {
		m := newMonitorForTest()
		addUnmuted(m)
		track := &typesfakes.FakeLocalMediaTrack{}
		m.PostEvent(types.OperationMonitorEventSetPublishedTrack, track)
		aged := age(m)

		m.PostEvent(types.OperationMonitorEventClearPublishedTrack, track)
		require.Equal(t, aged, getUnmutedAt(m))
		require.True(t, m.IsIdle())
	})
}

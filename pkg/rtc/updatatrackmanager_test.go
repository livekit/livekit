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
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
)

func newUpDataTrackManagerForTest() (*UpDataTrackManager, *typesfakes.FakeParticipant, *typesfakes.FakeParticipantListener) {
	listener := &typesfakes.FakeParticipantListener{}
	participant := &typesfakes.FakeParticipant{}
	participant.GetParticipantListenerReturns(listener)

	return NewUpDataTrackManager(UpDataTrackManagerParams{
		Logger:      logger.GetLogger(),
		Participant: participant,
	}), participant, listener
}

func newFakeDataTrackForTest(handle uint16) *typesfakes.FakeDataTrack {
	dt := &typesfakes.FakeDataTrack{}
	dt.PubHandleReturns(handle)
	return dt
}

func unpublishedDataTracks(listener *typesfakes.FakeParticipantListener, participant types.Participant, t *testing.T) []types.DataTrack {
	var tracks []types.DataTrack
	for i := range listener.OnDataTrackUnpublishedCallCount() {
		p, dt := listener.OnDataTrackUnpublishedArgsForCall(i)
		require.Equal(t, participant, p)
		tracks = append(tracks, dt)
	}
	return tracks
}

func TestUpDataTrackManagerClose(t *testing.T) {
	t.Run("reports each track unpublished once", func(t *testing.T) {
		u, participant, listener := newUpDataTrackManagerForTest()
		dt1 := newFakeDataTrackForTest(1)
		dt2 := newFakeDataTrackForTest(2)
		u.AddPublishedDataTrack(dt1)
		u.AddPublishedDataTrack(dt2)

		u.Close()

		require.ElementsMatch(t, []types.DataTrack{dt1, dt2}, unpublishedDataTracks(listener, participant, t))
		require.Equal(t, 1, dt1.CloseCallCount())
		require.Equal(t, 1, dt2.CloseCallCount())
		require.Empty(t, u.GetPublishedDataTracks())
	})

	t.Run("second close reports nothing", func(t *testing.T) {
		u, _, listener := newUpDataTrackManagerForTest()
		dt := newFakeDataTrackForTest(1)
		u.AddPublishedDataTrack(dt)

		u.Close()
		u.Close()

		require.Equal(t, 1, listener.OnDataTrackUnpublishedCallCount())
		require.Equal(t, 1, dt.CloseCallCount())
	})

	t.Run("track unpublished before close is not reported again", func(t *testing.T) {
		u, participant, listener := newUpDataTrackManagerForTest()
		dt1 := newFakeDataTrackForTest(1)
		dt2 := newFakeDataTrackForTest(2)
		u.AddPublishedDataTrack(dt1)
		u.AddPublishedDataTrack(dt2)

		u.RemovePublishedDataTrack(dt1)
		u.Close()

		require.Equal(t, []types.DataTrack{dt1, dt2}, unpublishedDataTracks(listener, participant, t))
		require.Equal(t, 1, dt1.CloseCallCount())
		require.Equal(t, 1, dt2.CloseCallCount())
	})

	t.Run("no tracks reports nothing", func(t *testing.T) {
		u, _, listener := newUpDataTrackManagerForTest()

		u.Close()

		require.Zero(t, listener.OnDataTrackUnpublishedCallCount())
	})
}

func TestUpDataTrackManagerPermissionTrackRegistration(t *testing.T) {
	vg := utils.NewDefaultTimedVersionGenerator()
	denyAll := &livekit.SubscriptionPermission{}

	pm := NewSubscriptionPermissionManager(SubscriptionPermissionManagerParams{
		Logger:           logger.GetLogger(),
		VersionGenerator: vg,
	})
	listener := &typesfakes.FakeParticipantListener{}
	participant := &typesfakes.FakeParticipant{}
	participant.GetParticipantListenerReturns(listener)
	u := NewUpDataTrackManager(UpDataTrackManagerParams{
		Logger:      logger.GetLogger(),
		Participant: participant,
		Permissions: pm,
	})

	dt1 := newFakeDataTrackForTest(1)
	dt1.IDReturns("DT_1")
	dt1Replacement := newFakeDataTrackForTest(1)
	dt1Replacement.IDReturns("DT_1_new")
	dt2 := newFakeDataTrackForTest(2)
	dt2.IDReturns("DT_2")

	u.AddPublishedDataTrack(dt1)
	u.AddPublishedDataTrack(dt1Replacement)
	u.AddPublishedDataTrack(dt2)

	require.NoError(t, pm.UpdateSubscriptionPermission(denyAll, vg.Next(), nil))
	require.Zero(t, dt1.RevokeDisallowedSubscribersCallCount())
	require.Equal(t, 1, dt1Replacement.RevokeDisallowedSubscribersCallCount())
	require.Equal(t, 1, dt2.RevokeDisallowedSubscribersCallCount())

	u.RemovePublishedDataTrack(dt1Replacement)
	require.NoError(t, pm.UpdateSubscriptionPermission(denyAll, vg.Next(), nil))
	require.Equal(t, 1, dt1Replacement.RevokeDisallowedSubscribersCallCount())
	require.Equal(t, 2, dt2.RevokeDisallowedSubscribersCallCount())

	u.Close()
	require.NoError(t, pm.UpdateSubscriptionPermission(denyAll, vg.Next(), nil))
	require.Equal(t, 2, dt2.RevokeDisallowedSubscribersCallCount())
}

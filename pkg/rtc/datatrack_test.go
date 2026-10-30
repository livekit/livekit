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

package rtc

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/observability/roomobs"
)

func newTestDataTrackSubscriber(id livekit.ParticipantID, identity livekit.ParticipantIdentity, isRecorder bool) *typesfakes.FakeLocalParticipant {
	sub := &typesfakes.FakeLocalParticipant{}
	sub.IDReturns(id)
	sub.IdentityReturns(identity)
	sub.IsRecorderReturns(isRecorder)
	sub.GetLoggerReturns(logger.GetLogger())
	sub.GetReporterReturns(roomobs.NewNoopParticipantSessionReporter())
	sub.GetTelemetryListenerReturns(&typesfakes.FakeParticipantTelemetryListener{})
	return sub
}

func TestDataTrackRevokeDisallowedSubscribers(t *testing.T) {
	dt := NewDataTrack(
		DataTrackParams{
			Logger:              logger.GetLogger(),
			ParticipantID:       func() livekit.ParticipantID { return "pubID" },
			ParticipantIdentity: "pub",
		},
		&livekit.DataTrackInfo{
			PubHandle: 1,
			Sid:       "DTR_test",
			Name:      "test",
		},
	)
	defer dt.Close()

	allowed := newTestDataTrackSubscriber("allowedID", "allowed", false)
	disallowed := newTestDataTrackSubscriber("disallowedID", "disallowed", false)
	recorder := newTestDataTrackSubscriber("recorderID", "recorder", true)

	for _, sub := range []*typesfakes.FakeLocalParticipant{allowed, disallowed, recorder} {
		_, err := dt.AddSubscriber(sub)
		require.NoError(t, err)
		require.True(t, dt.IsSubscriber(sub.ID()))
	}

	revoked := dt.RevokeDisallowedSubscribers([]livekit.ParticipantIdentity{"allowed"})
	require.Equal(t, []livekit.ParticipantIdentity{"disallowed"}, revoked)

	// disallowed subscriber is removed, allowed and permission exempt (recorder) subscribers are kept
	require.True(t, dt.IsSubscriber(allowed.ID()))
	require.False(t, dt.IsSubscriber(disallowed.ID()))
	require.True(t, dt.IsSubscriber(recorder.ID()))
}

func TestDataTrackDemand(t *testing.T) {
	var (
		lock     sync.Mutex
		reported []bool
	)
	requireReported := func(expected ...bool) {
		require.Eventually(t, func() bool {
			lock.Lock()
			defer lock.Unlock()
			return slices.Equal(expected, reported)
		}, time.Second, 10*time.Millisecond)
	}

	dt := NewDataTrack(
		DataTrackParams{
			Logger:              logger.GetLogger(),
			ParticipantID:       func() livekit.ParticipantID { return "pubID" },
			ParticipantIdentity: "pub",
			OnDemandChanged: func(hasSubscribers bool) {
				lock.Lock()
				reported = append(reported, hasSubscribers)
				lock.Unlock()
			},
		},
		&livekit.DataTrackInfo{
			PubHandle: 1,
			Sid:       "DTR_test",
			Name:      "test",
		},
	)
	defer dt.Close()

	sub := newTestDataTrackSubscriber("subID", "sub", false)
	_, err := dt.AddSubscriber(sub)
	require.NoError(t, err)
	dt.RemoveSubscriber(sub.ID())

	dt.StartDemandUpdates()
	requireReported(false)

	_, err = dt.AddSubscriber(sub)
	require.NoError(t, err)
	dt.NotifySubscriptionNode("nodeB", true)
	dt.RemoveSubscriber(sub.ID())
	requireReported(false, true)

	dt.NotifySubscriptionNode("nodeB", false)
	requireReported(false, true, false)

	dt.NotifySubscriptionNode("nodeB", true)
	dt.NotifySubscriptionNode("nodeC", true)
	requireReported(false, true, false, true)
	dt.ClearSubscriberNodes()
	requireReported(false, true, false, true, false)
}

func TestDataTrackGetAllSubscribers(t *testing.T) {
	dt := NewDataTrack(
		DataTrackParams{
			Logger:              logger.GetLogger(),
			ParticipantID:       func() livekit.ParticipantID { return "pubID" },
			ParticipantIdentity: "pub",
		},
		&livekit.DataTrackInfo{
			PubHandle: 1,
			Sid:       "DTR_test",
			Name:      "test",
		},
	)
	defer dt.Close()

	require.Empty(t, dt.GetAllSubscribers())

	subA := newTestDataTrackSubscriber("subA", "a", false)
	subB := newTestDataTrackSubscriber("subB", "b", false)
	for _, sub := range []*typesfakes.FakeLocalParticipant{subA, subB} {
		_, err := dt.AddSubscriber(sub)
		require.NoError(t, err)
	}
	require.ElementsMatch(t, []livekit.ParticipantID{subA.ID(), subB.ID()}, dt.GetAllSubscribers())

	dt.RemoveSubscriber(subA.ID())
	require.Equal(t, []livekit.ParticipantID{subB.ID()}, dt.GetAllSubscribers())
}

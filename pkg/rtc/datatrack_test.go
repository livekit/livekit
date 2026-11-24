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
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/rtc/types"
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

type benchDataTrackParticipant struct {
	types.LocalParticipant
	id       livekit.ParticipantID
	identity livekit.ParticipantIdentity
}

func (p *benchDataTrackParticipant) ID() livekit.ParticipantID             { return p.id }
func (p *benchDataTrackParticipant) Identity() livekit.ParticipantIdentity { return p.identity }
func (p *benchDataTrackParticipant) IsRecorder() bool                      { return false }

func newTestRevokeDataTrack(sid, name string) *DataTrack {
	return NewDataTrack(
		DataTrackParams{
			Logger:              logger.GetLogger(),
			ParticipantID:       func() livekit.ParticipantID { return "pubID" },
			ParticipantIdentity: "pub",
		},
		&livekit.DataTrackInfo{
			PubHandle: 1,
			Sid:       sid,
			Name:      name,
		},
	)
}

func newTestDataTrackAllowedIdentities(n int) []livekit.ParticipantIdentity {
	allowed := make([]livekit.ParticipantIdentity, 0, n)
	for i := range n {
		allowed = append(allowed, livekit.ParticipantIdentity(fmt.Sprintf("sub_%04d", i)))
	}
	return allowed
}

func TestDataTrackRevokeDisallowedSubscribersLargeAllowlist(t *testing.T) {
	dt := newTestRevokeDataTrack("DTR_test", "test")
	defer dt.Close()

	// Empty subscribers returns empty without allocating
	require.Empty(t, dt.RevokeDisallowedSubscribers([]livekit.ParticipantIdentity{"sub_0000"}))

	for i := range 20 {
		id := livekit.ParticipantID(fmt.Sprintf("PA_%04d", i))
		identity := livekit.ParticipantIdentity(fmt.Sprintf("sub_%04d", i))
		_, err := dt.AddSubscriber(newTestDataTrackSubscriber(id, identity, false))
		require.NoError(t, err)
	}
	recorder := newTestDataTrackSubscriber("PA_rec", "rec", true)
	_, err := dt.AddSubscriber(recorder)
	require.NoError(t, err)

	revoked := dt.RevokeDisallowedSubscribers(newTestDataTrackAllowedIdentities(15))
	expected := []livekit.ParticipantIdentity{"sub_0015", "sub_0016", "sub_0017", "sub_0018", "sub_0019"}
	require.ElementsMatch(t, expected, revoked)

	for i := range 15 {
		require.True(t, dt.IsSubscriber(livekit.ParticipantID(fmt.Sprintf("PA_%04d", i))))
	}
	for i := 15; i < 20; i++ {
		require.False(t, dt.IsSubscriber(livekit.ParticipantID(fmt.Sprintf("PA_%04d", i))))
	}
	require.True(t, dt.IsSubscriber(recorder.ID()))
}

func BenchmarkDataTrackRevokeDisallowedSubscribers(b *testing.B) {
	for _, size := range []int{100, 500, 1000} {
		b.Run(fmt.Sprintf("subscribers_%d", size), func(b *testing.B) {
			dt := newTestRevokeDataTrack("DTR_bench", "bench")
			defer dt.Close()

			for i := range size {
				id := livekit.ParticipantID(fmt.Sprintf("PA_%04d", i))
				identity := livekit.ParticipantIdentity(fmt.Sprintf("sub_%04d", i))
				dt.subscribedTracks[id] = subscribedDataTrack{
					subscriber: &benchDataTrackParticipant{id: id, identity: identity},
				}
			}
			allowed := newTestDataTrackAllowedIdentities(size)

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				_ = dt.RevokeDisallowedSubscribers(allowed)
			}
		})
	}
}

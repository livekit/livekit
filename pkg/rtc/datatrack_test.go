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
	"sync"
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

func addTestDataTrackSubscribers(t *testing.T, dt *DataTrack, n int, includeRecorder bool) {
	t.Helper()
	for i := range n {
		id := livekit.ParticipantID(fmt.Sprintf("PA_%04d", i))
		identity := livekit.ParticipantIdentity(fmt.Sprintf("sub_%04d", i))
		_, err := dt.AddSubscriber(newTestDataTrackSubscriber(id, identity, false))
		require.NoError(t, err)
	}
	if includeRecorder {
		_, err := dt.AddSubscriber(newTestDataTrackSubscriber("PA_rec", "rec", true))
		require.NoError(t, err)
	}
}

func TestDataTrackRevokeDisallowedSubscribersLargeAllowlist(t *testing.T) {
	t.Run("empty subscribers", func(t *testing.T) {
		dt := newTestRevokeDataTrack("DTR_empty", "empty")
		defer dt.Close()
		require.Empty(t, dt.RevokeDisallowedSubscribers([]livekit.ParticipantIdentity{"sub_0000"}))
	})

	t.Run("single subscriber with large allowlist", func(t *testing.T) {
		dt := newTestRevokeDataTrack("DTR_single", "single")
		defer dt.Close()
		addTestDataTrackSubscribers(t, dt, 1, false)

		allowed := newTestDataTrackAllowedIdentities(15)
		require.Empty(t, dt.RevokeDisallowedSubscribers(allowed))
		require.True(t, dt.IsSubscriber("PA_0000"))

		revoked := dt.RevokeDisallowedSubscribers(allowed[1:])
		require.Equal(t, []livekit.ParticipantIdentity{"sub_0000"}, revoked)
		require.False(t, dt.IsSubscriber("PA_0000"))
	})

	t.Run("non-nil empty allowlist revokes all non-exempt", func(t *testing.T) {
		dt := newTestRevokeDataTrack("DTR_empty_allow", "empty_allow")
		defer dt.Close()
		addTestDataTrackSubscribers(t, dt, 3, true)

		revoked := dt.RevokeDisallowedSubscribers([]livekit.ParticipantIdentity{})
		require.ElementsMatch(t, []livekit.ParticipantIdentity{"sub_0000", "sub_0001", "sub_0002"}, revoked)
		require.True(t, dt.IsSubscriber("PA_rec"))
		require.Empty(t, dt.RevokeDisallowedSubscribers([]livekit.ParticipantIdentity{}))
	})

	t.Run("threshold boundary and large allowlist", func(t *testing.T) {
		dt := newTestRevokeDataTrack("DTR_test", "test")
		defer dt.Close()
		addTestDataTrackSubscribers(t, dt, 20, true)

		revoked := dt.RevokeDisallowedSubscribers(newTestDataTrackAllowedIdentities(15))
		expected := []livekit.ParticipantIdentity{"sub_0015", "sub_0016", "sub_0017", "sub_0018", "sub_0019"}
		require.ElementsMatch(t, expected, revoked)
		require.Empty(t, dt.RevokeDisallowedSubscribers(newTestDataTrackAllowedIdentities(15)))

		revokedBoundary := dt.RevokeDisallowedSubscribers(newTestDataTrackAllowedIdentities(allowedSubscribersMapThreshold))
		require.Len(t, revokedBoundary, 15-allowedSubscribersMapThreshold)

		for i := range allowedSubscribersMapThreshold {
			require.True(t, dt.IsSubscriber(livekit.ParticipantID(fmt.Sprintf("PA_%04d", i))))
		}
		for i := allowedSubscribersMapThreshold; i < 20; i++ {
			require.False(t, dt.IsSubscriber(livekit.ParticipantID(fmt.Sprintf("PA_%04d", i))))
		}
		require.True(t, dt.IsSubscriber("PA_rec"))
	})
}

func TestDataTrackRevokeDisallowedSubscribersConcurrent(t *testing.T) {
	dt := newTestRevokeDataTrack("DTR_conc", "conc")
	defer dt.Close()

	_, err := dt.AddSubscriber(newTestDataTrackSubscriber("PA_rec", "rec", true))
	require.NoError(t, err)

	allowlists := [][]livekit.ParticipantIdentity{
		newTestDataTrackAllowedIdentities(20),
		newTestDataTrackAllowedIdentities(allowedSubscribersMapThreshold),
		{},
	}

	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := range 30 {
				idx := (worker*7 + i) % 24
				id := livekit.ParticipantID(fmt.Sprintf("PA_%04d", idx))
				identity := livekit.ParticipantIdentity(fmt.Sprintf("sub_%04d", idx))
				_, _ = dt.AddSubscriber(newTestDataTrackSubscriber(id, identity, false))
				if i%3 == 0 {
					dt.RemoveSubscriber(id)
				}
			}
		}(w)
	}

	for r := range 4 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := range 40 {
				_ = dt.RevokeDisallowedSubscribers(allowlists[(worker+i)%len(allowlists)])
			}
		}(r)
	}
	wg.Wait()

	require.True(t, dt.IsSubscriber("PA_rec"))
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

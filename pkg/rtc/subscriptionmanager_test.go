/*
 * Copyright 2023 LiveKit, Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package rtc

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
	"github.com/livekit/livekit-server/pkg/telemetry/telemetryfakes"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

func init() {
	reconcileInterval = 50 * time.Millisecond
	notFoundTimeout = 200 * time.Millisecond
	subscriptionTimeout = 200 * time.Millisecond
}

const (
	subSettleTimeout = 600 * time.Millisecond
	subCheckInterval = 10 * time.Millisecond
)

func TestSubscribe(t *testing.T) {
	t.Run("happy path subscribe", func(t *testing.T) {
		sm := newTestSubscriptionManager()
		defer sm.Close(false)
		resolver := newTestResolver(true, true, "pub", "pubID")
		sm.params.TrackResolver = resolver.Resolve
		subCount := atomic.Int32{}
		failed := atomic.Bool{}
		sm.params.OnTrackSubscribed = func(subTrack types.SubscribedTrack) {
			subCount.Add(1)
		}
		sm.params.OnSubscriptionError = func(trackID livekit.TrackID, fatal bool, err error) {
			failed.Store(true)
		}
		numParticipantSubscribed := atomic.Int32{}
		numParticipantUnsubscribed := atomic.Int32{}
		sm.OnSubscribeStatusChanged(func(pubID livekit.ParticipantID, subscribed bool) {
			if subscribed {
				numParticipantSubscribed.Add(1)
			} else {
				numParticipantUnsubscribed.Add(1)
			}
		})

		sm.SubscribeToTrack("track", false)
		s := sm.subscriptions["track"]
		require.True(t, s.isDesired())
		require.Eventually(t, func() bool {
			return subCount.Load() == 1
		}, subSettleTimeout, subCheckInterval, "track was not subscribed")

		require.NotNil(t, s.getSubscribedTrack())
		require.Len(t, sm.GetSubscribedTracks(), 1)

		require.Eventually(t, func() bool {
			return len(sm.GetSubscribedParticipants()) == 1
		}, subSettleTimeout, subCheckInterval, "GetSubscribedParticipants should have returned one item")
		require.Equal(t, "pubID", string(sm.GetSubscribedParticipants()[0]))

		// ensure telemetry events are sent
		tm := sm.params.Telemetry.(*telemetryfakes.FakeTelemetryService)
		require.Equal(t, 1, tm.TrackSubscribeRequestedCallCount())

		// ensure bound
		setTestSubscribedTrackBound(t, s.getSubscribedTrack())
		require.Eventually(t, func() bool {
			return !s.needsBind()
		}, subSettleTimeout, subCheckInterval, "track was not bound")

		// telemetry event should have been sent
		require.Equal(t, 1, tm.TrackSubscribedCallCount())

		time.Sleep(notFoundTimeout)
		require.False(t, failed.Load())

		resolver.SetPause(true)
		// ensure its resilience after being closed
		setTestSubscribedTrackClosed(t, s.getSubscribedTrack(), false)
		require.Eventually(t, func() bool {
			return s.needsSubscribe()
		}, subSettleTimeout, subCheckInterval, "needs subscribe did not persist across track close")
		resolver.SetPause(false)

		require.Eventually(t, func() bool {
			return s.isDesired() && !s.needsSubscribe()
		}, subSettleTimeout, subCheckInterval, "track was not resubscribed")

		// was subscribed twice, unsubscribed once (due to close)
		require.Eventually(t, func() bool {
			return numParticipantSubscribed.Load() == 2
		}, subSettleTimeout, subCheckInterval, "participant subscribe status was not updated twice")
		require.Equal(t, int32(1), numParticipantUnsubscribed.Load())
	})

	t.Run("no track permission", func(t *testing.T) {
		sm := newTestSubscriptionManager()
		defer sm.Close(false)
		resolver := newTestResolver(false, true, "pub", "pubID")
		sm.params.TrackResolver = resolver.Resolve
		failed := atomic.Bool{}
		sm.params.OnSubscriptionError = func(trackID livekit.TrackID, fatal bool, err error) {
			failed.Store(true)
		}

		sm.SubscribeToTrack("track", false)
		s := sm.subscriptions["track"]
		require.Eventually(t, func() bool {
			return !s.getHasPermission()
		}, subSettleTimeout, subCheckInterval, "should not have permission to subscribe")

		time.Sleep(subscriptionTimeout)

		// should not have called failed callbacks, isDesired remains unchanged
		require.True(t, s.isDesired())
		require.False(t, failed.Load())
		require.True(t, s.needsSubscribe())
		require.Len(t, sm.GetSubscribedTracks(), 0)

		// trackSubscribed telemetry not sent
		tm := sm.params.Telemetry.(*telemetryfakes.FakeTelemetryService)
		require.Equal(t, 1, tm.TrackSubscribeRequestedCallCount())
		require.Equal(t, 0, tm.TrackSubscribedCallCount())

		// give permissions now
		resolver.lock.Lock()
		resolver.hasPermission = true
		resolver.lock.Unlock()

		require.Eventually(t, func() bool {
			return !s.needsSubscribe()
		}, subSettleTimeout, subCheckInterval, "should be subscribed")

		require.Len(t, sm.GetSubscribedTracks(), 1)
	})

	t.Run("publisher left", func(t *testing.T) {
		sm := newTestSubscriptionManager()
		defer sm.Close(false)
		resolver := newTestResolver(true, true, "pub", "pubID")
		sm.params.TrackResolver = resolver.Resolve
		failed := atomic.Bool{}
		sm.params.OnSubscriptionError = func(trackID livekit.TrackID, fatal bool, err error) {
			failed.Store(true)
		}

		sm.SubscribeToTrack("track", false)
		s := sm.subscriptions["track"]
		require.Eventually(t, func() bool {
			return !s.needsSubscribe()
		}, subSettleTimeout, subCheckInterval, "should be subscribed")

		resolver.lock.Lock()
		resolver.hasTrack = false
		resolver.lock.Unlock()

		// publisher triggers close
		setTestSubscribedTrackClosed(t, s.getSubscribedTrack(), false)

		require.Eventually(t, func() bool {
			return !s.isDesired()
		}, subSettleTimeout, subCheckInterval, "isDesired not set to false")
	})
}

func TestUnsubscribe(t *testing.T) {
	sm := newTestSubscriptionManager()
	defer sm.Close(false)
	unsubCount := atomic.Int32{}
	sm.params.OnTrackUnsubscribed = func(subTrack types.SubscribedTrack) {
		unsubCount.Add(1)
	}

	resolver := newTestResolver(true, true, "pub", "pubID")

	s := &trackSubscription{
		trackID:           "track",
		desired:           true,
		subscriberID:      sm.params.Participant.ID(),
		publisherID:       "pubID",
		publisherIdentity: "pub",
		hasPermission:     true,
		bound:             true,
		logger:            logger.GetLogger(),
	}
	// a bunch of unfortunate manual wiring
	res := resolver.Resolve(nil, s.trackID)
	res.TrackChangedNotifier.AddObserver(string(sm.params.Participant.ID()), func() {})
	s.changedNotifier = res.TrackChangedNotifier
	st, err := res.Track.AddSubscriber(sm.params.Participant)
	require.NoError(t, err)
	s.subscribedTrack = st
	st.OnClose(func(isExpectedToResume bool) {
		sm.handleSubscribedTrackClose(s, isExpectedToResume)
	})
	res.Track.(*typesfakes.FakeMediaTrack).RemoveSubscriberCalls(func(pID livekit.ParticipantID, isExpectedToResume bool) {
		setTestSubscribedTrackClosed(t, st, isExpectedToResume)
	})

	sm.lock.Lock()
	sm.subscriptions["track"] = s
	sm.lock.Unlock()

	require.False(t, s.needsSubscribe())
	require.False(t, s.needsUnsubscribe())

	// unsubscribe
	sm.UnsubscribeFromTrack("track")
	require.False(t, s.isDesired())

	require.Eventually(t, func() bool {
		if s.needsUnsubscribe() {
			return false
		}
		if sm.pendingUnsubscribes.Load() != 0 {
			return false
		}
		sm.lock.RLock()
		subLen := len(sm.subscriptions)
		sm.lock.RUnlock()
		if subLen != 0 {
			return false
		}
		return true
	}, subSettleTimeout, subCheckInterval, "Track was not unsubscribed")

	// no traces should be left
	require.Len(t, sm.GetSubscribedTracks(), 0)
	require.False(t, res.TrackChangedNotifier.HasObservers())

	tm := sm.params.Telemetry.(*telemetryfakes.FakeTelemetryService)
	require.Equal(t, 1, tm.TrackUnsubscribedCallCount())
}

func TestSubscribeStatusChanged(t *testing.T) {
	sm := newTestSubscriptionManager()
	defer sm.Close(false)
	resolver := newTestResolver(true, true, "pub", "pubID")
	sm.params.TrackResolver = resolver.Resolve
	numParticipantSubscribed := atomic.Int32{}
	numParticipantUnsubscribed := atomic.Int32{}
	sm.OnSubscribeStatusChanged(func(pubID livekit.ParticipantID, subscribed bool) {
		if subscribed {
			numParticipantSubscribed.Add(1)
		} else {
			numParticipantUnsubscribed.Add(1)
		}
	})

	sm.SubscribeToTrack("track1", false)
	sm.SubscribeToTrack("track2", false)
	s1 := sm.subscriptions["track1"]
	s2 := sm.subscriptions["track2"]
	require.Eventually(t, func() bool {
		return !s1.needsSubscribe() && !s2.needsSubscribe()
	}, subSettleTimeout, subCheckInterval, "track1 and track2 should be subscribed")
	st1 := s1.getSubscribedTrack()
	st1.OnClose(func(isExpectedToResume bool) {
		sm.handleSubscribedTrackClose(s1, isExpectedToResume)
	})
	st2 := s2.getSubscribedTrack()
	st2.OnClose(func(isExpectedToResume bool) {
		sm.handleSubscribedTrackClose(s2, isExpectedToResume)
	})
	st1.MediaTrack().(*typesfakes.FakeMediaTrack).RemoveSubscriberCalls(func(pID livekit.ParticipantID, isExpectedToResume bool) {
		setTestSubscribedTrackClosed(t, st1, isExpectedToResume)
	})
	st2.MediaTrack().(*typesfakes.FakeMediaTrack).RemoveSubscriberCalls(func(pID livekit.ParticipantID, isExpectedToResume bool) {
		setTestSubscribedTrackClosed(t, st2, isExpectedToResume)
	})

	require.Eventually(t, func() bool {
		return numParticipantSubscribed.Load() == 1
	}, subSettleTimeout, subCheckInterval, "should be subscribed to publisher")
	require.Equal(t, int32(0), numParticipantUnsubscribed.Load())
	require.True(t, sm.IsSubscribedTo("pubID"))

	// now unsubscribe track2, no event should be fired
	sm.UnsubscribeFromTrack("track2")
	require.Eventually(t, func() bool {
		return !s2.needsUnsubscribe()
	}, subSettleTimeout, subCheckInterval, "track2 should be unsubscribed")
	require.Equal(t, int32(0), numParticipantUnsubscribed.Load())

	// unsubscribe track1, expect event
	sm.UnsubscribeFromTrack("track1")
	require.Eventually(t, func() bool {
		return !s1.needsUnsubscribe()
	}, subSettleTimeout, subCheckInterval, "track1 should be unsubscribed")
	require.Eventually(t, func() bool {
		return numParticipantUnsubscribed.Load() == 1
	}, subSettleTimeout, subCheckInterval, "should be subscribed to publisher")
	require.False(t, sm.IsSubscribedTo("pubID"))
}

// clients may send update subscribed settings prior to subscription events coming through
// settings should be persisted and used when the subscription does take place.
func TestUpdateSettingsBeforeSubscription(t *testing.T) {
	sm := newTestSubscriptionManager()
	defer sm.Close(false)
	resolver := newTestResolver(true, true, "pub", "pubID")
	sm.params.TrackResolver = resolver.Resolve

	settings := &livekit.UpdateTrackSettings{
		Disabled: true,
		Width:    100,
		Height:   100,
	}
	sm.UpdateSubscribedTrackSettings("track", settings)

	sm.SubscribeToTrack("track", false)

	s := sm.subscriptions["track"]
	require.Eventually(t, func() bool {
		return !s.needsSubscribe()
	}, subSettleTimeout, subCheckInterval, "Track should be subscribed")

	st := s.getSubscribedTrack().(*typesfakes.FakeSubscribedTrack)
	require.Eventually(t, func() bool {
		return st.UpdateSubscriberSettingsCallCount() == 1
	}, subSettleTimeout, subCheckInterval, "UpdateSubscriberSettings should be called once")

	applied, _ := st.UpdateSubscriberSettingsArgsForCall(0)
	require.Equal(t, settings.Disabled, applied.Disabled)
	require.Equal(t, settings.Width, applied.Width)
	require.Equal(t, settings.Height, applied.Height)
}

func TestSubscriptionLimits(t *testing.T) {
	sm := newTestSubscriptionManagerWithParams(testSubscriptionParams{
		SubscriptionLimitAudio: 1,
		SubscriptionLimitVideo: 1,
	})
	defer sm.Close(false)
	resolver := newTestResolver(true, true, "pub", "pubID")
	sm.params.TrackResolver = resolver.Resolve
	subCount := atomic.Int32{}
	failed := atomic.Bool{}
	sm.params.OnTrackSubscribed = func(subTrack types.SubscribedTrack) {
		subCount.Add(1)
	}
	sm.params.OnSubscriptionError = func(trackID livekit.TrackID, fatal bool, err error) {
		failed.Store(true)
	}
	numParticipantSubscribed := atomic.Int32{}
	numParticipantUnsubscribed := atomic.Int32{}
	sm.OnSubscribeStatusChanged(func(pubID livekit.ParticipantID, subscribed bool) {
		if subscribed {
			numParticipantSubscribed.Add(1)
		} else {
			numParticipantUnsubscribed.Add(1)
		}
	})

	sm.SubscribeToTrack("track", false)
	s := sm.subscriptions["track"]
	require.True(t, s.isDesired())
	require.Eventually(t, func() bool {
		return subCount.Load() == 1
	}, subSettleTimeout, subCheckInterval, "track was not subscribed")

	require.NotNil(t, s.getSubscribedTrack())
	require.Len(t, sm.GetSubscribedTracks(), 1)

	require.Eventually(t, func() bool {
		return len(sm.GetSubscribedParticipants()) == 1
	}, subSettleTimeout, subCheckInterval, "GetSubscribedParticipants should have returned one item")
	require.Equal(t, "pubID", string(sm.GetSubscribedParticipants()[0]))

	// ensure telemetry events are sent
	tm := sm.params.Telemetry.(*telemetryfakes.FakeTelemetryService)
	require.Equal(t, 1, tm.TrackSubscribeRequestedCallCount())

	// ensure bound
	setTestSubscribedTrackBound(t, s.getSubscribedTrack())
	require.Eventually(t, func() bool {
		return !s.needsBind()
	}, subSettleTimeout, subCheckInterval, "track was not bound")

	// telemetry event should have been sent
	require.Equal(t, 1, tm.TrackSubscribedCallCount())

	// reach subscription limit, subscribe pending
	sm.SubscribeToTrack("track2", false)
	s2 := sm.subscriptions["track2"]
	time.Sleep(subscriptionTimeout * 2)
	require.True(t, s2.needsSubscribe())
	require.Equal(t, 2, tm.TrackSubscribeRequestedCallCount())
	require.Equal(t, 1, tm.TrackSubscribeFailedCallCount())
	require.Len(t, sm.GetSubscribedTracks(), 1)

	// unsubscribe track1, then track2 should be subscribed
	sm.UnsubscribeFromTrack("track")
	require.False(t, s.isDesired())
	require.True(t, s.needsUnsubscribe())
	// wait for unsubscribe to take effect
	time.Sleep(reconcileInterval)
	setTestSubscribedTrackClosed(t, s.getSubscribedTrack(), false)
	require.Nil(t, s.getSubscribedTrack())

	time.Sleep(reconcileInterval)
	require.True(t, s2.isDesired())
	require.False(t, s2.needsSubscribe())
	require.EqualValues(t, 2, subCount.Load())
	require.NotNil(t, s2.getSubscribedTrack())
	require.Equal(t, 2, tm.TrackSubscribeRequestedCallCount())
	require.Len(t, sm.GetSubscribedTracks(), 1)

	// ensure bound
	setTestSubscribedTrackBound(t, s2.getSubscribedTrack())
	require.Eventually(t, func() bool {
		return !s2.needsBind()
	}, subSettleTimeout, subCheckInterval, "track was not bound")

	// subscribe to track1 again, which should pending
	sm.SubscribeToTrack("track", false)
	s = sm.subscriptions["track"]
	require.True(t, s.isDesired())
	time.Sleep(subscriptionTimeout * 2)
	require.True(t, s.needsSubscribe())
	require.Equal(t, 3, tm.TrackSubscribeRequestedCallCount())
	require.Equal(t, 2, tm.TrackSubscribeFailedCallCount())
	require.Len(t, sm.GetSubscribedTracks(), 1)
}

type testSubscriptionParams struct {
	SubscriptionLimitAudio int32
	SubscriptionLimitVideo int32
}

func newTestSubscriptionManager() *SubscriptionManager {
	return newTestSubscriptionManagerWithParams(testSubscriptionParams{})
}

func newTestSubscriptionManagerWithParams(params testSubscriptionParams) *SubscriptionManager {
	p := &typesfakes.FakeLocalParticipant{}
	p.CanSubscribeReturns(true)
	p.IDReturns("subID")
	p.IdentityReturns("sub")
	p.KindReturns(livekit.ParticipantInfo_STANDARD)
	return NewSubscriptionManager(SubscriptionManagerParams{
		Participant:         p,
		Logger:              logger.GetLogger(),
		OnTrackSubscribed:   func(subTrack types.SubscribedTrack) {},
		OnTrackUnsubscribed: func(subTrack types.SubscribedTrack) {},
		OnSubscriptionError: func(trackID livekit.TrackID, fatal bool, err error) {},
		TrackResolver: func(sub types.LocalParticipant, trackID livekit.TrackID) types.MediaResolverResult {
			return types.MediaResolverResult{}
		},
		Telemetry:              &telemetryfakes.FakeTelemetryService{},
		SubscriptionLimitAudio: params.SubscriptionLimitAudio,
		SubscriptionLimitVideo: params.SubscriptionLimitVideo,
	})
}

type testResolver struct {
	lock          sync.Mutex
	hasPermission bool
	hasTrack      bool
	pubIdentity   livekit.ParticipantIdentity
	pubID         livekit.ParticipantID

	paused bool
}

func newTestResolver(hasPermission bool, hasTrack bool, pubIdentity livekit.ParticipantIdentity, pubID livekit.ParticipantID) *testResolver {
	return &testResolver{
		hasPermission: hasPermission,
		hasTrack:      hasTrack,
		pubIdentity:   pubIdentity,
		pubID:         pubID,
	}
}

func (t *testResolver) SetPause(paused bool) {
	t.lock.Lock()
	defer t.lock.Unlock()
	t.paused = paused
}

func (t *testResolver) Resolve(_subscriber types.LocalParticipant, trackID livekit.TrackID) types.MediaResolverResult {
	t.lock.Lock()
	defer t.lock.Unlock()
	res := types.MediaResolverResult{
		TrackChangedNotifier: utils.NewChangeNotifier(),
		TrackRemovedNotifier: utils.NewChangeNotifier(),
		HasPermission:        t.hasPermission,
		PublisherID:          t.pubID,
		PublisherIdentity:    t.pubIdentity,
	}
	if t.hasTrack && !t.paused {
		mt := &typesfakes.FakeMediaTrack{}
		st := &typesfakes.FakeSubscribedTrack{}
		st.IDReturns(trackID)
		st.PublisherIDReturns(t.pubID)
		st.PublisherIdentityReturns(t.pubIdentity)
		mt.AddSubscriberCalls(func(sub types.LocalParticipant) (types.SubscribedTrack, error) {
			st.SubscriberReturns(sub)
			return st, nil
		})
		st.MediaTrackReturns(mt)
		res.Track = mt
	}
	return res
}

func setTestSubscribedTrackBound(t *testing.T, st types.SubscribedTrack) {
	fst, ok := st.(*typesfakes.FakeSubscribedTrack)
	require.True(t, ok)

	for i := 0; i < fst.AddOnBindCallCount(); i++ {
		fst.AddOnBindArgsForCall(i)(nil)
	}
}

func setTestSubscribedTrackClosed(t *testing.T, st types.SubscribedTrack, isExpectedToResume bool) {
	fst, ok := st.(*typesfakes.FakeSubscribedTrack)
	require.True(t, ok)

	fst.OnCloseArgsForCall(0)(isExpectedToResume)
}

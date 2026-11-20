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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

// Delay only the first retirement, before it reaches the real observer map.
type delayedObserverRemoval struct {
	types.ChangeNotifier
	once    sync.Once
	entered chan struct{}
	release chan struct{}
	done    chan struct{}
}

func (n *delayedObserverRemoval) RemoveObserver(key string) {
	first := false
	n.once.Do(func() { first = true })
	if first {
		close(n.entered)
		<-n.release
	}
	n.ChangeNotifier.RemoveObserver(key)
	if first {
		close(n.done)
	}
}

func waitForObserverStep(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("observer lifecycle step did not finish")
	}
}

func TestDataTrackResubscribeNotifications(t *testing.T) {
	testResubscribeNotifications(t, true)
}

func TestMediaTrackResubscribeNotifications(t *testing.T) {
	testResubscribeNotifications(t, false)
}

func testResubscribeNotifications(t *testing.T, data bool) {
	for _, kind := range []string{"changed", "removed"} {
		for _, history := range []string{"close while desired", "unsubscribe then subscribe", "fast toggle"} {
			t.Run(kind+"/"+history, func(t *testing.T) {
				room := newRoomWithParticipants(t, testRoomOpts{})
				listener := room.LocalParticipantListener()
				pub := NewMockParticipant("publisher", types.CurrentProtocol, false, true, listener)
				sub := NewMockParticipant("subscriber", types.CurrentProtocol, false, false, listener)
				pub.HasPermissionReturns(true)
				sub.GetTelemetryListenerReturns(&typesfakes.FakeParticipantTelemetryListener{})
				for _, p := range []*typesfakes.FakeLocalParticipant{pub, sub} {
					require.NoError(t, room.Join(p, nil, &ParticipantOptions{AutoSubscribe: true, AutoSubscribeDataTrack: true}, iceServersForRoom))
					p.StateReturns(livekit.ParticipantInfo_ACTIVE)
				}

				sm := newTestSubscriptionManager()
				// Drive the same worker reconciliation steps explicitly so periodic retries
				// cannot hide a lost notification.
				close(sm.closeCh)
				<-sm.doneCh
				sm.closeCh = make(chan struct{})
				sm.params.Participant = sub
				t.Cleanup(func() { sm.Close(false) })
				sub.SubscribeToTrackCalls(sm.SubscribeToTrack)
				sub.UnsubscribeFromTrackCalls(sm.UnsubscribeFromTrack)
				sub.SubscribeToDataTrackCalls(sm.SubscribeToDataTrack)
				sub.UnsubscribeFromDataTrackCalls(sm.UnsubscribeFromDataTrack)

				const trackID livekit.TrackID = "track"
				changed := room.trackManager.GetOrCreateTrackChangeNotifier(trackID)
				removed := room.trackManager.GetOrCreateTrackRemoveNotifier(trackID)
				blocked := types.ChangeNotifier(changed)
				if kind == "removed" {
					blocked = removed
				}
				gate := &delayedObserverRemoval{ChangeNotifier: blocked, entered: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
				release := sync.OnceFunc(func() { close(gate.release) })
				defer release()

				var getSub func() *trackSubscription
				var isSubscribed func() bool
				var reconcile, subscribe, unsubscribe, publish, unpublish, closeDownTrack func()
				if data {
					track := NewDataTrack(DataTrackParams{Logger: logger.GetLogger(), ParticipantID: pub.ID, ParticipantIdentity: pub.Identity()}, &livekit.DataTrackInfo{Sid: string(trackID), PubHandle: 1})
					t.Cleanup(track.Close)
					sm.params.DataTrackResolver = func(p types.LocalParticipant, id livekit.TrackID) types.DataResolverResult {
						res := room.ResolveDataTrackForSubscriber(p, id)
						if res.TrackChangedNotifier == changed && kind == "changed" {
							res.TrackChangedNotifier = gate
						}
						if res.TrackRemovedNotifier == removed && kind == "removed" {
							res.TrackRemovedNotifier = gate
						}
						return res
					}
					getSub = func() *trackSubscription {
						if s := sm.dataTrackSubscriptions[trackID]; s != nil {
							return &s.trackSubscription
						}
						return nil
					}
					isSubscribed = func() bool { return sm.dataTrackSubscriptions[trackID].getDataDownTrack() != nil }
					closeDownTrack = func() { track.RevokeDisallowedSubscribers(nil) }
					reconcile = sm.reconcileDataTrackSubscriptions
					subscribe = func() { sub.SubscribeToDataTrack(trackID) }
					unsubscribe = func() { sub.UnsubscribeFromDataTrack(trackID) }
					publish = func() { listener.OnDataTrackPublished(pub, track) }
					unpublish = func() { listener.OnDataTrackUnpublished(pub, track) }
				} else {
					track := NewMockTrack(livekit.TrackType_AUDIO, "mic")
					track.IDReturns(trackID)
					track.IsOpenReturns(true)
					var st *typesfakes.FakeSubscribedTrack
					track.AddSubscriberCalls(func(p types.LocalParticipant) (types.SubscribedTrack, error) {
						st = &typesfakes.FakeSubscribedTrack{}
						st.IDReturns(trackID)
						st.MediaTrackReturns(track)
						st.SubscriberReturns(p)
						return st, nil
					})
					track.RemoveSubscriberCalls(func(_ livekit.ParticipantID, resume bool) { setTestSubscribedTrackClosed(t, st, resume) })
					sm.params.TrackResolver = func(p types.LocalParticipant, id livekit.TrackID) types.MediaResolverResult {
						res := room.ResolveMediaTrackForSubscriber(p, id)
						if res.TrackChangedNotifier == changed && kind == "changed" {
							res.TrackChangedNotifier = gate
						}
						if res.TrackRemovedNotifier == removed && kind == "removed" {
							res.TrackRemovedNotifier = gate
						}
						return res
					}
					getSub = func() *trackSubscription {
						if s := sm.subscriptions[trackID]; s != nil {
							return &s.trackSubscription
						}
						return nil
					}
					isSubscribed = func() bool { return sm.subscriptions[trackID].getSubscribedTrack() != nil }
					closeDownTrack = func() { track.RemoveSubscriber(sub.ID(), false) }
					reconcile = sm.reconcileSubscriptions
					subscribe = func() { sub.SubscribeToTrack(trackID, false) }
					unsubscribe = func() { sub.UnsubscribeFromTrack(trackID) }
					publish = func() { listener.OnTrackPublished(pub, track) }
					unpublish = func() { listener.OnTrackUnpublished(pub, track) }
				}

				publish() // the room auto-subscribes the joined participant
				reconcile()
				old := getSub()
				require.NotNil(t, old)
				require.True(t, isSubscribed())
				if history == "fast toggle" {
					release()
					unsubscribe()
					subscribe()
					reconcile()
					// If toggling retired the registration, finish that retirement
					// before testing notification delivery.
					old.lock.RLock()
					retired := old.changedNotifier == nil || old.removedNotifier == nil
					old.lock.RUnlock()
					if retired {
						waitForObserverStep(t, gate.entered)
						waitForObserverStep(t, gate.done)
					}
				} else {
					finished := make(chan struct{})
					// Keep the first retirement pending while reconciliation tries to
					// establish the replacement. Synchronous retirement holds it off.
					go func() {
						if history == "close while desired" {
							closeDownTrack()
						} else {
							unsubscribe()
							reconcile()
						}
						close(finished)
					}()
					waitForObserverStep(t, gate.entered)
					replaced := make(chan struct{})
					go func() {
						<-finished
						// Media close completion is asynchronous on unsubscribe.
						for sm.pendingUnsubscribes.Load() != 0 {
							time.Sleep(time.Millisecond)
						}
						reconcile()
						subscribe()
						reconcile()
						close(replaced)
					}()
					select {
					case <-replaced:
					case <-time.After(100 * time.Millisecond):
						// Retirement may have to complete before teardown can return.
					}
					release()
					waitForObserverStep(t, gate.done)
					waitForObserverStep(t, replaced)
				}
				replacement := getSub()
				require.NotNil(t, replacement)
				require.True(t, isSubscribed())

				// Drain requests from setup; only the next notification may queue work.
				for len(sm.reconcileCh) > 0 {
					<-sm.reconcileCh
				}
				for len(sm.reconcileDataTrackCh) > 0 {
					<-sm.reconcileDataTrackCh
				}
				if kind == "changed" {
					room.trackManager.NotifyTrackChanged(trackID)
					require.Eventually(t, func() bool { return len(sm.reconcileCh)+len(sm.reconcileDataTrackCh) > 0 }, time.Second, time.Millisecond, "replacement lost track-change notification")
				} else {
					unpublish()
					require.Eventually(t, func() bool { return !replacement.isDesired() }, time.Second, time.Millisecond, "replacement lost source-removal notification")
				}
			})
		}
	}
}

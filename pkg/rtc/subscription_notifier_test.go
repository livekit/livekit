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

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
	"github.com/livekit/livekit-server/pkg/telemetry"
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
	require.Eventually(t, func() bool {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}, 2*time.Second, time.Millisecond, "observer lifecycle step did not finish")
}

// Drive worker passes explicitly so periodic retries cannot mask a lost notification.
func newManualSubscriptionManager(t *testing.T) *SubscriptionManager {
	sm := newTestSubscriptionManager()
	sm.lock.Lock()
	close(sm.closeCh)
	sm.lock.Unlock()
	<-sm.doneCh
	sm.lock.Lock()
	sm.closeCh = make(chan struct{})
	sm.lock.Unlock()
	t.Cleanup(func() { sm.Close(false) })
	return sm
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
				// Exercise the room's notification paths without starting telemetry workers.
				room := NewRoom(&livekit.Room{Name: "room"}, nil, WebRTCConfig{}, config.DefaultConfig.Room,
					&config.DefaultConfig.Audio, &livekit.ServerInfo{NodeId: "testnode"}, telemetry.NullTelemetryService{}, nil, nil, nil)
				t.Cleanup(func() { room.Close(types.RoomCloseReasonUnknown) })
				listener := room.LocalParticipantListener()
				pub := NewMockParticipant("publisher", types.CurrentProtocol, false, true, listener)
				sub := NewMockParticipant("subscriber", types.CurrentProtocol, false, false, listener)
				pub.HasPermissionReturns(true)
				sub.GetTelemetryListenerReturns(&typesfakes.FakeParticipantTelemetryListener{})
				for _, p := range []*typesfakes.FakeLocalParticipant{pub, sub} {
					require.NoError(t, room.Join(p, nil, &ParticipantOptions{AutoSubscribe: true, AutoSubscribeDataTrack: true}, iceServersForRoom))
					p.StateReturns(livekit.ParticipantInfo_ACTIVE)
				}

				sm := newManualSubscriptionManager(t)
				sm.params.Participant = sub
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

				var downTrackDetached func() bool
				var getSub func() (*trackSubscription, bool)
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
					getSub = func() (*trackSubscription, bool) {
						sm.lock.RLock()
						defer sm.lock.RUnlock()
						if s := sm.dataTrackSubscriptions[trackID]; s != nil {
							return &s.trackSubscription, s.getDataDownTrack() != nil
						}
						return nil, false
					}
					closeDownTrack = func() {
						sm.lock.RLock()
						dt := sm.dataTrackSubscriptions[trackID].getDataDownTrack().(*DataDownTrack)
						sm.lock.RUnlock()
						downTrackDetached = func() bool {
							dt.lock.Lock()
							defer dt.lock.Unlock()
							return dt.onClose == nil
						}
						track.RevokeDisallowedSubscribers(nil)
					}
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
					getSub = func() (*trackSubscription, bool) {
						sm.lock.RLock()
						defer sm.lock.RUnlock()
						if s := sm.subscriptions[trackID]; s != nil {
							return &s.trackSubscription, s.getSubscribedTrack() != nil
						}
						return nil, false
					}
					closeDownTrack = func() {
						oldTrack := st
						downTrackDetached = func() bool {
							n := oldTrack.OnCloseCallCount()
							return n > 0 && oldTrack.OnCloseArgsForCall(n-1) == nil
						}
						track.RemoveSubscriber(sub.ID(), false)
					}
					reconcile = sm.reconcileSubscriptions
					subscribe = func() { sub.SubscribeToTrack(trackID, false) }
					unsubscribe = func() { sub.UnsubscribeFromTrack(trackID) }
					publish = func() { listener.OnTrackPublished(pub, track) }
					unpublish = func() { listener.OnTrackUnpublished(pub, track) }
				}

				publish() // the room auto-subscribes the joined participant
				reconcile()
				old, subscribed := getSub()
				require.NotNil(t, old)
				require.True(t, subscribed)
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
					retiredOnUnsubscribe := false
					if history == "unsubscribe then subscribe" {
						unsubscribe()
						old.lock.RLock()
						retiredOnUnsubscribe = old.changedNotifier == nil || old.removedNotifier == nil
						old.lock.RUnlock()
					}
					finished := make(chan struct{})
					// Keep the first retirement pending while reconciliation tries to
					// establish the replacement. Synchronous retirement holds it off.
					go func() {
						if history == "close while desired" {
							closeDownTrack()
						} else {
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
					// If teardown already exposed an empty slot (or unsubscribe retired
					// the observers), let the replacement finish before releasing cleanup.
					// Otherwise retirement must finish first, so do not wait on a blocked replacement.
					if retiredOnUnsubscribe || (downTrackDetached != nil && downTrackDetached()) {
						waitForObserverStep(t, replaced)
					}
					release()
					waitForObserverStep(t, gate.done)
					waitForObserverStep(t, replaced)
				}
				replacement, subscribed := getSub()
				require.NotNil(t, replacement)
				require.True(t, subscribed)

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

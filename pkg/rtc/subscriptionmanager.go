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
	"context"
	"errors"
	"sync"
	"time"

	"github.com/pion/webrtc/v4/pkg/rtcerr"
	"go.uber.org/atomic"
	"golang.org/x/exp/maps"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
)

// using var instead of const to override in tests
var (
	reconcileInterval = 3 * time.Second
	// amount of time to give up if a track or publisher isn't found
	// ensuring this is longer than iceFailedTimeout so we are certain the participant won't return
	notFoundTimeout = time.Minute
	// amount of time to try otherwise before flagging subscription as failed
	subscriptionTimeout = iceFailedTimeoutTotal
	maxUnsubscribeWait  = time.Second
)

const (
	trackIDForReconcileSubscriptions = livekit.TrackID("subscriptions_reconcile")
)

type SubscriptionManagerParams struct {
	Logger              logger.Logger
	Participant         types.LocalParticipant
	TrackResolver       types.MediaTrackResolver
	OnTrackSubscribed   func(subTrack types.SubscribedTrack)
	OnTrackUnsubscribed func(subTrack types.SubscribedTrack)
	OnSubscriptionError func(trackID livekit.TrackID, fatal bool, err error)
	Telemetry           telemetry.TelemetryService

	SubscriptionLimitVideo, SubscriptionLimitAudio int32

	UseOneShotSignallingMode bool
}

// SubscriptionManager manages a participant's subscriptions
type SubscriptionManager struct {
	params              SubscriptionManagerParams
	lock                sync.RWMutex
	subscriptions       map[livekit.TrackID]*trackSubscription
	pendingUnsubscribes atomic.Int32

	subscribedVideoCount, subscribedAudioCount atomic.Int32

	subscribedTo map[livekit.ParticipantID]map[livekit.TrackID]struct{}
	reconcileCh  chan livekit.TrackID
	closeCh      chan struct{}
	doneCh       chan struct{}

	onSubscribeStatusChanged func(publisherID livekit.ParticipantID, subscribed bool)
}

func NewSubscriptionManager(params SubscriptionManagerParams) *SubscriptionManager {
	m := &SubscriptionManager{
		params:        params,
		subscriptions: make(map[livekit.TrackID]*trackSubscription),
		subscribedTo:  make(map[livekit.ParticipantID]map[livekit.TrackID]struct{}),
		reconcileCh:   make(chan livekit.TrackID, 50),
		closeCh:       make(chan struct{}),
		doneCh:        make(chan struct{}),
	}

	go m.reconcileWorker()
	return m
}

func (m *SubscriptionManager) Close(isExpectedToResume bool) {
	m.lock.Lock()
	if m.isClosed() {
		m.lock.Unlock()
		return
	}
	close(m.closeCh)
	m.lock.Unlock()

	<-m.doneCh

	subTracks := m.GetSubscribedTracks()
	downTracksToClose := make([]*sfu.DownTrack, 0, len(subTracks))
	for _, st := range subTracks {
		m.setDesired(st.ID(), false)
		dt := st.DownTrack()
		// nil check exists primarily for tests
		if dt != nil {
			downTracksToClose = append(downTracksToClose, st.DownTrack())
		}
	}

	if isExpectedToResume {
		for _, dt := range downTracksToClose {
			dt.CloseWithFlush(false)
		}
	} else {
		// flush blocks, so execute in parallel
		for _, dt := range downTracksToClose {
			go dt.CloseWithFlush(true)
		}
	}
}

func (m *SubscriptionManager) isClosed() bool {
	select {
	case <-m.closeCh:
		return true
	default:
		return false
	}
}

func (m *SubscriptionManager) SubscribeToTrack(trackID livekit.TrackID, isSync bool) {
	if m.params.UseOneShotSignallingMode || isSync {
		m.subscribeSynchronous(trackID)
		return
	}

	sub, desireChanged := m.setDesired(trackID, true)
	if sub == nil {
		sLogger := m.params.Logger.WithValues(
			"trackID", trackID,
		)
		sub = newTrackSubscription(m.params.Participant.ID(), trackID, sLogger)

		m.lock.Lock()
		m.subscriptions[trackID] = sub
		m.lock.Unlock()

		sub, desireChanged = m.setDesired(trackID, true)
	}
	if desireChanged {
		sub.logger.Debugw("subscribing to track")
	}

	// always reconcile, since SubscribeToTrack could be called when the track is ready
	m.queueReconcile(trackID)
}

func (m *SubscriptionManager) UnsubscribeFromTrack(trackID livekit.TrackID) {
	if m.params.UseOneShotSignallingMode {
		m.unsubscribeSynchronous(trackID)
		return
	}

	sub, desireChanged := m.setDesired(trackID, false)
	if sub == nil || !desireChanged {
		return
	}

	sub.logger.Debugw("unsubscribing from track")
	m.queueReconcile(trackID)
}

func (m *SubscriptionManager) ClearAllSubscriptions() {
	m.params.Logger.Debugw("clearing all subscriptions")

	if m.params.UseOneShotSignallingMode {
		for _, track := range m.GetSubscribedTracks() {
			m.unsubscribeSynchronous(track.ID())
		}
	}

	m.lock.RLock()
	for _, sub := range m.subscriptions {
		sub.setDesired(false)
	}
	m.lock.RUnlock()
	m.ReconcileAll()
}

func (m *SubscriptionManager) GetSubscribedTracks() []types.SubscribedTrack {
	m.lock.RLock()
	defer m.lock.RUnlock()

	tracks := make([]types.SubscribedTrack, 0, len(m.subscriptions))
	for _, t := range m.subscriptions {
		st := t.getSubscribedTrack()
		if st != nil {
			tracks = append(tracks, st)
		}
	}
	return tracks
}

func (m *SubscriptionManager) IsTrackNameSubscribed(publisherIdentity livekit.ParticipantIdentity, trackName string) bool {
	m.lock.RLock()
	defer m.lock.RUnlock()

	for _, s := range m.subscriptions {
		st := s.getSubscribedTrack()
		if st != nil && st.PublisherIdentity() == publisherIdentity && st.MediaTrack() != nil && st.MediaTrack().Name() == trackName {
			return true
		}
	}
	return false
}

func (m *SubscriptionManager) StopAndGetSubscribedTracksForwarderState() map[livekit.TrackID]*livekit.RTPForwarderState {
	m.lock.RLock()
	defer m.lock.RUnlock()

	states := make(map[livekit.TrackID]*livekit.RTPForwarderState, len(m.subscriptions))
	for trackID, t := range m.subscriptions {
		st := t.getSubscribedTrack()
		if st != nil {
			dt := st.DownTrack()
			if dt != nil {
				state := dt.StopWriteAndGetState()
				if state.ForwarderState != nil {
					states[trackID] = state.ForwarderState
				}
			}
		}
	}
	return states
}

func (m *SubscriptionManager) HasSubscriptions() bool {
	m.lock.RLock()
	defer m.lock.RUnlock()
	for _, s := range m.subscriptions {
		if s.isDesired() {
			return true
		}
	}
	return false
}

func (m *SubscriptionManager) GetSubscribedParticipants() []livekit.ParticipantID {
	m.lock.RLock()
	defer m.lock.RUnlock()

	return maps.Keys(m.subscribedTo)
}

func (m *SubscriptionManager) IsSubscribedTo(participantID livekit.ParticipantID) bool {
	m.lock.RLock()
	defer m.lock.RUnlock()

	_, ok := m.subscribedTo[participantID]
	return ok
}

func (m *SubscriptionManager) UpdateSubscribedTrackSettings(trackID livekit.TrackID, settings *livekit.UpdateTrackSettings) {
	m.lock.Lock()
	sub, ok := m.subscriptions[trackID]
	if !ok {
		sLogger := m.params.Logger.WithValues(
			"trackID", trackID,
		)
		sub = newTrackSubscription(m.params.Participant.ID(), trackID, sLogger)
		m.subscriptions[trackID] = sub
	}
	m.lock.Unlock()

	sub.setSettings(settings)
}

// OnSubscribeStatusChanged callback will be notified when a participant subscribes or unsubscribes to another participant
// it will only fire once per publisher. If current participant is subscribed to multiple tracks from another, this
// callback will only fire once.
func (m *SubscriptionManager) OnSubscribeStatusChanged(fn func(publisherID livekit.ParticipantID, subscribed bool)) {
	m.lock.Lock()
	m.onSubscribeStatusChanged = fn
	m.lock.Unlock()
}

func (m *SubscriptionManager) WaitUntilSubscribed(timeout time.Duration) error {
	expiresAt := time.Now().Add(timeout)
	for expiresAt.After(time.Now()) {
		allSubscribed := true
		m.lock.RLock()
		for _, sub := range m.subscriptions {
			if sub.needsSubscribe() {
				allSubscribed = false
				break
			}
		}
		m.lock.RUnlock()
		if allSubscribed {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}

	return context.DeadlineExceeded
}

func (m *SubscriptionManager) ReconcileAll() {
	m.queueReconcile(trackIDForReconcileSubscriptions)
}

func (m *SubscriptionManager) setDesired(trackID livekit.TrackID, desired bool) (*trackSubscription, bool) {
	m.lock.RLock()
	defer m.lock.RUnlock()

	sub, ok := m.subscriptions[trackID]
	if !ok {
		return nil, false
	}

	return sub, sub.setDesired(desired)
}

func (m *SubscriptionManager) canReconcile() bool {
	p := m.params.Participant
	if m.isClosed() || p.IsClosed() || p.IsDisconnected() {
		return false
	}
	return true
}

func (m *SubscriptionManager) reconcileSubscriptions() {
	var needsToReconcile []*trackSubscription
	m.lock.RLock()
	for _, sub := range m.subscriptions {
		if sub.needsSubscribe() || sub.needsUnsubscribe() || sub.needsBind() || sub.needsCleanup() {
			needsToReconcile = append(needsToReconcile, sub)
		}
	}
	m.lock.RUnlock()

	for _, s := range needsToReconcile {
		m.reconcileSubscription(s)
	}
}

func (m *SubscriptionManager) reconcileSubscription(s *trackSubscription) {
	if !m.canReconcile() {
		return
	}
	if s.needsSubscribe() {
		if m.pendingUnsubscribes.Load() != 0 && s.durationSinceStart() < maxUnsubscribeWait {
			// enqueue this in a bit, after pending unsubscribes are complete
			go func() {
				time.Sleep(time.Duration(sfu.RTPBlankFramesCloseSeconds * float32(time.Second)))
				m.queueReconcile(s.trackID)
			}()
			return
		}

		numAttempts := s.getNumAttempts()
		if numAttempts == 0 {
			m.params.Telemetry.TrackSubscribeRequested(
				context.Background(),
				s.subscriberID,
				&livekit.TrackInfo{
					Sid: string(s.trackID),
				},
			)
		}
		if err := m.subscribe(s); err != nil {
			s.recordAttempt(false)

			switch err {
			case ErrNoTrackPermission, ErrNoSubscribePermission, ErrNoReceiver, ErrNotOpen, ErrSubscriptionLimitExceeded:
				// these are errors that are outside of our control, so we'll keep trying
				// - ErrNoTrackPermission: publisher did not grant subscriber permission, may change any moment
				// - ErrNoSubscribePermission: participant was not granted canSubscribe, may change any moment
				// - ErrNoReceiver: Track is in the process of closing (another local track published to the same instance)
				// - ErrNotOpen: Track is closing or already closed
				// - ErrSubscriptionLimitExceeded: the participant have reached the limit of subscriptions, wait for the other subscription to be unsubscribed
				// We'll still log an event to reflect this in telemetry since it's been too long
				if s.durationSinceStart() > subscriptionTimeout {
					s.maybeRecordError(m.params.Telemetry, s.subscriberID, err, true)
				}
			case ErrTrackNotFound:
				// source track was never published or closed
				// if after timeout we'd unsubscribe from it.
				// this is the *only* case we'd change desired state
				if s.durationSinceStart() > notFoundTimeout {
					s.maybeRecordError(m.params.Telemetry, s.subscriberID, err, true)
					s.logger.Infow("unsubscribing from track after notFoundTimeout", "error", err)
					s.setDesired(false)
					m.queueReconcile(s.trackID)
					m.params.OnSubscriptionError(s.trackID, false, err)
				}
			default:
				// all other errors
				if s.durationSinceStart() > subscriptionTimeout {
					s.logger.Warnw("failed to subscribe, triggering error handler", err,
						"attempt", numAttempts,
					)
					s.maybeRecordError(m.params.Telemetry, s.subscriberID, err, false)
					m.params.OnSubscriptionError(s.trackID, true, err)
				} else {
					s.logger.Debugw("failed to subscribe, retrying",
						"error", err,
						"attempt", numAttempts,
					)
				}
			}
		} else {
			s.recordAttempt(true)
		}

		return
	}

	if s.needsUnsubscribe() {
		if err := m.unsubscribe(s); err != nil {
			s.logger.Warnw("failed to unsubscribe", err)
		}
		// do not remove subscription from map. Wait for subscribed track to close
		// and the callback (handleSubscribedTrackClose) to set the subscribedTrack to nil
		// and the clean up path to handle removing subscription from the subscription map.
		// It is possible that the track is re-published before subscribed track is closed.
		// That could create a new subscription and a duplicate entry in SDP.
		// Waiting for susbcribed track close would ensure that the track is removed from
		// the peer connection before re-published track is re-subscribed and added back to the SDP.
		return
	}

	if s.needsBind() {
		// check bound status, notify error callback if it's not bound
		// if a publisher leaves or closes the source track, SubscribedTrack will be closed as well and it will go
		// back to needsSubscribe state
		if s.durationSinceStart() > subscriptionTimeout {
			s.logger.Warnw("track not bound after timeout", nil)
			s.maybeRecordError(m.params.Telemetry, s.subscriberID, ErrTrackNotBound, false)
			m.params.OnSubscriptionError(s.trackID, true, ErrTrackNotBound)
		}
	}

	m.lock.Lock()
	if s.needsCleanup() {
		s.logger.Debugw("cleanup removing subscription")
		delete(m.subscriptions, s.trackID)
	}
	m.lock.Unlock()
}

// trigger an immediate reconciliation, when trackID is empty, will reconcile all subscriptions
func (m *SubscriptionManager) queueReconcile(trackID livekit.TrackID) {
	select {
	case m.reconcileCh <- trackID:
	default:
		// queue is full, will reconcile based on timer
	}
}

func (m *SubscriptionManager) reconcileWorker() {
	reconcileTicker := time.NewTicker(reconcileInterval)
	defer reconcileTicker.Stop()
	defer close(m.doneCh)

	for {
		select {
		case <-m.closeCh:
			return
		case <-reconcileTicker.C:
			m.reconcileSubscriptions()
		case trackID := <-m.reconcileCh:
			m.lock.Lock()
			s := m.subscriptions[trackID]
			m.lock.Unlock()
			if s != nil {
				m.reconcileSubscription(s)
			} else {
				m.reconcileSubscriptions()
			}
		}
	}
}

func (m *SubscriptionManager) hasCapacityForSubscription(kind livekit.TrackType) bool {
	switch kind {
	case livekit.TrackType_VIDEO:
		if m.params.SubscriptionLimitVideo > 0 && m.subscribedVideoCount.Load() >= m.params.SubscriptionLimitVideo {
			return false
		}

	case livekit.TrackType_AUDIO:
		if m.params.SubscriptionLimitAudio > 0 && m.subscribedAudioCount.Load() >= m.params.SubscriptionLimitAudio {
			return false
		}
	}
	return true
}

func (m *SubscriptionManager) subscribe(s *trackSubscription) error {
	s.logger.Debugw("executing subscribe")

	if !m.params.Participant.CanSubscribe() {
		return ErrNoSubscribePermission
	}

	if kind, ok := s.getKind(); ok && !m.hasCapacityForSubscription(kind) {
		return ErrSubscriptionLimitExceeded
	}

	trackID := s.trackID
	res := m.params.TrackResolver(m.params.Participant, trackID)
	s.logger.Debugw("resolved track", "result", res)

	if res.TrackChangedNotifier != nil && s.setChangedNotifier(res.TrackChangedNotifier) {
		// set callback only when we haven't done it before
		// we set the observer before checking for existence of track, so that we may get notified
		// when the track becomes available
		res.TrackChangedNotifier.AddObserver(string(s.subscriberID), func() {
			m.queueReconcile(trackID)
		})
	}
	if res.TrackRemovedNotifier != nil && s.setRemovedNotifier(res.TrackRemovedNotifier) {
		res.TrackRemovedNotifier.AddObserver(string(s.subscriberID), func() {
			// re-resolve the track in case the same track had been re-published
			res := m.params.TrackResolver(m.params.Participant, trackID)
			if res.Track != nil {
				// do not unsubscribe, track is still available
				return
			}
			m.handleSourceTrackRemoved(trackID)
		})
	}

	track := res.Track
	if track == nil {
		return ErrTrackNotFound
	}
	s.trySetKind(track.Kind())
	if !m.hasCapacityForSubscription(track.Kind()) {
		return ErrSubscriptionLimitExceeded
	}

	s.setPublisher(res.PublisherIdentity, res.PublisherID)

	permChanged := s.setHasPermission(res.HasPermission)
	if permChanged {
		m.params.Participant.SendSubscriptionPermissionUpdate(s.getPublisherID(), trackID, res.HasPermission)
	}
	if !res.HasPermission {
		return ErrNoTrackPermission
	}

	subTrack, err := track.AddSubscriber(m.params.Participant)
	if err != nil && !errors.Is(err, errAlreadySubscribed) {
		// ignore error(s): already subscribed
		if !utils.ErrorIsOneOf(err, ErrNoReceiver) {
			// as track resolution could take some time, not logging errors due to waiting for track resolution
			m.params.Logger.Warnw("add subscriber failed", err, "trackID", trackID)
		}
		return err
	}
	if err == errAlreadySubscribed {
		m.params.Logger.Debugw(
			"already subscribed to track",
			"trackID", trackID,
			"subscribedAudioCount", m.subscribedAudioCount.Load(),
			"subscribedVideoCount", m.subscribedVideoCount.Load(),
		)
	}
	if err == nil && subTrack != nil { // subTrack could be nil if already subscribed
		subTrack.OnClose(func(isExpectedToResume bool) {
			m.handleSubscribedTrackClose(s, isExpectedToResume)
		})
		subTrack.AddOnBind(func(err error) {
			if err != nil {
				s.logger.Infow("failed to bind track", "err", err)
				s.maybeRecordError(m.params.Telemetry, s.subscriberID, err, true)
				m.UnsubscribeFromTrack(trackID)
				m.params.OnSubscriptionError(trackID, false, err)
				return
			}
			s.setBound()
			s.maybeRecordSuccess(m.params.Telemetry, s.subscriberID)
		})
		s.setSubscribedTrack(subTrack)

		switch track.Kind() {
		case livekit.TrackType_VIDEO:
			m.subscribedVideoCount.Inc()
		case livekit.TrackType_AUDIO:
			m.subscribedAudioCount.Inc()
		}

		if subTrack.NeedsNegotiation() {
			m.params.Participant.Negotiate(false)
		}

		go m.params.OnTrackSubscribed(subTrack)

		m.params.Logger.Debugw(
			"subscribed to track",
			"trackID", trackID,
			"subscribedAudioCount", m.subscribedAudioCount.Load(),
			"subscribedVideoCount", m.subscribedVideoCount.Load(),
		)
	}

	// add mark the participant as someone we've subscribed to
	firstSubscribe := false
	publisherID := s.getPublisherID()
	m.lock.Lock()
	pTracks := m.subscribedTo[publisherID]
	changedCB := m.onSubscribeStatusChanged
	if pTracks == nil {
		pTracks = make(map[livekit.TrackID]struct{})
		m.subscribedTo[publisherID] = pTracks
		firstSubscribe = true
	}
	pTracks[trackID] = struct{}{}
	m.lock.Unlock()

	if changedCB != nil && firstSubscribe {
		changedCB(publisherID, true)
	}
	return nil
}

func (m *SubscriptionManager) subscribeSynchronous(trackID livekit.TrackID) error {
	m.params.Logger.Debugw("executing subscribe synchronous", "trackID", trackID)

	if !m.params.Participant.CanSubscribe() {
		return ErrNoSubscribePermission
	}

	res := m.params.TrackResolver(m.params.Participant, trackID)
	m.params.Logger.Debugw("resolved track", "trackID", trackID, " result", res)

	track := res.Track
	if track == nil {
		return ErrTrackNotFound
	}

	m.lock.Lock()
	sub, ok := m.subscriptions[trackID]
	if !ok {
		sLogger := m.params.Logger.WithValues(
			"trackID", trackID,
		)
		sub = newTrackSubscription(m.params.Participant.ID(), trackID, sLogger)
		m.subscriptions[trackID] = sub
	}
	m.lock.Unlock()
	sub.setDesired(true)

	subTrack, err := track.AddSubscriber(m.params.Participant)
	if err != nil && !errors.Is(err, errAlreadySubscribed) {
		return err
	}
	if err == errAlreadySubscribed {
		m.params.Logger.Debugw(
			"already subscribed to track",
			"trackID", trackID,
			"subscribedAudioCount", m.subscribedAudioCount.Load(),
			"subscribedVideoCount", m.subscribedVideoCount.Load(),
		)
	}
	if err == nil && subTrack != nil { // subTrack could be nil if already subscribed
		subTrack.OnClose(func(isExpectedToResume bool) {
			m.handleSubscribedTrackClose(sub, isExpectedToResume)

			m.lock.Lock()
			delete(m.subscriptions, trackID)
			m.lock.Unlock()
		})
		subTrack.AddOnBind(func(err error) {
			if err != nil {
				sub.logger.Infow("failed to bind track", "err", err)
				sub.maybeRecordError(m.params.Telemetry, sub.subscriberID, err, true)
				m.UnsubscribeFromTrack(trackID)
				m.params.OnSubscriptionError(trackID, false, err)
				return
			}
			sub.setBound()
			sub.maybeRecordSuccess(m.params.Telemetry, sub.subscriberID)
		})
		sub.setSubscribedTrack(subTrack)

		switch track.Kind() {
		case livekit.TrackType_VIDEO:
			m.subscribedVideoCount.Inc()
		case livekit.TrackType_AUDIO:
			m.subscribedAudioCount.Inc()
		}

		go m.params.OnTrackSubscribed(subTrack)

		m.params.Logger.Debugw(
			"subscribed to track",
			"trackID", trackID,
			"subscribedAudioCount", m.subscribedAudioCount.Load(),
			"subscribedVideoCount", m.subscribedVideoCount.Load(),
		)
	}
	return nil
}

func (m *SubscriptionManager) unsubscribe(s *trackSubscription) error {
	s.logger.Debugw("executing unsubscribe")

	subTrack := s.getSubscribedTrack()
	if subTrack == nil {
		// already unsubscribed
		return nil
	}

	track := subTrack.MediaTrack()
	pID := s.subscriberID
	m.pendingUnsubscribes.Inc()
	go func() {
		defer m.pendingUnsubscribes.Dec()
		track.RemoveSubscriber(pID, false)
	}()

	return nil
}

func (m *SubscriptionManager) unsubscribeSynchronous(trackID livekit.TrackID) error {
	m.lock.Lock()
	sub := m.subscriptions[trackID]
	delete(m.subscriptions, trackID)
	m.lock.Unlock()
	if sub == nil {
		// already unsubscribed or not subscribed
		return nil
	}

	sub.logger.Debugw("executing unsubscribe synchronous")

	subTrack := sub.getSubscribedTrack()
	if subTrack == nil {
		// already unsubscribed
		return nil
	}

	track := subTrack.MediaTrack()
	track.RemoveSubscriber(sub.subscriberID, false)
	return nil
}

func (m *SubscriptionManager) handleSourceTrackRemoved(trackID livekit.TrackID) {
	m.lock.Lock()
	sub := m.subscriptions[trackID]
	m.lock.Unlock()

	if sub != nil {
		sub.handleSourceTrackRemoved()
	}
}

// DownTrack closing is how the publisher signifies that the subscription is no longer fulfilled
// this could be due to a few reasons:
// - subscriber-initiated unsubscribe
// - UpTrack was closed
// - publisher revoked permissions for the participant
func (m *SubscriptionManager) handleSubscribedTrackClose(s *trackSubscription, isExpectedToResume bool) {
	s.logger.Debugw(
		"subscribed track closed",
		"isExpectedToResume", isExpectedToResume,
	)
	wasBound := s.isBound()
	subTrack := s.getSubscribedTrack()
	if subTrack == nil {
		return
	}
	s.setSubscribedTrack(nil)

	var relieveFromLimits bool
	switch subTrack.MediaTrack().Kind() {
	case livekit.TrackType_VIDEO:
		videoCount := m.subscribedVideoCount.Dec()
		relieveFromLimits = m.params.SubscriptionLimitVideo > 0 && videoCount == m.params.SubscriptionLimitVideo-1
	case livekit.TrackType_AUDIO:
		audioCount := m.subscribedAudioCount.Dec()
		relieveFromLimits = m.params.SubscriptionLimitAudio > 0 && audioCount == m.params.SubscriptionLimitAudio-1
	}

	// remove from subscribedTo
	publisherID := s.getPublisherID()
	lastSubscription := false
	m.lock.Lock()
	changedCB := m.onSubscribeStatusChanged
	pTracks := m.subscribedTo[publisherID]
	if pTracks != nil {
		delete(pTracks, s.trackID)
		if len(pTracks) == 0 {
			delete(m.subscribedTo, publisherID)
			lastSubscription = true
		}
	}
	m.lock.Unlock()
	if changedCB != nil && lastSubscription {
		go changedCB(publisherID, false)
	}

	go m.params.OnTrackUnsubscribed(subTrack)

	// trigger to decrement unsubscribed counter as long as track has been bound
	// Only log an analytics event when
	// * the participant isn't closing
	// * it's not a migration
	if wasBound {
		m.params.Telemetry.TrackUnsubscribed(
			context.Background(),
			s.subscriberID,
			&livekit.TrackInfo{Sid: string(s.trackID), Type: subTrack.MediaTrack().Kind()},
			!isExpectedToResume,
		)

		dt := subTrack.DownTrack()
		if dt != nil {
			stats := dt.GetTrackStats()
			if stats != nil {
				m.params.Telemetry.TrackSubscribeRTPStats(
					context.Background(),
					s.subscriberID,
					s.trackID,
					dt.Mime(),
					stats,
				)
			}
		}
	}

	if !isExpectedToResume {
		sender := subTrack.RTPSender()
		if sender != nil {
			s.logger.Debugw("removing PeerConnection track",
				"kind", subTrack.MediaTrack().Kind(),
			)

			if err := m.params.Participant.RemoveTrackLocal(sender); err != nil {
				if _, ok := err.(*rtcerr.InvalidStateError); !ok {
					// most of these are safe to ignore, since the track state might have already
					// been set to Inactive
					m.params.Logger.Debugw("could not remove remoteTrack from forwarder",
						"error", err,
						"publisher", subTrack.PublisherIdentity(),
						"publisherID", subTrack.PublisherID(),
					)
				}
			}
		}

		m.params.Participant.Negotiate(false)
	} else {
		t := time.Now()
		s.subscribeAt.Store(&t)
	}
	if !m.params.UseOneShotSignallingMode {
		if relieveFromLimits {
			m.queueReconcile(trackIDForReconcileSubscriptions)
		} else {
			m.queueReconcile(s.trackID)
		}
	}
}

// --------------------------------------------------------------------------------------

type trackSubscription struct {
	subscriberID livekit.ParticipantID
	trackID      livekit.TrackID
	logger       logger.Logger

	lock                     sync.RWMutex
	desired                  bool
	publisherID              livekit.ParticipantID
	publisherIdentity        livekit.ParticipantIdentity
	settings                 *livekit.UpdateTrackSettings
	changedNotifier          types.ChangeNotifier
	removedNotifier          types.ChangeNotifier
	hasPermissionInitialized bool
	hasPermission            bool
	subscribedTrack          types.SubscribedTrack
	eventSent                atomic.Bool
	numAttempts              atomic.Int32
	bound                    bool
	kind                     atomic.Pointer[livekit.TrackType]

	// the later of when subscription was requested OR when the first failure was encountered OR when permission is granted
	// this timestamp determines when failures are reported
	subStartedAt atomic.Pointer[time.Time]

	// the timestamp when the subscription was started, will be reset when downtrack is closed with expected resume
	subscribeAt       atomic.Pointer[time.Time]
	succRecordCounter atomic.Int32
}

func newTrackSubscription(subscriberID livekit.ParticipantID, trackID livekit.TrackID, l logger.Logger) *trackSubscription {
	s := &trackSubscription{
		subscriberID: subscriberID,
		trackID:      trackID,
		logger:       l,
	}
	t := time.Now()
	s.subscribeAt.Store(&t)
	return s
}

func (s *trackSubscription) setPublisher(publisherIdentity livekit.ParticipantIdentity, publisherID livekit.ParticipantID) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.publisherID = publisherID
	s.publisherIdentity = publisherIdentity
}

func (s *trackSubscription) getPublisherID() livekit.ParticipantID {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return s.publisherID
}

func (s *trackSubscription) setDesired(desired bool) bool {
	s.lock.Lock()
	defer s.lock.Unlock()

	if desired {
		// as long as user explicitly set it to desired
		// we'll reset the timer so it has sufficient time to reconcile
		t := time.Now()
		s.subStartedAt.Store(&t)
		s.subscribeAt.Store(&t)
	}

	if s.desired == desired {
		return false
	}
	s.desired = desired

	// when no longer desired, we no longer care about change notifications
	if desired {
		// reset attempts
		s.numAttempts.Store(0)
	} else {
		s.setChangedNotifierLocked(nil)
		s.setRemovedNotifierLocked(nil)
	}
	return true
}

// set permission and return true if it has changed
func (s *trackSubscription) setHasPermission(perm bool) bool {
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.hasPermissionInitialized && s.hasPermission == perm {
		return false
	}

	s.hasPermissionInitialized = true
	s.hasPermission = perm
	if s.hasPermission {
		// when permission is granted, reset the timer so it has sufficient time to reconcile
		t := time.Now()
		s.subStartedAt.Store(&t)
		s.subscribeAt.Store(&t)
	}
	return true
}

func (s *trackSubscription) getHasPermission() bool {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return s.hasPermission
}

func (s *trackSubscription) isDesired() bool {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return s.desired
}

func (s *trackSubscription) setSubscribedTrack(track types.SubscribedTrack) {
	s.lock.Lock()
	oldTrack := s.subscribedTrack
	s.subscribedTrack = track
	s.bound = false
	settings := s.settings
	s.lock.Unlock()

	if settings != nil && track != nil {
		s.logger.Debugw("restoring subscriber settings", "settings", logger.Proto(settings))
		track.UpdateSubscriberSettings(settings, true)
	}
	if oldTrack != nil {
		oldTrack.OnClose(nil)
	}
}

func (s *trackSubscription) trySetKind(kind livekit.TrackType) {
	s.kind.CompareAndSwap(nil, &kind)
}

func (s *trackSubscription) getKind() (livekit.TrackType, bool) {
	kind := s.kind.Load()
	if kind == nil {
		return livekit.TrackType_AUDIO, false
	}
	return *kind, true
}

func (s *trackSubscription) getSubscribedTrack() types.SubscribedTrack {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return s.subscribedTrack
}

func (s *trackSubscription) setChangedNotifier(notifier types.ChangeNotifier) bool {
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.setChangedNotifierLocked(notifier)
}

func (s *trackSubscription) setChangedNotifierLocked(notifier types.ChangeNotifier) bool {
	if s.changedNotifier == notifier {
		return false
	}

	existing := s.changedNotifier
	s.changedNotifier = notifier

	if existing != nil {
		go existing.RemoveObserver(string(s.subscriberID))
	}
	return true
}

func (s *trackSubscription) setRemovedNotifier(notifier types.ChangeNotifier) bool {
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.setRemovedNotifierLocked(notifier)
}

func (s *trackSubscription) setRemovedNotifierLocked(notifier types.ChangeNotifier) bool {
	if s.removedNotifier == notifier {
		return false
	}

	existing := s.removedNotifier
	s.removedNotifier = notifier

	if existing != nil {
		go existing.RemoveObserver(string(s.subscriberID))
	}
	return true
}

func (s *trackSubscription) setSettings(settings *livekit.UpdateTrackSettings) {
	s.lock.Lock()
	s.settings = settings
	subTrack := s.subscribedTrack
	s.lock.Unlock()
	if subTrack != nil {
		subTrack.UpdateSubscriberSettings(settings, false)
	}
}

// mark the subscription as bound - when we've received the client's answer
func (s *trackSubscription) setBound() {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.bound = true
}

func (s *trackSubscription) isBound() bool {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return s.bound
}

func (s *trackSubscription) recordAttempt(success bool) {
	if !success {
		if s.numAttempts.Load() == 0 {
			// on first failure, we'd want to start the timer
			t := time.Now()
			s.subStartedAt.Store(&t)
		}
		s.numAttempts.Add(1)
	} else {
		s.numAttempts.Store(0)
	}
}

func (s *trackSubscription) getNumAttempts() int32 {
	return s.numAttempts.Load()
}

func (s *trackSubscription) handleSourceTrackRemoved() {
	s.lock.Lock()
	defer s.lock.Unlock()

	// source track removed, we would unsubscribe
	s.logger.Debugw("unsubscribing from track since source track was removed")
	s.desired = false

	s.setChangedNotifierLocked(nil)
	s.setRemovedNotifierLocked(nil)
}

func (s *trackSubscription) maybeRecordError(ts telemetry.TelemetryService, pID livekit.ParticipantID, err error, isUserError bool) {
	if s.eventSent.Swap(true) {
		return
	}

	ts.TrackSubscribeFailed(context.Background(), pID, s.trackID, err, isUserError)
}

func (s *trackSubscription) maybeRecordSuccess(ts telemetry.TelemetryService, pID livekit.ParticipantID) {
	subTrack := s.getSubscribedTrack()
	if subTrack == nil {
		return
	}
	mediaTrack := subTrack.MediaTrack()
	if mediaTrack == nil {
		return
	}

	d := time.Since(*s.subscribeAt.Load())
	s.logger.Debugw("track subscribed", "cost", d.Milliseconds())
	subscriber := subTrack.Subscriber()
	prometheus.RecordSubscribeTime(
		subscriber.GetCountry(),
		mediaTrack.Source(),
		mediaTrack.Kind(),
		d,
		subscriber.GetClientInfo().GetSdk(),
		subscriber.Kind(),
		int(s.succRecordCounter.Inc()),
	)

	eventSent := s.eventSent.Swap(true)

	pi := &livekit.ParticipantInfo{
		Identity: string(subTrack.PublisherIdentity()),
		Sid:      string(subTrack.PublisherID()),
	}
	ts.TrackSubscribed(context.Background(), pID, mediaTrack.ToProto(), pi, !eventSent)
}

func (s *trackSubscription) durationSinceStart() time.Duration {
	t := s.subStartedAt.Load()
	if t == nil {
		return 0
	}
	return time.Since(*t)
}

func (s *trackSubscription) needsSubscribe() bool {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return s.desired && s.subscribedTrack == nil
}

func (s *trackSubscription) needsUnsubscribe() bool {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return !s.desired && s.subscribedTrack != nil
}

func (s *trackSubscription) needsBind() bool {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return s.desired && s.subscribedTrack != nil && !s.bound
}

func (s *trackSubscription) needsCleanup() bool {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return !s.desired && s.subscribedTrack == nil
}

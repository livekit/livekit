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
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/routing/routingfakes"
	"github.com/livekit/livekit-server/pkg/rtc/signalling"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
	"github.com/livekit/livekit-server/pkg/sfu/sfufakes"
	"github.com/livekit/protocol/codecs/mime"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
)

type revokeBarrierDataTrack struct {
	types.DataTrack
	beforeRevoke func()
}

func (d *revokeBarrierDataTrack) RevokeDisallowedSubscribers(allowed []livekit.ParticipantIdentity) []livekit.ParticipantIdentity {
	d.beforeRevoke()
	return d.DataTrack.RevokeDisallowedSubscribers(allowed)
}

type closeDuringDataTrackAdd struct {
	types.DataTrack
	afterAdd func()
}

func (d *closeDuringDataTrackAdd) AddSubscriber(p types.LocalParticipant) (types.DataDownTrack, error) {
	down, err := d.DataTrack.AddSubscriber(p)
	if err == nil {
		d.afterAdd()
	}
	return down, err
}

func newRoomDataTrackParticipants(t *testing.T) (*Room, *ParticipantImpl, *ParticipantImpl) {
	t.Helper()
	room := newRoomWithParticipants(t, testRoomOpts{})
	pub := newParticipantForTest("pub")
	sub := newParticipantForTest("sub")
	for _, p := range []*ParticipantImpl{pub, sub} {
		p.setListener(room.LocalParticipantListener())
		p.params.EnableDataTracks = true
		p.params.ParticipantHelper.(*typesfakes.FakeLocalParticipantHelper).ResolveDataTrackCalls(room.ResolveDataTrackForSubscriber)
		require.NoError(t, room.Join(p, nil, &ParticipantOptions{AutoSubscribeDataTrack: true}, iceServersForRoom))
		p.updateState(livekit.ParticipantInfo_ACTIVE)
		t.Cleanup(func() { p.Close(false, types.ParticipantCloseReasonNone, false) })
	}
	return room, pub, sub
}

func TestDataTrackCloseDuringInstall(t *testing.T) {
	room, pub, sub := newRoomDataTrackParticipants(t)
	var once, wrapOnce sync.Once
	var revoked atomic.Bool
	var wrapped *closeDuringDataTrackAdd
	sub.params.ParticipantHelper.(*typesfakes.FakeLocalParticipantHelper).ResolveDataTrackCalls(func(p types.LocalParticipant, id livekit.TrackID) types.DataResolverResult {
		res := room.ResolveDataTrackForSubscriber(p, id)
		if res.DataTrack != nil {
			wrapOnce.Do(func() {
				wrapped = &closeDuringDataTrackAdd{DataTrack: res.DataTrack, afterAdd: func() {
					once.Do(func() {
						assert.NoError(t, pub.HandleUpdateSubscriptionPermission(&livekit.SubscriptionPermission{}))
						revoked.Store(true)
					})
				}}
			})
			res.DataTrack = wrapped
		}
		return res
	})
	pub.HandlePublishDataTrackRequest(&livekit.PublishDataTrackRequest{PubHandle: 1, Name: "test"})
	track := pub.GetPublishedDataTrack(1)
	require.NotNil(t, track)

	require.Eventually(t, revoked.Load, time.Second, time.Millisecond, "revoke must run during the real add")
	require.NoError(t, pub.HandleUpdateSubscriptionPermission(&livekit.SubscriptionPermission{AllParticipants: true}))
	require.Eventually(t, func() bool {
		return track.IsSubscriber(sub.ID()) && len(sub.GetSubscribedDataTracks()) == 1 && sub.IsSubscribedTo(pub.ID())
	}, time.Second, time.Millisecond, "regrant must restore the auto-subscription")
}

func TestDataTrackCloseBeforePublisherMark(t *testing.T) {
	room, pub, sub := newRoomDataTrackParticipants(t)
	var once sync.Once
	sub.params.ParticipantHelper.(*typesfakes.FakeLocalParticipantHelper).ResolveDataTrackCalls(func(p types.LocalParticipant, id livekit.TrackID) types.DataResolverResult {
		res := room.ResolveDataTrackForSubscriber(p, id)
		if res.DataTrack != nil && res.HasPermission && res.DataTrack.IsSubscriber(sub.ID()) && !sub.IsSubscribedTo(pub.ID()) {
			// Keep the permitted snapshot, but close the real downtrack before the
			// subscription manager consumes it and marks the publisher.
			once.Do(func() {
				assert.NoError(t, pub.HandleUpdateSubscriptionPermission(&livekit.SubscriptionPermission{}))
			})
		}
		return res
	})
	pub.HandlePublishDataTrackRequest(&livekit.PublishDataTrackRequest{PubHandle: 1, Name: "test"})
	track := pub.GetPublishedDataTrack(1)
	require.NotNil(t, track)
	sink := sub.params.Sink.(*routingfakes.FakeMessageSink)
	deniedUpdateSent := func() bool {
		for i := 0; i < sink.WriteMessageCallCount(); i++ {
			update := sink.WriteMessageArgsForCall(i).(*livekit.SignalResponse).GetSubscriptionPermissionUpdate()
			if update != nil && update.TrackSid == string(track.ID()) && !update.Allowed {
				return true
			}
		}
		return false
	}
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.True(c, deniedUpdateSent(), "closed subscription must retry and report denied permission")
		assert.False(c, track.IsSubscriber(sub.ID()))
		assert.Empty(c, sub.GetSubscribedDataTracks())
		assert.False(c, sub.IsSubscribedTo(pub.ID()), "closed subscription must not mark the publisher")
		assert.NotContains(c, sub.GetSubscribedParticipants(), pub.ID(), "closed subscription must not list the publisher")
	}, time.Second, time.Millisecond)
	require.NoError(t, pub.HandleUpdateSubscriptionPermission(&livekit.SubscriptionPermission{AllParticipants: true}))
	require.Eventually(t, func() bool {
		return track.IsSubscriber(sub.ID()) && len(sub.GetSubscribedDataTracks()) == 1 && sub.IsSubscribedTo(pub.ID())
	}, time.Second, time.Millisecond, "regrant must restore the auto-subscription")
}

func TestRoomSubscriptionPermissionEndToEnd(t *testing.T) {
	room := newRoomWithParticipants(t, testRoomOpts{})
	pub := newParticipantForTest("pub")
	sub1 := newParticipantForTest("sub1")
	sub2 := newParticipantForTest("sub2")
	for _, p := range []*ParticipantImpl{pub, sub1, sub2} {
		p.setListener(room.LocalParticipantListener())
		p.params.EnableDataTracks = true
		helper := p.params.ParticipantHelper.(*typesfakes.FakeLocalParticipantHelper)
		helper.ResolveDataTrackCalls(room.ResolveDataTrackForSubscriber)
		helper.ResolveMediaTrackCalls(room.ResolveMediaTrackForSubscriber)
		require.NoError(t, room.Join(p, nil, &ParticipantOptions{AutoSubscribeDataTrack: true}, iceServersForRoom))
		p.updateState(livekit.ParticipantInfo_ACTIVE)
		t.Cleanup(func() { p.Close(false, types.ParticipantCloseReasonNone, false) })
	}

	// Publish a real DataTrack (auto-subscribed by sub1 and sub2) and a MediaTrack
	// registered in UpTrackManager + RoomTrackManager for end-to-end permission resolution.
	pub.HandlePublishDataTrackRequest(&livekit.PublishDataTrackRequest{PubHandle: 1, Name: "dt"})
	dt := pub.GetPublishedDataTrack(1)
	require.NotNil(t, dt)

	mt := NewMediaTrack(MediaTrackParams{
		ParticipantID:       pub.ID,
		ParticipantIdentity: pub.Identity(),
		TelemetryListener:   pub.GetTelemetryListener,
		Logger:              logger.GetLogger(),
	}, &livekit.TrackInfo{
		Sid:  "TR_e2e",
		Type: livekit.TrackType_VIDEO,
	})
	fakeReceiver := &sfufakes.FakeTrackReceiver{}
	fakeReceiver.MimeReturns(mime.MimeTypeVP8)
	fakeReceiver.CodecReturns(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: mime.MimeTypeVP8.String(), ClockRate: 90000},
		PayloadType:        96,
	})
	mt.MediaTrackReceiver.SetupReceiver(fakeReceiver, 0, "0")
	pub.UpTrackManager.AddPublishedTrack(mt)
	room.trackManager.AddTrack(mt, pub.Identity(), pub.ID())
	subTrack1, err := mt.AddSubscriber(sub1)
	require.NoError(t, err)
	_, err = mt.AddSubscriber(sub2)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return dt.IsSubscriber(sub1.ID()) && dt.IsSubscriber(sub2.ID()) &&
			mt.IsSubscriber(sub1.ID()) && mt.IsSubscriber(sub2.ID()) &&
			len(sub1.GetSubscribedDataTracks()) == 1 && len(sub2.GetSubscribedDataTracks()) == 1
	}, 2*time.Second, time.Millisecond)

	sh := signalling.NewSignalHandler(signalling.SignalHandlerParams{
		Logger:      logger.GetLogger(),
		Participant: pub,
	})

	allowSub1 := &livekit.SubscriptionPermission{
		TrackPermissions: []*livekit.TrackPermission{
			{ParticipantSid: string(sub1.ID()), AllTracks: true},
		},
	}
	require.NoError(t, sh.HandleMessage(&livekit.SignalRequest{
		Message: &livekit.SignalRequest_SubscriptionPermission{
			SubscriptionPermission: allowSub1,
		},
	}))

	require.Eventually(t, func() bool {
		return dt.IsSubscriber(sub1.ID()) && !dt.IsSubscriber(sub2.ID()) &&
			mt.IsSubscriber(sub1.ID()) && !mt.IsSubscriber(sub2.ID()) &&
			len(sub1.GetSubscribedDataTracks()) == 1 && len(sub2.GetSubscribedDataTracks()) == 0
	}, 2*time.Second, time.Millisecond)
	require.True(t, room.ResolveDataTrackForSubscriber(sub1, dt.ID()).HasPermission)
	require.False(t, room.ResolveDataTrackForSubscriber(sub2, dt.ID()).HasPermission)
	require.True(t, room.ResolveMediaTrackForSubscriber(sub1, mt.ID()).HasPermission)
	require.False(t, room.ResolveMediaTrackForSubscriber(sub2, mt.ID()).HasPermission)

	t.Run("MalformedSignalPreservesState", func(t *testing.T) {
		permBeforeInvalid, verBeforeInvalid := pub.SubscriptionPermission()
		require.Equal(t, allowSub1, permBeforeInvalid)

		// A malformed SignalRequest_SubscriptionPermission returns an error via SignalHandler
		// (logged, no crash) and must leave stored version, proto, and enforcement intact.
		invalidPerm := &livekit.SubscriptionPermission{
			TrackPermissions: []*livekit.TrackPermission{
				{ParticipantIdentity: string(sub2.Identity()), AllTracks: true},
				{},
			},
		}
		require.NoError(t, sh.HandleMessage(&livekit.SignalRequest{
			Message: &livekit.SignalRequest_SubscriptionPermission{
				SubscriptionPermission: invalidPerm,
			},
		}))

		permAfterInvalid, verAfterInvalid := pub.SubscriptionPermission()
		assert.Equal(t, verBeforeInvalid.Load(), verAfterInvalid.Load())
		assert.Equal(t, allowSub1, permAfterInvalid)
		// Wait briefly to confirm no asynchronous media DownTrack close was queued.
		require.Never(t, func() bool {
			return !dt.IsSubscriber(sub1.ID()) ||
				!mt.IsSubscriber(sub1.ID()) ||
				subTrack1.DownTrack().IsClosed()
		}, 50*time.Millisecond, time.Millisecond)
		assert.False(t, dt.IsSubscriber(sub2.ID()))
		assert.False(t, mt.IsSubscriber(sub2.ID()))
		assert.True(t, room.ResolveDataTrackForSubscriber(sub1, dt.ID()).HasPermission)
		assert.False(t, room.ResolveDataTrackForSubscriber(sub2, dt.ID()).HasPermission)
		assert.True(t, room.ResolveMediaTrackForSubscriber(sub1, mt.ID()).HasPermission)
		assert.False(t, room.ResolveMediaTrackForSubscriber(sub2, mt.ID()).HasPermission)
	})

	t.Run("ConcurrentOlderRemoteDoesNotOverwrite", func(t *testing.T) {
		// Simulate an older remote permission update (timedVersion != 0) pausing in SID
		// resolution while a newer signal update (allowSub2) arrives and commits.
		_, baseVer := pub.SubscriptionPermission()
		allowSub2 := &livekit.SubscriptionPermission{
			TrackPermissions: []*livekit.TrackPermission{
				{ParticipantSid: string(sub2.ID()), AllTracks: true},
			},
		}
		vOlderRemote := utils.TimedVersionFromTime(baseVer.Time().Add(5 * time.Millisecond))
		require.True(t, vOlderRemote.After(baseVer))
		enteredResolver := make(chan struct{})
		releaseResolver := make(chan struct{})
		release := sync.OnceFunc(func() {
			close(releaseResolver)
		})
		t.Cleanup(release)
		var barrierOnce sync.Once

		remoteErrCh := make(chan error, 1)
		go func() {
			remoteErrCh <- pub.UpdateSubscriptionPermission(allowSub1, vOlderRemote, func(pID livekit.ParticipantID) types.LocalParticipant {
				p := room.GetParticipantByID(pID)
				barrierOnce.Do(func() {
					close(enteredResolver)
					<-releaseResolver
				})
				return p
			})
		}()

		select {
		case <-enteredResolver:
		case <-time.After(5 * time.Second):
			require.FailNow(t, "timed out waiting for resolver entry")
		}
		for !time.Now().After(vOlderRemote.Time().Add(5 * time.Millisecond)) {
			time.Sleep(time.Millisecond)
		}

		require.NoError(t, sh.HandleMessage(&livekit.SignalRequest{
			Message: &livekit.SignalRequest_SubscriptionPermission{
				SubscriptionPermission: allowSub2,
			},
		}))
		subTrack2, err := mt.AddSubscriber(sub2)
		require.NoError(t, err)

		require.Eventually(t, func() bool {
			return !dt.IsSubscriber(sub1.ID()) && dt.IsSubscriber(sub2.ID()) &&
				!mt.IsSubscriber(sub1.ID()) && mt.IsSubscriber(sub2.ID()) &&
				len(sub1.GetSubscribedDataTracks()) == 0 && len(sub2.GetSubscribedDataTracks()) == 1
		}, 2*time.Second, time.Millisecond)

		release()
		select {
		case err := <-remoteErrCh:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			require.FailNow(t, "timed out waiting for UpdateSubscriptionPermission completion")
		}

		// Older remote update must not overwrite allowSub2 or revoke sub2's active track.
		// Poll continuously so an asynchronously queued media DownTrack close
		// (go dt.CloseWithFlush) cannot escape detection.
		require.Never(t, func() bool {
			return !dt.IsSubscriber(sub2.ID()) ||
				!mt.IsSubscriber(sub2.ID()) ||
				subTrack2.DownTrack().IsClosed()
		}, 100*time.Millisecond, time.Millisecond)
		permFinal, _ := pub.SubscriptionPermission()
		assert.Equal(t, allowSub2, permFinal)
		assert.False(t, dt.IsSubscriber(sub1.ID()))
		assert.False(t, mt.IsSubscriber(sub1.ID()))
		assert.True(t, room.ResolveDataTrackForSubscriber(sub2, dt.ID()).HasPermission)
		assert.False(t, room.ResolveDataTrackForSubscriber(sub1, dt.ID()).HasPermission)
		assert.True(t, room.ResolveMediaTrackForSubscriber(sub2, mt.ID()).HasPermission)
		assert.False(t, room.ResolveMediaTrackForSubscriber(sub1, mt.ID()).HasPermission)
	})
}

func TestRoomDataTrackRevocationAtomicWithPermissionCommit(t *testing.T) {
	_, pub, sub := newRoomDataTrackParticipants(t)
	pub.HandlePublishDataTrackRequest(&livekit.PublishDataTrackRequest{PubHandle: 1, Name: "test"})
	dt := pub.GetPublishedDataTrack(1)
	require.NotNil(t, dt)

	require.Eventually(t, func() bool {
		return dt.IsSubscriber(sub.ID()) && len(sub.GetSubscribedDataTracks()) == 1
	}, 2*time.Second, time.Millisecond)

	enteredRevoke := make(chan struct{})
	releaseRevoke := make(chan struct{})
	release := sync.OnceFunc(func() {
		close(releaseRevoke)
	})
	t.Cleanup(release)
	var barrierOnce sync.Once

	wrappedDT := &revokeBarrierDataTrack{
		DataTrack: dt,
		beforeRevoke: func() {
			barrierOnce.Do(func() {
				close(enteredRevoke)
				<-releaseRevoke
			})
		},
	}
	pub.UpDataTrackManager.lock.Lock()
	pub.UpDataTrackManager.dataTracks[dt.PubHandle()] = wrappedDT
	pub.SubscriptionPermissionManager.addTrack(wrappedDT)
	pub.UpDataTrackManager.lock.Unlock()
	t.Cleanup(func() {
		pub.UpDataTrackManager.lock.Lock()
		if pub.UpDataTrackManager.dataTracks[dt.PubHandle()] == wrappedDT {
			pub.UpDataTrackManager.dataTracks[dt.PubHandle()] = dt
		}
		pub.SubscriptionPermissionManager.removeTrack(wrappedDT)
		pub.SubscriptionPermissionManager.addTrack(dt)
		pub.UpDataTrackManager.lock.Unlock()
	})

	denyErrCh := make(chan error, 1)
	go func() {
		denyErrCh <- pub.HandleUpdateSubscriptionPermission(&livekit.SubscriptionPermission{})
	}()

	select {
	case <-enteredRevoke:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "timed out waiting for data track revoke entry")
	}

	grantErrCh := make(chan error, 1)
	go func() {
		grantErrCh <- pub.HandleUpdateSubscriptionPermission(&livekit.SubscriptionPermission{AllParticipants: true})
	}()

	// Grant must serialize behind the in-flight revoke rather than committing and letting
	// sub re-subscribe before the stale empty allowlist is applied.
	select {
	case err := <-grantErrCh:
		require.FailNowf(t, "grant finished before in-flight revoke completed", "err=%v", err)
	case <-time.After(50 * time.Millisecond):
	}

	release()
	for _, ch := range []chan error{denyErrCh, grantErrCh} {
		select {
		case err := <-ch:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			require.FailNow(t, "timed out waiting for permission update completion")
		}
	}

	require.Eventually(t, func() bool {
		return dt.IsSubscriber(sub.ID()) && len(sub.GetSubscribedDataTracks()) == 1 && sub.IsSubscribedTo(pub.ID())
	}, 2*time.Second, time.Millisecond, "concurrent grant must preserve/restore data track subscription")
}

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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/routing/routingfakes"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
	"github.com/livekit/protocol/livekit"
)

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

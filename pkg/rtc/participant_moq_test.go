// Copyright 2026 LiveKit, Inc.
// SPDX-License-Identifier: Apache-2.0

package rtc

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/routing/routingfakes"
	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

type testMoQTransport struct {
	closed  atomic.Uint32
	packets atomic.Uint32
}

func (t *testMoQTransport) PathQuality() (float32, livekit.ConnectionQuality) {
	return 4.5, livekit.ConnectionQuality_EXCELLENT
}
func (t *testMoQTransport) Close()                                   { t.closed.Add(1) }
func (t *testMoQTransport) SendDataTrackMessage([]byte, uint8) error { t.packets.Add(1); return nil }

func TestPublishDataTrackWithServerFlagOff(t *testing.T) {
	moq := &MoQParticipantOptions{tracks: NewRoomTrackManager(logger.GetLogger())}
	for _, tc := range []struct {
		name           string
		moq            *MoQParticipantOptions
		canPublishData bool
		wantPublished  bool
	}{
		{name: "MoQ with grant", moq: moq, canPublishData: true, wantPublished: true},
		{name: "MoQ without grant", moq: moq},
		{name: "WebRTC with grant", canPublishData: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newParticipantForTestWithOpts("pub", &participantOpts{
				moq:         tc.moq,
				permissions: &livekit.ParticipantPermission{CanPublishData: tc.canPublishData},
			})
			p.HandlePublishDataTrackRequest(&livekit.PublishDataTrackRequest{PubHandle: 1, Name: "test-track"})
			sink := p.params.Sink.(*routingfakes.FakeMessageSink)
			require.Equal(t, 1, sink.WriteMessageCallCount())
			if tc.wantPublished {
				require.NotNil(t, sink.WriteMessageArgsForCall(0).(*livekit.SignalResponse).GetPublishDataTrackResponse())
				require.NotNil(t, p.GetPublishedDataTrack(1))
			} else {
				require.Equal(t, livekit.RequestResponse_NOT_ALLOWED, lastRequestResponse(t, sink, 0).Reason)
				require.Nil(t, p.GetPublishedDataTrack(1))
			}
		})
	}
}

func TestMoQAttachment(t *testing.T) {
	var m moqAttachment
	t.Cleanup(m.close)
	first, second := &testMoQTransport{}, &testMoQTransport{}
	info := m.issue("moqt://localhost:7882/moq", "PA_test")
	require.NotEmpty(t, info.AttachmentToken)
	require.Error(t, m.attach(info.Generation, "wrong", first))
	var accepted atomic.Uint32
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if m.attach(info.Generation, info.AttachmentToken, first) == nil {
				accepted.Add(1)
			}
		})
	}
	wg.Wait()
	require.EqualValues(t, 1, accepted.Load())
	next := m.issue(info.Endpoint, info.ParticipantSid)
	require.Error(t, m.attach(info.Generation, info.AttachmentToken, second))
	require.NoError(t, m.SendDataTrackMessage([]byte("old"), 128))
	require.NoError(t, m.attach(next.Generation, next.AttachmentToken, second))
	require.EqualValues(t, 1, first.closed.Load())
	m.detach(info.Generation, func() { panic("stale disconnect") })
	require.True(t, (&ParticipantImpl{moq: &m}).IsMoQAttachmentCurrent(next.Generation))
	require.NoError(t, m.SendDataTrackMessage([]byte("new"), 128))
	require.EqualValues(t, 1, first.packets.Load())
	require.EqualValues(t, 1, second.packets.Load())
	m.detach(next.Generation, func() { panic("resume window unexpectedly expired") })
	require.False(t, (&ParticipantImpl{moq: &m}).IsMoQAttachmentCurrent(m.generation))
	require.True(t, m.everConnected())
	require.ErrorIs(t, m.SendDataTrackMessage(nil, 128), ErrDataChannelUnavailable)
	resume := m.issue(info.Endpoint, info.ParticipantSid)
	third := &testMoQTransport{}
	require.NoError(t, m.attach(resume.Generation, resume.AttachmentToken, third))
	m.close()
	m.close()
	require.EqualValues(t, 1, third.closed.Load())
	require.Nil(t, m.issue(info.Endpoint, info.ParticipantSid))
	require.Error(t, m.attach(resume.Generation, resume.AttachmentToken, second))
}

func TestMoQExpiredAttachment(t *testing.T) {
	var m moqAttachment
	t.Cleanup(m.close)
	info := m.issue("moqt://localhost:7882/moq", "PA_test")
	m.expires = time.Now().Add(-time.Second)
	require.Error(t, m.attach(info.Generation, info.AttachmentToken, &testMoQTransport{}))
	require.False(t, (&ParticipantImpl{moq: &m}).IsMoQAttachmentCurrent(m.generation))
}

func TestMoQSubscriptionLifetime(t *testing.T) {
	tracks := NewRoomTrackManager(logger.GetLogger())
	id := livekit.TrackID("TR_video")
	options := (&Room{trackManager: tracks}).MoQParticipantOptions("moqt://node:443/moq")
	p := &ParticipantImpl{moq: &moqAttachment{options: options, subscriptions: make(map[livekit.TrackID]struct{})}}
	track := &typesfakes.FakeMediaTrack{}
	track.IDReturns(id)
	track.IsOpenReturns(true)
	p.SubscribeToTrack(id, false)
	require.False(t, p.WantsMoQTrack(id))
	require.Nil(t, tracks.moqSubscribers)
	tracks.AddTrack(track, "publisher", "PA_pub")
	p.SubscribeToTrack(id, false)
	require.True(t, p.WantsMoQTrack(id))
	require.Equal(t, []livekit.TrackID{id}, p.MoQSubscribedTrackIDs())
	p.UnsubscribeFromTrack(id)
	require.Empty(t, tracks.moqSubscribers)

	var wg sync.WaitGroup
	wg.Go(func() { p.SubscribeToTrack(id, false) })
	wg.Go(func() { tracks.RemoveTrack(track) })
	wg.Wait()
	require.Empty(t, tracks.moqSubscribers)
	require.False(t, p.WantsMoQTrack(id))

	tracks.AddTrack(track, "publisher", "PA_pub")
	require.False(t, p.WantsMoQTrack(id))
	p.SubscribeToTrack(id, false)
	p.moq.close()
	p.SubscribeToTrack(id, false)
	require.False(t, p.WantsMoQTrack(id))
	require.Empty(t, tracks.moqSubscribers)
	webRTC := &ParticipantImpl{}
	require.False(t, webRTC.UsesMoQ())
	require.Error(t, webRTC.AttachMoQ(1, "unavailable", &testMoQTransport{}))
}

func TestSignalingOnlyTransport(t *testing.T) {
	manager, err := NewTransportManager(TransportManagerParams{SignalingOnly: true})
	require.NoError(t, err)
	defer manager.Close()
	require.Nil(t, manager.publisher)
	require.Nil(t, manager.subscriber)
	require.Nil(t, manager.GetDataTrackTransport())
	require.Nil(t, manager.GetSubscriberPacer())
	require.Empty(t, manager.GetICEConnectionInfo())
	require.False(t, manager.HasPublisherEverConnected())
	require.False(t, manager.HasSubscriberEverConnected())
	require.False(t, manager.HasPublisherICEEverConnected())
	require.False(t, manager.HasSubscriberICEEverConnected())
	require.ErrorIs(t, manager.SendDataMessage(livekit.DataPacket_RELIABLE, nil), ErrDataChannelUnavailable)
	_, _, err = manager.GetAnswer()
	require.ErrorIs(t, err, ErrNoPeerConnection)
	manager.SetSubscriberAllowPause(true)
	manager.UpdateLastSeenSignal()
	require.False(t, manager.LastSeenSignalAt().IsZero())
}

func TestMoQResumeWindowSurvivesSignalClose(t *testing.T) {
	manager, err := NewTransportManager(TransportManagerParams{SignalingOnly: true})
	require.NoError(t, err)
	defer manager.Close()
	p := &ParticipantImpl{TransportManager: manager, moq: &moqAttachment{}}
	defer p.moq.close()
	info := p.moq.issue("moqt://node:443/moq", "PA_existing")
	require.NoError(t, p.moq.attach(info.Generation, info.AttachmentToken, &testMoQTransport{}))
	p.DetachMoQ(info.Generation)
	p.HandleSignalSourceClose()
	require.False(t, p.IsClosed())
	next := p.moq.issue(info.Endpoint, info.ParticipantSid)
	require.NotNil(t, next)
	require.NoError(t, p.moq.attach(next.Generation, next.AttachmentToken, &testMoQTransport{}))
}

func TestMoQTapMembership(t *testing.T) {
	pub := livekit.ParticipantID("PA_publisher")
	track := &typesfakes.FakeMediaTrack{}
	track.IDReturns("TR_video")
	p := &ParticipantImpl{SubscriptionManager: &SubscriptionManager{
		subscribedTo: make(map[livekit.ParticipantID]map[subscribedTrackKey]struct{}),
	}}
	var changes []bool
	p.SubscriptionManager.OnSubscribeStatusChanged(func(id livekit.ParticipantID, subscribed bool) {
		require.Equal(t, pub, id)
		changes = append(changes, subscribed)
	})
	p.grants.Store(&auth.ClaimGrants{Video: &auth.VideoGrant{Hidden: true}})
	old := p.BindMoQSubscription(1, pub, track)
	require.Zero(t, track.OnTrackSubscribedCallCount())
	require.True(t, p.IsSubscribedTo(pub))
	p.grants.Store(&auth.ClaimGrants{Video: &auth.VideoGrant{}})
	current := p.BindMoQSubscription(2, pub, track)
	require.Equal(t, 1, track.OnTrackSubscribedCallCount())
	old()
	require.True(t, p.IsSubscribedTo(pub))
	current()
	require.Empty(t, p.GetSubscribedParticipants())
	require.Equal(t, []bool{true, false}, changes)
}

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"

	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
)

// fakeIngressHandlerClient records WHIPRTCConnectionNotify calls. It embeds the
// interface so only the method under test needs to be implemented; any other
// call would panic (and we assert none happen).
type fakeIngressHandlerClient struct {
	rpc.IngressHandlerClient
	notifyCount atomic.Int32
}

func (f *fakeIngressHandlerClient) WHIPRTCConnectionNotify(
	_ context.Context,
	_ string,
	_ *rpc.WHIPRTCConnectionNotifyRequest,
	_ ...psrpc.RequestOption,
) (*emptypb.Empty, error) {
	f.notifyCount.Inc()
	return &emptypb.Empty{}, nil
}

// TestWhipNotifySessionStopsWhenParticipantLeaves verifies the notifier loop
// terminates once the WHIP participant leaves the room (i.e. IsClosed becomes
// true), and stops issuing further connection notifications.
func TestWhipNotifySessionStopsWhenParticipantLeaves(t *testing.T) {
	origInterval := whipSessionNotifyInterval
	whipSessionNotifyInterval = 5 * time.Millisecond
	t.Cleanup(func() { whipSessionNotifyInterval = origInterval })

	var closed atomic.Bool
	participant := &typesfakes.FakeParticipant{}
	participant.IsClosedStub = func() bool { return closed.Load() }
	participant.IDReturns(livekit.ParticipantID("PA_test"))
	participant.ToProtoReturns(&livekit.ParticipantInfo{})

	cli := &fakeIngressHandlerClient{}
	s := whipService{ingressRpcCli: cli}

	done := make(chan error, 1)
	go func() {
		done <- s.notifySession(context.Background(), participant)
	}()

	// while the participant is connected the loop should keep notifying
	require.Eventually(t, func() bool {
		return cli.notifyCount.Load() > 0
	}, time.Second, time.Millisecond, "expected notifications while participant is connected")

	// the participant leaves the room
	closed.Store(true)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("notifySession did not stop after the participant left the room")
	}

	// no further notifications should be attempted after it stops
	countAtStop := cli.notifyCount.Load()
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, countAtStop, cli.notifyCount.Load(), "should not notify after the participant left")
}

// TestWhipNotifySessionStopsOnContextCancel verifies the loop exits when the
// aliveCtx (cancelled from the participant's OnClose callback) is done.
func TestWhipNotifySessionStopsOnContextCancel(t *testing.T) {
	origInterval := whipSessionNotifyInterval
	whipSessionNotifyInterval = 5 * time.Millisecond
	t.Cleanup(func() { whipSessionNotifyInterval = origInterval })

	participant := &typesfakes.FakeParticipant{}
	participant.IsClosedReturns(false)
	participant.IDReturns(livekit.ParticipantID("PA_test"))
	participant.ToProtoReturns(&livekit.ParticipantInfo{})

	cli := &fakeIngressHandlerClient{}
	s := whipService{ingressRpcCli: cli}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- s.notifySession(ctx, participant)
	}()

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("notifySession did not stop after context was cancelled")
	}
}

// TestWhipSendConnectionNotifySkipsClosedParticipant verifies the guard that
// short-circuits the RPC (and drives loop termination) for a closed participant.
func TestWhipSendConnectionNotifySkipsClosedParticipant(t *testing.T) {
	participant := &typesfakes.FakeParticipant{}
	participant.IsClosedReturns(true)
	participant.IDReturns(livekit.ParticipantID("PA_test"))
	participant.ToProtoReturns(&livekit.ParticipantInfo{})

	cli := &fakeIngressHandlerClient{}
	s := whipService{ingressRpcCli: cli}

	err := s.sendConnectionNotify(context.Background(), participant)
	require.ErrorIs(t, err, ErrParticipantNotFound)
	require.Zero(t, cli.notifyCount.Load(), "should not issue an RPC for a closed participant")
}

func whipSubscribeWaitTracks(publisher string, names ...string) map[string]*rpc.WHIPCreateRequest_TrackList {
	return map[string]*rpc.WHIPCreateRequest_TrackList{publisher: {TrackNames: names}}
}

func shortenWhipSubscribeWait(t *testing.T) {
	orig := whipSubscribeWaitInterval
	whipSubscribeWaitInterval = 5 * time.Millisecond
	t.Cleanup(func() { whipSubscribeWaitInterval = orig })
}

// TestWhipWaitForSubscribedTracksReturnsOnceSubscribed verifies the wait ends when every listed
// track is subscribed.
func TestWhipWaitForSubscribedTracksReturnsOnceSubscribed(t *testing.T) {
	shortenWhipSubscribeWait(t)

	var subscribed atomic.Bool
	participant := &typesfakes.FakeLocalParticipant{}
	participant.IsTrackNameSubscribedStub = func(livekit.ParticipantIdentity, string) bool { return subscribed.Load() }

	s := whipService{}
	done := make(chan error, 1)
	go func() {
		done <- s.waitForSubscribedTracks(context.Background(), participant, whipSubscribeWaitTracks("pub", "mic", "cam"))
	}()

	select {
	case <-done:
		t.Fatal("returned before the tracks were subscribed")
	case <-time.After(50 * time.Millisecond):
	}

	subscribed.Store(true)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("did not return after the tracks were subscribed")
	}
}

// TestWhipWaitForSubscribedTracksStopsOnContextCancel verifies a track that never appears does not
// keep the wait, and its polling goroutines, alive once the request is done.
func TestWhipWaitForSubscribedTracksStopsOnContextCancel(t *testing.T) {
	shortenWhipSubscribeWait(t)

	participant := &typesfakes.FakeLocalParticipant{}
	participant.IsTrackNameSubscribedReturns(false)

	s := whipService{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- s.waitForSubscribedTracks(ctx, participant, whipSubscribeWaitTracks("pub", "mic"))
	}()

	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("did not stop after the context was cancelled")
	}

	// the polling has stopped as well
	calls := participant.IsTrackNameSubscribedCallCount()
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, calls, participant.IsTrackNameSubscribedCallCount())
}

// TestWhipWaitForSubscribedTracksStopsWhenParticipantLeaves verifies the wait ends when the
// participant closes before the tracks appear.
func TestWhipWaitForSubscribedTracksStopsWhenParticipantLeaves(t *testing.T) {
	shortenWhipSubscribeWait(t)

	var closed atomic.Bool
	participant := &typesfakes.FakeLocalParticipant{}
	participant.IsTrackNameSubscribedReturns(false)
	participant.IsClosedStub = func() bool { return closed.Load() }

	s := whipService{}
	done := make(chan error, 1)
	go func() {
		done <- s.waitForSubscribedTracks(context.Background(), participant, whipSubscribeWaitTracks("pub", "mic"))
	}()

	closed.Store(true)
	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrParticipantNotFound)
	case <-time.After(time.Second):
		t.Fatal("did not stop after the participant left")
	}
}

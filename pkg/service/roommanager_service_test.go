package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing/routingfakes"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
	"github.com/livekit/livekit-server/pkg/sfu"
)

type lockFailingObjectStore struct {
	ObjectStore
	lockErr         error
	lockCalls       atomic.Int32
	unlockCalls     atomic.Int32
	deleteRoomCalls atomic.Int32
}

func (s *lockFailingObjectStore) LockRoom(context.Context, livekit.RoomName, time.Duration) (string, error) {
	s.lockCalls.Inc()
	return "", s.lockErr
}

func (s *lockFailingObjectStore) UnlockRoom(context.Context, livekit.RoomName, string) error {
	s.unlockCalls.Inc()
	return nil
}

func (s *lockFailingObjectStore) DeleteRoom(context.Context, livekit.RoomName) error {
	s.deleteRoomCalls.Inc()
	return nil
}

type recordingObjectStore struct {
	ObjectStore
	lockCalls       atomic.Int32
	unlockCalls     atomic.Int32
	deleteRoomCalls atomic.Int32
}

func (s *recordingObjectStore) LockRoom(context.Context, livekit.RoomName, time.Duration) (string, error) {
	s.lockCalls.Inc()
	return "lock", nil
}

func (s *recordingObjectStore) UnlockRoom(context.Context, livekit.RoomName, string) error {
	s.unlockCalls.Inc()
	return nil
}

func (s *recordingObjectStore) DeleteRoom(context.Context, livekit.RoomName) error {
	s.deleteRoomCalls.Inc()
	return nil
}

func newTestRoomForService(t *testing.T, roomName livekit.RoomName, sid string) *rtc.Room {
	t.Helper()
	room := rtc.NewRoom(
		&livekit.Room{Name: string(roomName), Sid: sid},
		nil,
		rtc.WebRTCConfig{},
		config.RoomConfig{},
		&sfu.AudioConfig{},
		&livekit.ServerInfo{},
		nil,
		nil,
		nil,
		nil,
	)
	t.Cleanup(func() { room.Close(types.RoomCloseReasonUnknown) })
	return room
}

// TestDeleteRoomClearsStateBeforeParticipantsClose reproduces the #4726
// window: a same-name CreateRoom can arrive while DeleteRoom is still closing
// participants. The old room's persistent state must already be gone by then,
// and its delayed teardown must not delete the room that replaced it.
func TestDeleteRoomClearsStateBeforeParticipantsClose(t *testing.T) {
	ctx := t.Context()
	roomName := livekit.RoomName("test-room")

	oldRoom := newTestRoomForService(t, roomName, "RM_old")
	participant := rtc.NewMockParticipant("p1", types.CurrentProtocol, false, true, oldRoom.LocalParticipantListener())
	closeStarted := make(chan struct{})
	releaseClose := make(chan struct{})
	participant.CloseCalls(func(bool, types.ParticipantCloseReason, bool) error {
		close(closeStarted)
		<-releaseClose
		return nil
	})
	require.NoError(t, oldRoom.Join(participant, nil, &rtc.ParticipantOptions{AutoSubscribe: true}, nil))

	store := &recordingObjectStore{}
	manager := &RoomManager{
		router:    &routingfakes.FakeRouter{},
		roomStore: store,
		rooms:     map[livekit.RoomName]*rtc.Room{roomName: oldRoom},
	}
	oldRoom.OnClose(func(types.RoomCloseReason) {
		manager.deleteRoomIfCurrent(ctx, roomName, oldRoom)
	})

	deleteDone := make(chan error, 1)
	go func() {
		_, err := manager.DeleteRoom(ctx, &livekit.DeleteRoomRequest{Room: string(roomName)})
		deleteDone <- err
	}()

	select {
	case <-closeStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("DeleteRoom never started closing participants")
	}

	// The in-memory room and the store must be clear while the old room's
	// participant close is still blocked, so a recreate in this window gets a
	// fresh SID instead of loading the old record.
	require.Nil(t, manager.GetRoom(ctx, roomName))
	require.Equal(t, int32(1), store.deleteRoomCalls.Load())

	recreated := newTestRoomForService(t, roomName, "RM_new")
	manager.lock.Lock()
	manager.rooms[roomName] = recreated
	manager.lock.Unlock()

	close(releaseClose)
	require.NoError(t, <-deleteDone)

	// The old room's delayed OnClose teardown must leave the replacement alone.
	require.Same(t, recreated, manager.GetRoom(ctx, roomName))
}

func TestDeleteRoomIfCurrentLockFailure(t *testing.T) {
	roomName := livekit.RoomName("test-room")
	room := rtc.NewRoom(
		&livekit.Room{Name: string(roomName), Sid: "RM_test"},
		nil,
		rtc.WebRTCConfig{},
		config.RoomConfig{},
		&sfu.AudioConfig{},
		nil,
		nil,
		nil,
		nil,
		nil,
	)
	store := &lockFailingObjectStore{lockErr: errors.New("lock unavailable")}
	router := &routingfakes.FakeRouter{}
	manager := &RoomManager{
		router:    router,
		roomStore: store,
		rooms:     map[livekit.RoomName]*rtc.Room{roomName: room},
	}

	require.False(t, manager.deleteRoomIfCurrent(t.Context(), roomName, room))
	require.Same(t, room, manager.rooms[roomName])
	require.Equal(t, int32(1), store.lockCalls.Load())
	require.Zero(t, store.unlockCalls.Load())
	require.Zero(t, store.deleteRoomCalls.Load())
	require.Zero(t, router.ClearRoomStateCallCount())
}

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

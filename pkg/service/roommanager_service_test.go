package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
)

func TestRoomManagerICEServersForParticipant_TURNURLOptions(t *testing.T) {
	for _, tc := range []struct {
		name             string
		advertiseTLSPort bool
		udpUseDomain     bool
		tlsOnly          bool
		iceServerCount   int
		urls             []string
	}{
		{
			name:           "defaults",
			iceServerCount: 1,
			urls: []string{
				"turn:203.0.113.1:3478?transport=udp",
				"turns:turn.example.com:443?transport=tcp",
			},
		},
		{
			name:             "advertise tls port",
			advertiseTLSPort: true,
			iceServerCount:   1,
			urls: []string{
				"turn:203.0.113.1:3478?transport=udp",
				"turns:turn.example.com:8443?transport=tcp",
			},
		},
		{
			name:           "use domain for udp",
			udpUseDomain:   true,
			iceServerCount: 1,
			urls: []string{
				"turn:turn.example.com:3478?transport=udp",
				"turns:turn.example.com:443?transport=tcp",
			},
		},
		{
			name:             "both options",
			advertiseTLSPort: true,
			udpUseDomain:     true,
			iceServerCount:   1,
			urls: []string{
				"turn:turn.example.com:3478?transport=udp",
				"turns:turn.example.com:8443?transport=tcp",
			},
		},
		{
			name:           "tls only omits udp",
			tlsOnly:        true,
			iceServerCount: 2,
			urls:           []string{"turns:turn.example.com:443?transport=tcp"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conf := &config.Config{}
			conf.RTC.NodeIP.UnmarshalString("203.0.113.1")
			conf.TURN = config.TURNConfig{
				Enabled:          true,
				Domain:           "turn.example.com",
				TLSPort:          8443,
				UDPPort:          3478,
				AdvertiseTLSPort: tc.advertiseTLSPort,
				UDPUseDomain:     tc.udpUseDomain,
			}
			participant := &typesfakes.FakeLocalParticipant{}
			participant.IDReturns(livekit.ParticipantID("PA_test"))
			manager := &RoomManager{
				config: conf,
				turnAuthHandler: NewTURNAuthHandler(auth.NewSimpleKeyProvider(
					turnTestAPIKey,
					turnTestAPISecret,
				)),
			}

			iceServers := manager.iceServersForParticipant(turnTestAPIKey, participant, tc.tlsOnly)
			require.Len(t, iceServers, tc.iceServerCount)
			require.Equal(t, tc.urls, iceServers[0].Urls)
			if tc.tlsOnly {
				for _, iceServer := range iceServers {
					for _, url := range iceServer.Urls {
						require.NotContains(t, url, "?transport=udp")
					}
				}
			}
		})
	}
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

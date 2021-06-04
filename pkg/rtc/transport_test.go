package rtc

import (
	"testing"
	"time"

	livekit "github.com/livekit/livekit-server/proto"
	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/require"
)

func TestMissingAnswerDuringICERestart(t *testing.T) {
	params := TransportParams{
		Target: livekit.SignalTarget_PUBLISHER,
		Config: &WebRTCConfig{},
		Stats:  nil,
	}
	transportA, err := NewPCTransport(params)
	require.NoError(t, err)
	_, err = transportA.pc.CreateDataChannel("test", nil)
	require.NoError(t, err)
	transportB, err := NewPCTransport(params)
	require.NoError(t, err)

	// exchange ICE
	transportA.pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		t.Logf("got ICE candidate from A: %v", candidate)
		require.NoError(t, transportB.AddICECandidate(candidate.ToJSON()))
	})
	transportB.pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		t.Logf("got ICE candidate from B: %v", candidate)
		require.NoError(t, transportA.AddICECandidate(candidate.ToJSON()))
	})
	// set offer/answer
	handleOffer := handleOfferFunc(t, transportA, transportB)
	transportA.OnOffer(handleOffer)

	// first establish connection
	require.NoError(t, transportA.CreateAndSendOffer(nil))

	// ensure we are connected the first time
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, webrtc.ICEConnectionStateConnected, transportA.pc.ICEConnectionState())
	require.Equal(t, webrtc.ICEConnectionStateConnected, transportB.pc.ICEConnectionState())

	// offer again, but missed
	transportA.OnOffer(func(sd webrtc.SessionDescription) {})
	require.NoError(t, transportA.CreateAndSendOffer(nil))
	require.Equal(t, webrtc.SignalingStateHaveLocalOffer, transportA.pc.SignalingState())
	require.Equal(t, negotiationStateClient, transportA.negotiationState.Load().(int))

	// now restart ICE
	t.Logf("creating offer with ICE restart")
	transportA.OnOffer(handleOffer)
	require.NoError(t, transportA.CreateAndSendOffer(&webrtc.OfferOptions{
		ICERestart: true,
	}))

	time.Sleep(100 * time.Millisecond)
	require.Equal(t, webrtc.ICEConnectionStateConnected, transportA.pc.ICEConnectionState())
	require.Equal(t, webrtc.ICEConnectionStateConnected, transportB.pc.ICEConnectionState())
}

func handleOfferFunc(t *testing.T, current, other *PCTransport) func(sd webrtc.SessionDescription) {
	return func(sd webrtc.SessionDescription) {
		t.Logf("handling offer")
		t.Logf("setting other remote description")
		require.NoError(t, other.SetRemoteDescription(sd))
		answer, err := other.pc.CreateAnswer(nil)
		require.NoError(t, err)
		require.NoError(t, other.pc.SetLocalDescription(answer))

		t.Logf("setting answer on current")
		require.NoError(t, current.SetRemoteDescription(answer))
	}
}

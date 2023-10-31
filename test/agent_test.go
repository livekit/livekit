package test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/auth"
)

func TestAgents(t *testing.T) {
	_, finish := setupSingleNodeTest("TestAgents")
	defer finish()

	ac1, err := newAgentClient(agentToken())
	require.NoError(t, err)
	ac2, err := newAgentClient(agentToken())
	require.NoError(t, err)
	defer ac1.close()
	defer ac2.close()
	ac1.Run()
	ac2.Run()

	time.Sleep(time.Second * 3)

	require.Equal(t, true, ac1.registered)
	require.Equal(t, true, ac2.registered)

	c1 := createRTCClient("c1", defaultServerPort, nil)
	c2 := createRTCClient("c2", defaultServerPort, nil)
	waitUntilConnected(t, c1, c2)

	// publish 2 tracks
	t1, err := c1.AddStaticTrack("audio/opus", "audio", "webcam")
	require.NoError(t, err)
	defer t1.Stop()
	t2, err := c1.AddStaticTrack("video/vp8", "video", "webcam")
	require.NoError(t, err)
	defer t2.Stop()

	time.Sleep(time.Second * 3)

	require.Equal(t, 1, ac1.roomJobs+ac2.roomJobs)
	require.Equal(t, 1, ac1.participantJobs+ac2.participantJobs)

	// publish 2 tracks
	t3, err := c2.AddStaticTrack("audio/opus", "audio", "webcam")
	require.NoError(t, err)
	defer t3.Stop()
	t4, err := c2.AddStaticTrack("video/vp8", "video", "webcam")
	require.NoError(t, err)
	defer t4.Stop()

	time.Sleep(time.Second * 3)

	require.Equal(t, 1, ac1.roomJobs+ac2.roomJobs)
	require.Equal(t, 2, ac1.participantJobs+ac2.participantJobs)
}

func agentToken() string {
	at := auth.NewAccessToken(testApiKey, testApiSecret).
		AddGrant(&auth.VideoGrant{Agent: true})
	t, err := at.ToJWT()
	if err != nil {
		panic(err)
	}
	return t
}

package test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClientCouldConnect(t *testing.T) {
	c1 := createRTCClient("c1")
	c2 := createRTCClient("c2")
	waitUntilConnected(t, c1, c2)

	// ensure they both see each other
	withTimeout(t, "c1 and c2 could connect", func() bool {
		if len(c1.RemoteParticipants()) == 0 || len(c2.RemoteParticipants()) == 0 {
			return false
		}
		//assert.Equal()
		return true
	})
}

func TestSinglePublisher(t *testing.T) {
	c1 := createRTCClient("c1")
	c2 := createRTCClient("c2")
	waitUntilConnected(t, c1, c2)

	// publish a track and ensure clients receive it ok
	t1, err := c1.AddStaticTrack("audio/opus", "audio", "webcam")
	assert.NoError(t, err)
	defer t1.Stop()
	t2, err := c1.AddStaticTrack("video/vp8", "video", "webcam")
	assert.NoError(t, err)
	defer t2.Stop()

	// a new client joins and should get the initial stream
	c3 := createRTCClient("c3")

	withTimeout(t, "c2 should receive two tracks", func() bool {
		if len(c2.SubscribedTracks()) == 0 {
			return false
		}
		// should have received two tracks
		if len(c2.SubscribedTracks()[c1.ID()]) != 2 {
			return false
		}

		tr1 := c2.SubscribedTracks()[c1.ID()][0]
		assert.Equal(t, "webcam", tr1.StreamID())
		return true
	})

	// ensure that new client that has joined also received tracks
	waitUntilConnected(t, c3)
	withTimeout(t, "c2 should receive two tracks", func() bool {
		if len(c3.SubscribedTracks()) == 0 {
			return false
		}
		// should have received two tracks
		if len(c3.SubscribedTracks()[c1.ID()]) != 2 {
			return false
		}
		return true
	})
}

func TestMain(m *testing.M) {
	s := setupSingleNodeTest(testRoom)

	code := m.Run()

	teardownTest(s, testRoom)
	os.Exit(code)
}

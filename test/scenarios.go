package test

import (
	"testing"
	"time"

	"github.com/livekit/protocol/logger"
	livekit "github.com/livekit/protocol/proto"
	"github.com/livekit/protocol/utils"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/testutils"
	testclient "github.com/livekit/livekit-server/test/client"
)

// a scenario with lots of clients connecting, publishing, and leaving at random periods
func scenarioPublishingUponJoining(t *testing.T, ports ...int) {
	c1 := createRTCClient("puj_1", defaultServerPort, nil)
	c2 := createRTCClient("puj_2", secondServerPort, &testclient.Options{AutoSubscribe: true})
	c3 := createRTCClient("puj_3", defaultServerPort, &testclient.Options{AutoSubscribe: true})
	defer stopClients(c1, c2, c3)

	waitUntilConnected(t, c1, c2, c3)

	// c1 and c2 publishing, c3 just receiving
	writers := publishTracksForClients(t, c1, c2)
	defer stopWriters(writers...)

	logger.Infow("waiting to receive tracks from c1 and c2")
	success := testutils.WithTimeout(t, "c3 should receive tracks from both clients", func() bool {
		tracks := c3.SubscribedTracks()
		if len(tracks[c1.ID()]) != 2 {
			return false
		}
		if len(tracks[c2.ID()]) != 2 {
			return false
		}
		return true
	})

	if !success {
		t.FailNow()
	}

	// after a delay, c2 reconnects, then publishing
	time.Sleep(syncDelay)
	c2.Stop()

	logger.Infow("waiting for c2 tracks to be gone")
	success = testutils.WithTimeout(t, "c2 tracks should be gone", func() bool {
		tracks := c3.SubscribedTracks()
		if len(tracks[c1.ID()]) != 2 {
			return false
		}
		if len(tracks[c2.ID()]) != 0 {
			return false
		}
		if len(c1.SubscribedTracks()[c2.ID()]) != 0 {
			return false
		}
		return true
	})
	if !success {
		t.FailNow()
	}

	logger.Infow("c2 reconnecting")
	// connect to a diff port
	c2 = createRTCClient("puj_2", defaultServerPort, nil)
	defer c2.Stop()
	waitUntilConnected(t, c2)
	writers = publishTracksForClients(t, c2)
	defer stopWriters(writers...)

	testutils.WithTimeout(t, "new c2 tracks should be published again", func() bool {
		tracks := c3.SubscribedTracks()
		if len(tracks[c2.ID()]) != 2 {
			return false
		}
		if len(c1.SubscribedTracks()[c2.ID()]) != 2 {
			return false
		}
		return true
	})
}

func scenarioReceiveBeforePublish(t *testing.T) {
	c1 := createRTCClient("rbp_1", defaultServerPort, nil)
	c2 := createRTCClient("rbp_2", defaultServerPort, nil)

	waitUntilConnected(t, c1, c2)
	defer stopClients(c1, c2)

	// c1 publishes
	writers := publishTracksForClients(t, c1)
	defer stopWriters(writers...)

	// c2 should see some bytes flowing through
	success := testutils.WithTimeout(t, "waiting to receive bytes on c2", func() bool {
		return c2.BytesReceived() > 20
	})
	if !success {
		t.FailNow()
	}

	// now publish on C2
	writers = publishTracksForClients(t, c2)
	defer stopWriters(writers...)

	success = testutils.WithTimeout(t, "waiting to receive c2 tracks on c1", func() bool {
		return len(c1.SubscribedTracks()[c2.ID()]) == 2
	})
	require.True(t, success)

	// now leave, and ensure that it's immediate
	c2.Stop()

	time.Sleep(testutils.ConnectTimeout)
	require.Empty(t, c1.RemoteParticipants())
}

func scenarioDataPublish(t *testing.T) {
	c1 := createRTCClient("dp1", defaultServerPort, nil)
	c2 := createRTCClient("dp2", secondServerPort, nil)
	waitUntilConnected(t, c1, c2)
	defer stopClients(c1, c2)

	payload := "test bytes"

	received := utils.AtomicFlag{}
	c2.OnDataReceived = func(data []byte, sid string) {
		if string(data) == payload && sid == c1.ID() {
			received.TrySet(true)
		}
	}

	require.NoError(t, c1.PublishData([]byte(payload), livekit.DataPacket_RELIABLE))

	testutils.WithTimeout(t, "waiting for c2 to receive data", func() bool {
		return received.Get()
	})
}

// websocket reconnects
func scenarioWSReconnect(t *testing.T) {
	c1 := createRTCClient("wsr_1", defaultServerPort, nil)
	c2 := createRTCClient("wsr_2", defaultServerPort, nil)

	waitUntilConnected(t, c1, c2)

	// c1 publishes track, but disconnects websockets and reconnects
}

func publishTracksForClients(t *testing.T, clients ...*testclient.RTCClient) []*testclient.TrackWriter {
	logger.Infow("publishing tracks for clients")
	var writers []*testclient.TrackWriter
	for i := range clients {
		c := clients[i]
		tw, err := c.AddStaticTrack("audio/opus", "audio", "webcam")
		require.NoError(t, err)

		writers = append(writers, tw)
		tw, err = c.AddStaticTrack("video/vp8", "video", "webcam")
		require.NoError(t, err)
		writers = append(writers, tw)
	}
	return writers
}

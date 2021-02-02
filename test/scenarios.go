package test

import (
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/livekit/livekit-server/cmd/cli/client"
	"github.com/livekit/livekit-server/pkg/logger"
)

// a scenario with lots of clients connecting, publishing, and leaving at random periods
func scenarioPublishingUponJoining(t *testing.T, ports ...int) {
	c1 := createRTCClient("puj_1", ports[rand.Intn(len(ports))])
	c2 := createRTCClient("puj_2", ports[rand.Intn(len(ports))])
	c3 := createRTCClient("puj_3", ports[rand.Intn(len(ports))])
	defer stopClients(c1, c2, c3)

	waitUntilConnected(t, c1, c2, c3)

	// c1 and c2 publishing, c3 just receiving
	writers := publishTracksForClients(t, c1, c2)
	defer stopWriters(writers...)

	logger.Infow("waiting to receive tracks from c1 and c2")
	success := withTimeout(t, "c3 should receive tracks from both clients", func() bool {
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
	success = withTimeout(t, "c2 tracks should be gone", func() bool {
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
	c2 = createRTCClient("puj_2", ports[rand.Intn(len(ports))])
	defer c2.Stop()
	waitUntilConnected(t, c2)
	writers = publishTracksForClients(t, c2)
	defer stopWriters(writers...)

	logger.Infow("waiting for reconnected c2 tracks to publish")
	success = withTimeout(t, "new c2 tracks should be published again", func() bool {
		tracks := c3.SubscribedTracks()
		if len(tracks[c2.ID()]) != 2 {
			return false
		}
		if len(c1.SubscribedTracks()[c2.ID()]) != 2 {
			return false
		}
		return true
	})
	if !success {
		t.FailNow()
	}
}

// websocket reconnects
func scenarioWSReconnect(t *testing.T) {
	c1 := createRTCClient("wsr_1", defaultServerPort)
	c2 := createRTCClient("wsr_2", defaultServerPort)

	waitUntilConnected(t, c1, c2)

	// c1 publishes track, but disconnects websockets and reconnects
}

func publishTracksForClients(t *testing.T, clients ...*client.RTCClient) []*client.TrackWriter {
	logger.Infow("publishing tracks for clients")
	var writers []*client.TrackWriter
	for i, _ := range clients {
		c := clients[i]
		tw, err := c.AddStaticTrack("audio/opus", "audio", "webcam")
		if !assert.NoError(t, err) {
			t.FailNow()
			return nil
		}
		writers = append(writers, tw)
		tw, err = c.AddStaticTrack("video/vp8", "video", "webcam")
		if !assert.NoError(t, err) {
			t.FailNow()
			return nil
		}
		writers = append(writers, tw)
	}
	return writers
}

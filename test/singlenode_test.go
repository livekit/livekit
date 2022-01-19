package test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/require"
	"github.com/thoas/go-funk"

	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/testutils"
	testclient "github.com/livekit/livekit-server/test/client"
)

func TestClientCouldConnect(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, finish := setupSingleNodeTest("TestClientCouldConnect", testRoom)
	defer finish()

	c1 := createRTCClient("c1", defaultServerPort, nil)
	c2 := createRTCClient("c2", defaultServerPort, nil)
	waitUntilConnected(t, c1, c2)

	// ensure they both see each other
	testutils.WithTimeout(t, "c1 and c2 could connect", func() bool {
		return len(c1.RemoteParticipants()) != 0 && len(c2.RemoteParticipants()) != 0
	})
}

func TestClientConnectDuplicate(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, finish := setupSingleNodeTest("TestClientCouldConnect", testRoom)
	defer finish()

	grant := &auth.VideoGrant{RoomJoin: true, Room: testRoom}
	grant.SetCanPublish(true)
	grant.SetCanSubscribe(true)
	token := joinTokenWithGrant("c1", grant)

	c1 := createRTCClientWithToken(token, defaultServerPort, nil)

	// publish 2 tracks
	t1, err := c1.AddStaticTrack("audio/opus", "audio", "webcam")
	require.NoError(t, err)
	defer t1.Stop()
	t2, err := c1.AddStaticTrack("video/vp8", "video", "webcam")
	require.NoError(t, err)
	defer t2.Stop()

	c2 := createRTCClient("c2", defaultServerPort, nil)
	waitUntilConnected(t, c1, c2)

	opts := &testclient.Options{
		Publish: "duplicate_connection",
	}
	success := testutils.WithTimeout(t, "c2 should receive two tracks", func() bool {
		if len(c2.SubscribedTracks()) == 0 {
			return false
		}
		// should have received three tracks
		if len(c2.SubscribedTracks()[c1.ID()]) != 2 {
			return false
		}

		// participant ID can be appended with '#..' . but should contain orig id as prefix
		tr1 := c2.SubscribedTracks()[c1.ID()][0]
		participantId1, _ := rtc.UnpackStreamID(tr1.StreamID())
		require.Equal(t, c1.ID(), participantId1)
		tr2 := c2.SubscribedTracks()[c1.ID()][1]
		participantId2, _ := rtc.UnpackStreamID(tr2.StreamID())
		require.Equal(t, c1.ID(), participantId2)
		return true
	})
	if !success {
		t.FailNow()
	}

	c1Dup := createRTCClientWithToken(token, defaultServerPort, opts)

	waitUntilConnected(t, c1Dup)

	t3, err := c1Dup.AddStaticTrack("video/vp8", "video", "webcam")
	require.NoError(t, err)
	defer t3.Stop()

	success = testutils.WithTimeout(t, "c2 should receive third tracks", func() bool {
		if len(c2.SubscribedTracks()[c1Dup.ID()]) != 1 {
			return false
		}

		tr3 := c2.SubscribedTracks()[c1Dup.ID()][0]
		participantId3, _ := rtc.UnpackStreamID(tr3.StreamID())
		require.Contains(t, c1Dup.ID(), participantId3)

		return true
	})
	if !success {
		t.FailNow()
	}
}

func TestSinglePublisher(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	s, finish := setupSingleNodeTest("TestSinglePublisher", testRoom)
	defer finish()

	c1 := createRTCClient("c1", defaultServerPort, nil)
	c2 := createRTCClient("c2", defaultServerPort, nil)
	waitUntilConnected(t, c1, c2)

	// publish a track and ensure clients receive it ok
	t1, err := c1.AddStaticTrack("audio/opus", "audio", "webcamaudio")
	require.NoError(t, err)
	defer t1.Stop()
	t2, err := c1.AddStaticTrack("video/vp8", "video", "webcamvideo")
	require.NoError(t, err)
	defer t2.Stop()

	testutils.WithTimeout(t, "c2 should receive two tracks", func() bool {
		if len(c2.SubscribedTracks()) == 0 {
			return false
		}
		// should have received two tracks
		if len(c2.SubscribedTracks()[c1.ID()]) != 2 {
			return false
		}

		tr1 := c2.SubscribedTracks()[c1.ID()][0]
		participantId, _ := rtc.UnpackStreamID(tr1.StreamID())
		require.Equal(t, c1.ID(), participantId)
		return true
	})
	// ensure mime type is received
	remoteC1 := c2.GetRemoteParticipant(c1.ID())
	audioTrack := funk.Find(remoteC1.Tracks, func(ti *livekit.TrackInfo) bool {
		return ti.Name == "webcamaudio"
	}).(*livekit.TrackInfo)
	require.Equal(t, "audio/opus", audioTrack.MimeType)

	// a new client joins and should get the initial stream
	c3 := createRTCClient("c3", defaultServerPort, nil)

	// ensure that new client that has joined also received tracks
	waitUntilConnected(t, c3)
	testutils.WithTimeout(t, "c3 should receive two tracks", func() bool {
		if len(c3.SubscribedTracks()) == 0 {
			return false
		}
		// should have received two tracks
		if len(c3.SubscribedTracks()[c1.ID()]) != 2 {
			return false
		}
		return true
	})

	// ensure that the track ids are generated by server
	tracks := c3.SubscribedTracks()[c1.ID()]
	for _, tr := range tracks {
		require.True(t, strings.HasPrefix(tr.ID(), "TR_"), "track should begin with TR")
	}

	// when c3 disconnects, ensure subscriber is cleaned up correctly
	c3.Stop()

	testutils.WithTimeout(t, "c3 is cleaned up as a subscriber", func() bool {
		room := s.RoomManager().GetRoom(context.Background(), testRoom)
		require.NotNil(t, room)

		p := room.GetParticipant("c1")
		require.NotNil(t, p)

		for _, t := range p.GetPublishedTracks() {
			if t.IsSubscriber(c3.ID()) {
				return false
			}
		}
		return true
	})
}

func Test_WhenAutoSubscriptionDisabled_ClientShouldNotReceiveAnyPublishedTracks(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, finish := setupSingleNodeTest("Test_WhenAutoSubscriptionDisabled_ClientShouldNotReceiveAnyPublishedTracks", testRoom)
	defer finish()

	opts := testclient.Options{AutoSubscribe: false}
	publisher := createRTCClient("publisher", defaultServerPort, &opts)
	client := createRTCClient("client", defaultServerPort, &opts)
	defer publisher.Stop()
	defer client.Stop()
	waitUntilConnected(t, publisher, client)

	track, err := publisher.AddStaticTrack("audio/opus", "audio", "webcam")
	require.NoError(t, err)
	defer track.Stop()

	time.Sleep(syncDelay)

	require.Empty(t, client.SubscribedTracks()[publisher.ID()])
}

func Test_RenegotiationWithDifferentCodecs(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, finish := setupSingleNodeTest("TestRenegotiationWithDifferentCodecs", testRoom)
	defer finish()

	c1 := createRTCClient("c1", defaultServerPort, nil)
	c2 := createRTCClient("c2", defaultServerPort, nil)
	waitUntilConnected(t, c1, c2)

	// publish a vp8 video track and ensure clients receive it ok
	t1, err := c1.AddStaticTrack("audio/opus", "audio", "webcam")
	require.NoError(t, err)
	defer t1.Stop()
	t2, err := c1.AddStaticTrack("video/vp8", "video", "webcam")
	require.NoError(t, err)
	defer t2.Stop()

	success := testutils.WithTimeout(t, "c2 should receive two tracks, video is vp8", func() bool {
		if len(c2.SubscribedTracks()) == 0 {
			return false
		}
		// should have received two tracks
		if len(c2.SubscribedTracks()[c1.ID()]) != 2 {
			return false
		}

		tracks := c2.SubscribedTracks()[c1.ID()]
		for _, t := range tracks {
			if strings.EqualFold(t.Codec().MimeType, "video/vp8") {
				return true
			}
		}
		return false
	})
	if !success {
		t.FailNow()
	}

	t3, err := c1.AddStaticTrackWithCodec(webrtc.RTPCodecCapability{
		MimeType:    "video/h264",
		ClockRate:   90000,
		SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
	}, "videoscreen", "screen")
	defer t3.Stop()
	require.NoError(t, err)

	success = testutils.WithTimeout(t, "c2 should receive two video tracks, one vp8 one h264", func() bool {
		if len(c2.SubscribedTracks()) == 0 {
			return false
		}
		// should have received three tracks
		if len(c2.SubscribedTracks()[c1.ID()]) != 3 {
			return false
		}

		var vp8Found, h264Found bool
		tracks := c2.SubscribedTracks()[c1.ID()]
		for _, t := range tracks {
			if strings.EqualFold(t.Codec().MimeType, "video/vp8") {
				vp8Found = true
			} else if strings.EqualFold(t.Codec().MimeType, "video/h264") {
				h264Found = true
			}
		}
		return vp8Found && h264Found
	})
	if !success {
		t.FailNow()
	}
}

func TestSingleNodeRoomList(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}
	_, finish := setupSingleNodeTest("TestSingleNodeRoomList", testRoom)
	defer finish()

	roomServiceListRoom(t)
}

// Ensure that CORS headers are returned
func TestSingleNodeCORS(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}
	s, finish := setupSingleNodeTest("TestSingleNodeCORS", testRoom)
	defer finish()

	req, err := http.NewRequest("POST", fmt.Sprintf("http://localhost:%d", s.HTTPPort()), nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "bearer xyz")
	req.Header.Set("Origin", "testhost.com")
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, "testhost.com", res.Header.Get("Access-Control-Allow-Origin"))
}

// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package test

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"
	"github.com/thoas/go-funk"
	"github.com/twitchtv/twirp"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/sfu/datachannel"
	"github.com/livekit/livekit-server/pkg/sfu/mime"
	"github.com/livekit/livekit-server/pkg/testutils"
	testclient "github.com/livekit/livekit-server/test/client"
)

const (
	waitTick    = 10 * time.Millisecond
	waitTimeout = 5 * time.Second
)

func TestClientCouldConnect(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, finish := setupSingleNodeTest("TestClientCouldConnect")
	defer finish()

	for _, useSinglePeerConnection := range []bool{false, true} {
		t.Run(fmt.Sprintf("singlePeerConnection=%+v", useSinglePeerConnection), func(t *testing.T) {
			c1 := createRTCClient("c1", defaultServerPort, useSinglePeerConnection, nil)
			c2 := createRTCClient("c2", defaultServerPort, useSinglePeerConnection, nil)
			waitUntilConnected(t, c1, c2)

			// ensure they both see each other
			testutils.WithTimeout(t, func() string {
				if len(c1.RemoteParticipants()) == 0 {
					return "c1 did not see c2"
				}
				if len(c2.RemoteParticipants()) == 0 {
					return "c2 did not see c1"
				}
				return ""
			})
		})
	}
}

func TestClientConnectDuplicate(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, finish := setupSingleNodeTest("TestClientConnectDuplicate")
	defer finish()

	for _, useSinglePeerConnection := range []bool{false, true} {
		t.Run(fmt.Sprintf("singlePeerConnection=%+v", useSinglePeerConnection), func(t *testing.T) {
			grant := &auth.VideoGrant{RoomJoin: true, Room: testRoom}
			grant.SetCanPublish(true)
			grant.SetCanSubscribe(true)
			token := joinTokenWithGrant("c1", grant)
			c1 := createRTCClientWithToken(token, defaultServerPort, useSinglePeerConnection, nil)

			// publish 2 tracks
			t1, err := c1.AddStaticTrack("audio/opus", "audio", "webcam")
			require.NoError(t, err)
			defer t1.Stop()
			t2, err := c1.AddStaticTrack("video/vp8", "video", "webcam")
			require.NoError(t, err)
			defer t2.Stop()

			c2 := createRTCClient("c2", defaultServerPort, useSinglePeerConnection, nil)
			waitUntilConnected(t, c1, c2)

			opts := &testclient.Options{
				Publish: "duplicate_connection",
			}
			testutils.WithTimeout(t, func() string {
				if len(c2.SubscribedTracks()) == 0 {
					return "c2 didn't subscribe to anything"
				}
				// should have received two tracks
				if len(c2.SubscribedTracks()[c1.ID()]) != 2 {
					return "c2 didn't subscribe to both tracks from c1"
				}

				// participant ID can be appended with '#..' . but should contain orig id as prefix
				tr1 := c2.SubscribedTracks()[c1.ID()][0]
				participantId1, _ := rtc.UnpackStreamID(tr1.StreamID())
				require.Equal(t, c1.ID(), participantId1)
				tr2 := c2.SubscribedTracks()[c1.ID()][1]
				participantId2, _ := rtc.UnpackStreamID(tr2.StreamID())
				require.Equal(t, c1.ID(), participantId2)
				return ""
			})

			c1Dup := createRTCClientWithToken(token, defaultServerPort, useSinglePeerConnection, opts)

			waitUntilConnected(t, c1Dup)

			t3, err := c1Dup.AddStaticTrack("video/vp8", "video", "webcam")
			require.NoError(t, err)
			defer t3.Stop()

			testutils.WithTimeout(t, func() string {
				if len(c2.SubscribedTracks()[c1Dup.ID()]) != 1 {
					return "c2 was not subscribed to track from duplicated c1"
				}

				tr3 := c2.SubscribedTracks()[c1Dup.ID()][0]
				participantId3, _ := rtc.UnpackStreamID(tr3.StreamID())
				require.Contains(t, c1Dup.ID(), participantId3)

				return ""
			})
		})
	}
}

func TestSinglePublisher(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	s, finish := setupSingleNodeTest("TestSinglePublisher")
	defer finish()

	for _, useSinglePeerConnection := range []bool{false, true} {
		t.Run(fmt.Sprintf("singlePeerConnection=%+v", useSinglePeerConnection), func(t *testing.T) {
			c1 := createRTCClient("c1", defaultServerPort, useSinglePeerConnection, nil)
			c2 := createRTCClient("c2", defaultServerPort, useSinglePeerConnection, nil)
			waitUntilConnected(t, c1, c2)

			// publish a track and ensure clients receive it ok
			t1, err := c1.AddStaticTrack("audio/OPUS", "audio", "webcamaudio")
			require.NoError(t, err)
			defer t1.Stop()
			t2, err := c1.AddStaticTrack("video/vp8", "video", "webcamvideo")
			require.NoError(t, err)
			defer t2.Stop()

			testutils.WithTimeout(t, func() string {
				if len(c2.SubscribedTracks()) == 0 {
					return "c2 was not subscribed to anything"
				}
				// should have received two tracks
				if len(c2.SubscribedTracks()[c1.ID()]) != 2 {
					return "c2 didn't subscribe to both tracks from c1"
				}

				tr1 := c2.SubscribedTracks()[c1.ID()][0]
				participantId, _ := rtc.UnpackStreamID(tr1.StreamID())
				require.Equal(t, c1.ID(), participantId)
				return ""
			})
			// ensure mime type is received
			remoteC1 := c2.GetRemoteParticipant(c1.ID())
			audioTrack := funk.Find(remoteC1.Tracks, func(ti *livekit.TrackInfo) bool {
				return ti.Name == "webcamaudio"
			}).(*livekit.TrackInfo)
			require.Equal(t, "audio/opus", audioTrack.MimeType)

			// a new client joins and should get the initial stream
			c3 := createRTCClient("c3", defaultServerPort, useSinglePeerConnection, nil)

			// ensure that new client that has joined also received tracks
			waitUntilConnected(t, c3)
			testutils.WithTimeout(t, func() string {
				if len(c3.SubscribedTracks()) == 0 {
					return "c3 didn't subscribe to anything"
				}
				// should have received two tracks
				if len(c3.SubscribedTracks()[c1.ID()]) != 2 {
					return "c3 didn't subscribe to tracks from c1"
				}
				return ""
			})

			// ensure that the track ids are generated by server
			tracks := c3.SubscribedTracks()[c1.ID()]
			for _, tr := range tracks {
				require.True(t, strings.HasPrefix(tr.ID(), "TR_"), "track should begin with TR")
			}

			// when c3 disconnects, ensure subscriber is cleaned up correctly
			c3.Stop()

			testutils.WithTimeout(t, func() string {
				room := s.RoomManager().GetRoom(context.Background(), testRoom)
				p := room.GetParticipant("c1")
				require.NotNil(t, p)

				for _, t := range p.GetPublishedTracks() {
					if t.IsSubscriber(c3.ID()) {
						return "c3 was not a subscriber of c1's tracks"
					}
				}
				return ""
			})
		})
	}
}

func Test_WhenAutoSubscriptionDisabled_ClientShouldNotReceiveAnyPublishedTracks(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, finish := setupSingleNodeTest("Test_WhenAutoSubscriptionDisabled_ClientShouldNotReceiveAnyPublishedTracks")
	defer finish()

	for _, useSinglePeerConnection := range []bool{false, true} {
		t.Run(fmt.Sprintf("singlePeerConnection=%+v", useSinglePeerConnection), func(t *testing.T) {
			opts := testclient.Options{AutoSubscribe: false}
			publisher := createRTCClient("publisher", defaultServerPort, useSinglePeerConnection, &opts)
			client := createRTCClient("client", defaultServerPort, useSinglePeerConnection, &opts)
			defer publisher.Stop()
			defer client.Stop()
			waitUntilConnected(t, publisher, client)

			track, err := publisher.AddStaticTrack("audio/opus", "audio", "webcam")
			require.NoError(t, err)
			defer track.Stop()

			time.Sleep(syncDelay)

			require.Empty(t, client.SubscribedTracks()[publisher.ID()])
		})
	}
}

func Test_RenegotiationWithDifferentCodecs(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, finish := setupSingleNodeTest("TestRenegotiationWithDifferentCodecs")
	defer finish()

	for _, useSinglePeerConnection := range []bool{false, true} {
		t.Run(fmt.Sprintf("singlePeerConnection=%+v", useSinglePeerConnection), func(t *testing.T) {
			c1 := createRTCClient("c1", defaultServerPort, useSinglePeerConnection, nil)
			c2 := createRTCClient("c2", defaultServerPort, useSinglePeerConnection, nil)
			waitUntilConnected(t, c1, c2)

			// publish a vp8 video track and ensure clients receive it ok
			t1, err := c1.AddStaticTrack("audio/opus", "audio", "webcam")
			require.NoError(t, err)
			defer t1.Stop()
			t2, err := c1.AddStaticTrack("video/vp8", "video", "webcam")
			require.NoError(t, err)
			defer t2.Stop()

			testutils.WithTimeout(t, func() string {
				if len(c2.SubscribedTracks()) == 0 {
					return "c2 was not subscribed to anything"
				}
				// should have received two tracks
				if len(c2.SubscribedTracks()[c1.ID()]) != 2 {
					return "c2 was not subscribed to tracks from c1"
				}

				tracks := c2.SubscribedTracks()[c1.ID()]
				for _, t := range tracks {
					if mime.IsMimeTypeStringVP8(t.Codec().MimeType) {
						return ""

					}
				}
				return "did not receive track with vp8"
			})

			t3, err := c1.AddStaticTrackWithCodec(webrtc.RTPCodecCapability{
				MimeType:    "video/h264",
				ClockRate:   90000,
				SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
			}, "videoscreen", "screen")
			defer t3.Stop()
			require.NoError(t, err)

			testutils.WithTimeout(t, func() string {
				if len(c2.SubscribedTracks()) == 0 {
					return "c2's not subscribed to anything"
				}
				// should have received three tracks
				if len(c2.SubscribedTracks()[c1.ID()]) != 3 {
					return "c2's not subscribed to 3 tracks from c1"
				}

				var vp8Found, h264Found bool
				tracks := c2.SubscribedTracks()[c1.ID()]
				for _, t := range tracks {
					if mime.IsMimeTypeStringVP8(t.Codec().MimeType) {
						vp8Found = true
					} else if mime.IsMimeTypeStringH264(t.Codec().MimeType) {
						h264Found = true
					}
				}
				if !vp8Found {
					return "did not receive track with vp8"
				}
				if !h264Found {
					return "did not receive track with h264"
				}
				return ""
			})
		})
	}
}

func TestSingleNodeRoomList(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}
	_, finish := setupSingleNodeTest("TestSingleNodeRoomList")
	defer finish()

	roomServiceListRoom(t)
}

func TestSingleNodeUpdateParticipant(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}
	_, finish := setupSingleNodeTest("TestSingleNodeRoomList")
	defer finish()

	adminCtx := contextWithToken(adminRoomToken(testRoom))
	t.Run("update nonexistent participant", func(t *testing.T) {
		_, err := roomClient.UpdateParticipant(adminCtx, &livekit.UpdateParticipantRequest{
			Room:     testRoom,
			Identity: "nonexistent",
			Permission: &livekit.ParticipantPermission{
				CanPublish: true,
			},
		})
		require.Error(t, err)
		var twErr twirp.Error
		require.True(t, errors.As(err, &twErr))
		require.Equal(t, twirp.NotFound, twErr.Code())
	})
}

// Ensure that CORS headers are returned
func TestSingleNodeCORS(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}
	s, finish := setupSingleNodeTest("TestSingleNodeCORS")
	defer finish()

	req, err := http.NewRequest("POST", fmt.Sprintf("http://localhost:%d", s.HTTPPort()), nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "bearer xyz")
	req.Header.Set("Origin", "testhost.com")
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, "testhost.com", res.Header.Get("Access-Control-Allow-Origin"))
}

func TestSingleNodeDoubleSlash(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}
	s, finish := setupSingleNodeTest("TestSingleNodeDoubleSlash")
	defer finish()
	// client contains trailing slash in URL, causing path to contain double //
	// without our middleware, this would cause a 302 redirect
	roomClient = livekit.NewRoomServiceJSONClient(fmt.Sprintf("http://localhost:%d/", s.HTTPPort()), &http.Client{})
	_, err := roomClient.ListRooms(contextWithToken(listRoomToken()), &livekit.ListRoomsRequest{})
	require.NoError(t, err)
}

func TestPingPong(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}
	_, finish := setupSingleNodeTest("TestPingPong")
	defer finish()

	for _, useSinglePeerConnection := range []bool{false, true} {
		t.Run(fmt.Sprintf("singlePeerConnection=%+v", useSinglePeerConnection), func(t *testing.T) {
			c1 := createRTCClient("c1", defaultServerPort, useSinglePeerConnection, nil)
			waitUntilConnected(t, c1)

			require.NoError(t, c1.SendPing())
			require.Eventually(t, func() bool {
				return c1.PongReceivedAt() > 0
			}, time.Second, 10*time.Millisecond)
		})
	}
}

func TestSingleNodeJoinAfterClose(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, finish := setupSingleNodeTest("TestJoinAfterClose")
	defer finish()

	scenarioJoinClosedRoom(t)
}

func TestSingleNodeCloseNonRTCRoom(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, finish := setupSingleNodeTest("closeNonRTCRoom")
	defer finish()

	closeNonRTCRoom(t)
}

func TestAutoCreate(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}
	disableAutoCreate := func(conf *config.Config) {
		conf.Room.AutoCreate = false
	}
	t.Run("cannot join if room isn't created", func(t *testing.T) {
		s := createSingleNodeServer(disableAutoCreate)
		go func() {
			if err := s.Start(); err != nil {
				logger.Errorw("server returned error", err)
			}
		}()
		defer s.Stop(true)

		waitForServerToStart(s)

		for _, useSinglePeerConnection := range []bool{false, true} {
			t.Run(fmt.Sprintf("singlePeerConnection=%+v", useSinglePeerConnection), func(t *testing.T) {
				token := joinToken(testRoom, "start-before-create", nil)
				_, err := testclient.NewWebSocketConn(fmt.Sprintf("ws://localhost:%d", defaultServerPort), token, &testclient.Options{UseJoinRequestQueryParam: useSinglePeerConnection})
				require.Error(t, err)

				// second join should also fail
				token = joinToken(testRoom, "start-before-create-2", nil)
				_, err = testclient.NewWebSocketConn(fmt.Sprintf("ws://localhost:%d", defaultServerPort), token, &testclient.Options{UseJoinRequestQueryParam: useSinglePeerConnection})
				require.Error(t, err)
			})
		}
	})

	t.Run("join with explicit createRoom", func(t *testing.T) {
		s := createSingleNodeServer(disableAutoCreate)
		go func() {
			if err := s.Start(); err != nil {
				logger.Errorw("server returned error", err)
			}
		}()
		defer s.Stop(true)

		waitForServerToStart(s)

		// explicitly create
		_, err := roomClient.CreateRoom(contextWithToken(createRoomToken()), &livekit.CreateRoomRequest{Name: testRoom})
		require.NoError(t, err)

		for _, useSinglePeerConnection := range []bool{false, true} {
			t.Run(fmt.Sprintf("singlePeerConnection=%+v", useSinglePeerConnection), func(t *testing.T) {
				c1 := createRTCClient("join-after-create", defaultServerPort, useSinglePeerConnection, nil)
				waitUntilConnected(t, c1)

				c1.Stop()
			})
		}
	})
}

// don't give user subscribe permissions initially, and ensure autosubscribe is triggered afterwards
func TestSingleNodeUpdateSubscriptionPermissions(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}
	_, finish := setupSingleNodeTest("TestSingleNodeUpdateSubscriptionPermissions")
	defer finish()

	for _, useSinglePeerConnection := range []bool{false, true} {
		t.Run(fmt.Sprintf("singlePeerConnection=%+v", useSinglePeerConnection), func(t *testing.T) {
			pub := createRTCClient("pub", defaultServerPort, useSinglePeerConnection, nil)

			grant := &auth.VideoGrant{RoomJoin: true, Room: testRoom}
			grant.SetCanSubscribe(false)
			at := auth.NewAccessToken(testApiKey, testApiSecret).
				AddGrant(grant).
				SetIdentity("sub")
			token, err := at.ToJWT()
			require.NoError(t, err)
			sub := createRTCClientWithToken(token, defaultServerPort, useSinglePeerConnection, nil)

			waitUntilConnected(t, pub, sub)

			writers := publishTracksForClients(t, pub)
			defer stopWriters(writers...)

			// wait sub receives tracks
			testutils.WithTimeout(t, func() string {
				pubRemote := sub.GetRemoteParticipant(pub.ID())
				if pubRemote == nil {
					return "could not find remote publisher"
				}
				if len(pubRemote.Tracks) != 2 {
					return "did not receive metadata for published tracks"
				}
				return ""
			})

			// set permissions out of band
			ctx := contextWithToken(adminRoomToken(testRoom))
			_, err = roomClient.UpdateParticipant(ctx, &livekit.UpdateParticipantRequest{
				Room:     testRoom,
				Identity: "sub",
				Permission: &livekit.ParticipantPermission{
					CanSubscribe: true,
					CanPublish:   true,
				},
			})
			require.NoError(t, err)

			testutils.WithTimeout(t, func() string {
				tracks := sub.SubscribedTracks()[pub.ID()]
				if len(tracks) == 2 {
					return ""
				} else {
					return fmt.Sprintf("expected 2 tracks subscribed, actual: %d", len(tracks))
				}
			})
		})
	}
}

func TestSingleNodeAttributes(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}
	_, finish := setupSingleNodeTest("TestSingleNodeAttributes")
	defer finish()

	for _, useSinglePeerConnection := range []bool{false, true} {
		t.Run(fmt.Sprintf("singlePeerConnection=%+v", useSinglePeerConnection), func(t *testing.T) {
			pub := createRTCClient("pub", defaultServerPort, useSinglePeerConnection, &testclient.Options{
				Attributes: map[string]string{
					"b": "2",
					"c": "3",
				},
				TokenCustomizer: func(token *auth.AccessToken, grants *auth.VideoGrant) {
					T := true
					grants.CanUpdateOwnMetadata = &T
					token.SetAttributes(map[string]string{
						"a": "0",
						"b": "1",
					})
				},
			})

			grant := &auth.VideoGrant{RoomJoin: true, Room: testRoom}
			grant.SetCanSubscribe(false)
			at := auth.NewAccessToken(testApiKey, testApiSecret).
				SetVideoGrant(grant).
				SetIdentity("sub")
			token, err := at.ToJWT()
			require.NoError(t, err)
			sub := createRTCClientWithToken(token, defaultServerPort, useSinglePeerConnection, nil)

			waitUntilConnected(t, pub, sub)

			// wait sub receives initial attributes
			testutils.WithTimeout(t, func() string {
				pubRemote := sub.GetRemoteParticipant(pub.ID())
				if pubRemote == nil {
					return "could not find remote publisher"
				}
				attrs := pubRemote.Attributes
				if !reflect.DeepEqual(attrs, map[string]string{
					"a": "0",
					"b": "2",
					"c": "3",
				}) {
					return fmt.Sprintf("did not receive expected attributes: %v", attrs)
				}
				return ""
			})
		})
	}
}

// TestDeviceCodecOverride checks that codecs that are incompatible with a device is not
// negotiated by the server
func TestDeviceCodecOverride(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, finish := setupSingleNodeTest("TestDeviceCodecOverride")
	defer finish()

	for _, useSinglePeerConnection := range []bool{false, true} {
		t.Run(fmt.Sprintf("singlePeerConnection=%+v", useSinglePeerConnection), func(t *testing.T) {
			// simulate device that isn't compatible with H.264
			c1 := createRTCClient("c1", defaultServerPort, useSinglePeerConnection, &testclient.Options{
				ClientInfo: &livekit.ClientInfo{
					Os:          "android",
					DeviceModel: "Xiaomi 2201117TI",
				},
			})
			defer c1.Stop()
			waitUntilConnected(t, c1)

			// it doesn't really matter what the codec set here is, uses default Pion MediaEngine codecs
			tw, err := c1.AddStaticTrack("video/h264", "video", "webcam")
			require.NoError(t, err)
			defer stopWriters(tw)

			var desc *sdp.MediaDescription
			require.Eventually(t, func() bool {
				lastAnswer := c1.LastAnswer()
				if lastAnswer == nil {
					return false
				}

				sd := webrtc.SessionDescription{
					Type: webrtc.SDPTypeAnswer,
					SDP:  lastAnswer.SDP,
				}
				answer, err := sd.Unmarshal()
				require.NoError(t, err)

				// video and data channel
				if len(answer.MediaDescriptions) < 2 {
					return false
				}

				for _, md := range answer.MediaDescriptions {
					if md.MediaName.Media == "video" {
						desc = md
						break
					}
				}
				if desc == nil {
					return false
				}

				return true
			}, waitTimeout, waitTick, "did not receive answer")

			hasSeenVP8 := false
			for _, a := range desc.Attributes {
				if a.Key == "rtpmap" {
					require.NotContains(t, a.Value, mime.MimeTypeCodecH264.String(), "should not contain H264 codec")
					if strings.Contains(a.Value, mime.MimeTypeCodecVP8.String()) {
						hasSeenVP8 = true
					}
				}
			}
			require.True(t, hasSeenVP8, "should have seen VP8 codec in SDP")
		})
	}
}

func TestSubscribeToCodecUnsupported(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, finish := setupSingleNodeTest("TestSubscribeToCodecUnsupported")
	defer finish()

	for _, useSinglePeerConnection := range []bool{false, true} {
		t.Run(fmt.Sprintf("singlePeerConnection=%+v", useSinglePeerConnection), func(t *testing.T) {
			c1 := createRTCClient("c1", defaultServerPort, useSinglePeerConnection, nil)
			// create a client that doesn't support H264
			c2 := createRTCClient("c2", defaultServerPort, useSinglePeerConnection, &testclient.Options{
				AutoSubscribe: true,
				DisabledCodecs: []webrtc.RTPCodecCapability{
					{MimeType: "video/H264"},
				},
			})
			waitUntilConnected(t, c1, c2)

			// publish a vp8 video track and ensure c2 receives it ok
			t1, err := c1.AddStaticTrack("audio/opus", "audio", "webcam")
			require.NoError(t, err)
			defer t1.Stop()
			t2, err := c1.AddStaticTrack("video/vp8", "video", "webcam")
			require.NoError(t, err)
			defer t2.Stop()

			testutils.WithTimeout(t, func() string {
				if len(c2.SubscribedTracks()) == 0 {
					return "c2 was not subscribed to anything"
				}
				// should have received two tracks
				if len(c2.SubscribedTracks()[c1.ID()]) != 2 {
					return "c2 was not subscribed to tracks from c1"
				}

				tracks := c2.SubscribedTracks()[c1.ID()]
				for _, t := range tracks {
					if mime.IsMimeTypeStringVP8(t.Codec().MimeType) {
						return ""
					}
				}
				return "did not receive track with vp8"
			})
			require.Nil(t, c2.GetSubscriptionResponseAndClear())

			// publish a h264 track and ensure c2 got subscription error
			t3, err := c1.AddStaticTrackWithCodec(webrtc.RTPCodecCapability{
				MimeType:    "video/h264",
				ClockRate:   90000,
				SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
			}, "videoscreen", "screen")
			defer t3.Stop()
			require.NoError(t, err)

			var h264TrackID string
			require.Eventually(t, func() bool {
				remoteC1 := c2.GetRemoteParticipant(c1.ID())
				require.NotNil(t, remoteC1)
				for _, track := range remoteC1.Tracks {
					if mime.IsMimeTypeStringH264(track.MimeType) {
						h264TrackID = track.Sid
						return true
					}
				}
				return false
			}, time.Second, 10*time.Millisecond, "did not receive track info with h264")

			require.Eventually(t, func() bool {
				sr := c2.GetSubscriptionResponseAndClear()
				if sr == nil {
					return false
				}
				require.Equal(t, h264TrackID, sr.TrackSid)
				require.Equal(t, livekit.SubscriptionError_SE_CODEC_UNSUPPORTED, sr.Err)
				return true
			}, 5*time.Second, 10*time.Millisecond, "did not receive subscription response")

			// publish another vp8 track again, ensure the transport recovered by sfu and c2 can receive it
			t4, err := c1.AddStaticTrack("video/vp8", "video2", "webcam2")
			require.NoError(t, err)
			defer t4.Stop()

			testutils.WithTimeout(t, func() string {
				if len(c2.SubscribedTracks()) == 0 {
					return "c2 was not subscribed to anything"
				}
				// should have received two tracks
				if len(c2.SubscribedTracks()[c1.ID()]) != 3 {
					return "c2 was not subscribed to tracks from c1"
				}

				var vp8Count int
				tracks := c2.SubscribedTracks()[c1.ID()]
				for _, t := range tracks {
					if mime.IsMimeTypeStringVP8(t.Codec().MimeType) {
						vp8Count++
					}
				}
				if vp8Count == 2 {
					return ""
				}
				return "did not 2 receive track with vp8"
			})
			require.Nil(t, c2.GetSubscriptionResponseAndClear())
		})
	}
}

func TestDataPublishSlowSubscriber(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	dataChannelSlowThreshold := 21024

	logger.Infow("----------------STARTING TEST----------------", "test", t.Name())
	s := createSingleNodeServer(func(c *config.Config) {
		c.RTC.DatachannelSlowThreshold = dataChannelSlowThreshold
	})
	go func() {
		if err := s.Start(); err != nil {
			logger.Errorw("server returned error", err)
		}
	}()

	waitForServerToStart(s)

	defer func() {
		s.Stop(true)
		logger.Infow("----------------FINISHING TEST----------------", "test", t.Name())
	}()

	for _, useSinglePeerConnection := range []bool{false, true} {
		t.Run(fmt.Sprintf("singlePeerConnection=%+v", useSinglePeerConnection), func(t *testing.T) {
			pub := createRTCClient("pub", defaultServerPort, useSinglePeerConnection, nil)
			fastSub := createRTCClient("fastSub", defaultServerPort, useSinglePeerConnection, nil)
			slowSubNotDrop := createRTCClient("slowSubNotDrop", defaultServerPort, useSinglePeerConnection, nil)
			slowSubDrop := createRTCClient("slowSubDrop", defaultServerPort, useSinglePeerConnection, nil)
			waitUntilConnected(t, pub, fastSub, slowSubDrop, slowSubNotDrop)
			defer func() {
				pub.Stop()
				fastSub.Stop()
				slowSubNotDrop.Stop()
				slowSubDrop.Stop()
			}()

			// no data should be dropped for fast subscriber
			var fastDataIndex atomic.Uint64
			fastSub.OnDataReceived = func(data []byte, sid string) {
				idx := binary.BigEndian.Uint64(data[len(data)-8:])
				require.Equal(t, fastDataIndex.Load()+1, idx)
				fastDataIndex.Store(idx)
			}

			// no data should be dropped for slow subscriber that is above threshold
			var slowNoDropDataIndex atomic.Uint64
			var drainSlowSubNotDrop atomic.Bool
			slowNoDropReader := testclient.NewDataChannelReader(dataChannelSlowThreshold * 2)
			slowSubNotDrop.OnDataReceived = func(data []byte, sid string) {
				idx := binary.BigEndian.Uint64(data[len(data)-8:])
				require.Equal(t, slowNoDropDataIndex.Load()+1, idx)
				slowNoDropDataIndex.Store(idx)
				if !drainSlowSubNotDrop.Load() {
					slowNoDropReader.Read(data, sid)
				}
			}

			// data should be dropped for slow subscriber that is below threshold
			var slowDropDataIndex atomic.Uint64
			dropped := make(chan struct{})
			slowDropReader := testclient.NewDataChannelReader(dataChannelSlowThreshold / 2)
			slowSubDrop.OnDataReceived = func(data []byte, sid string) {
				select {
				case <-dropped:
					return
				default:
				}
				idx := binary.BigEndian.Uint64(data[len(data)-8:])
				if idx != slowDropDataIndex.Load()+1 {
					close(dropped)
				}
				slowDropDataIndex.Store(idx)
				slowDropReader.Read(data, sid)
			}

			// publisher sends data as fast as possible, it will block by the slowest subscriber above the slow threshold
			var (
				blocked   atomic.Bool
				stopWrite atomic.Bool
				writeIdx  atomic.Uint64
			)
			writeStopped := make(chan struct{})
			go func() {
				defer close(writeStopped)
				var i int
				buf := make([]byte, 100)
				for !stopWrite.Load() {
					i++
					binary.BigEndian.PutUint64(buf[len(buf)-8:], uint64(i))
					if err := pub.PublishData(buf, livekit.DataPacket_RELIABLE); err != nil {
						if errors.Is(err, datachannel.ErrDataDroppedBySlowReader) {
							blocked.Store(true)
							i--
							continue
						} else {
							t.Log("error writing", err)
							break
						}
					}
					writeIdx.Store(uint64(i))
				}
			}()

			<-dropped

			time.Sleep(time.Second)
			blocked.Store(false)
			require.Eventually(t, func() bool { return blocked.Load() }, 30*time.Second, 100*time.Millisecond)
			stopWrite.Store(true)
			<-writeStopped
			drainSlowSubNotDrop.Store(true)
			require.Eventually(t, func() bool {
				return writeIdx.Load() == fastDataIndex.Load() &&
					writeIdx.Load() == slowNoDropDataIndex.Load()
			}, 10*time.Second, 50*time.Millisecond, "writeIdx %d, fast %d, slowNoDrop %d", writeIdx.Load(), fastDataIndex.Load(), slowNoDropDataIndex.Load())
		})
	}
}

func TestFireTrackBySdp(t *testing.T) {
	_, finish := setupSingleNodeTest("TestFireTrackBySdp")
	defer finish()

	var cases = []struct {
		name   string
		codecs []webrtc.RTPCodecCapability
		pubSDK livekit.ClientInfo_SDK
	}{
		{
			name: "js client could pub a/v tracks",
			codecs: []webrtc.RTPCodecCapability{
				{MimeType: mime.MimeTypeH264.String()},
				{MimeType: mime.MimeTypeOpus.String()},
			},
			pubSDK: livekit.ClientInfo_JS,
		},
		{
			name: "go client could pub audio tracks",
			codecs: []webrtc.RTPCodecCapability{
				{MimeType: "audio/opus"},
			},
			pubSDK: livekit.ClientInfo_GO,
		},
	}

	for _, c := range cases {
		codecs, sdk := c.codecs, c.pubSDK
		t.Run(c.name, func(t *testing.T) {
			for _, useSinglePeerConnection := range []bool{false, true} {
				t.Run(fmt.Sprintf("singlePeerConnection=%+v", useSinglePeerConnection), func(t *testing.T) {
					c1 := createRTCClient(c.name+"_c1", defaultServerPort, useSinglePeerConnection, &testclient.Options{
						ClientInfo: &livekit.ClientInfo{
							Sdk: sdk,
						},
					})
					c2 := createRTCClient(c.name+"_c2", defaultServerPort, useSinglePeerConnection, &testclient.Options{
						AutoSubscribe: true,
						ClientInfo: &livekit.ClientInfo{
							Sdk: livekit.ClientInfo_JS,
						},
					})
					waitUntilConnected(t, c1, c2)
					defer func() {
						c1.Stop()
						c2.Stop()
					}()

					// publish tracks and don't write any packets
					for _, codec := range codecs {
						_, err := c1.AddStaticTrackWithCodec(codec, codec.MimeType, codec.MimeType, testclient.AddTrackNoWriter())
						require.NoError(t, err)
					}

					require.Eventually(t, func() bool {
						return len(c2.SubscribedTracks()[c1.ID()]) == len(codecs)
					}, 5*time.Second, 10*time.Millisecond)

					var found int
					for _, pubTrack := range c1.GetPublishedTrackIDs() {
						t.Log("pub track", pubTrack)
						tracks := c2.SubscribedTracks()[c1.ID()]
						for _, track := range tracks {
							t.Log("sub track", track.ID(), track.Codec())
							if track.Codec().PayloadType == 0 && track.ID() == pubTrack {
								found++
								break
							}
						}
					}
					require.Equal(t, len(codecs), found)
				})
			}
		})
	}
}

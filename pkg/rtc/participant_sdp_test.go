// Copyright 2026 LiveKit, Inc.
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

package rtc

import (
	"runtime"
	"strings"
	"testing"

	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
)

func TestConfigurePublisherAnswerWhileTrackMovesToPublished(t *testing.T) {
	const cid = "C"
	p := newParticipantForTest("test")
	p.AddTrack(&livekit.AddTrackRequest{Cid: cid, Name: "mic", Type: livekit.TrackType_AUDIO})

	offer := &sdp.SessionDescription{}
	require.NoError(t, offer.UnmarshalString(strings.Join([]string{
		"v=0",
		"o=- 0 0 IN IP4 127.0.0.1",
		"s=-",
		"t=0 0",
		"m=audio 9 UDP/TLS/RTP/SAVPF 111",
		"c=IN IP4 0.0.0.0",
		"a=mid:0",
		"a=msid:- " + cid,
		"a=sendonly",
		"a=rtpmap:111 opus/48000/2",
		"a=fmtp:111 minptime=10;stereo=1;useinbandfec=1",
		"",
	}, "\r\n")))
	p.TransportManager.publisher.setRemoteOfferParsed(webrtc.SDPTypeOffer, offer)

	done := make(chan struct{})
	started := false
	decoy := &typesfakes.FakeLocalMediaTrack{}
	decoy.HasSdpCidCalls(func(string) bool {
		if started {
			return false
		}
		started = true
		go func() {
			p.pendingTracksLock.RWMutex.Lock()
			signalCid, ti, _, _, _ := p.getPendingTrack(cid, livekit.TrackType_AUDIO, true)
			p.addMediaTrack(signalCid, ti)
			p.pendingTracksLock.RWMutex.Unlock()
			close(done)
		}()
		// a failed TryRLock means the mover holds the lock or is queued, and a queued writer blocks later readers
		for {
			select {
			case <-done:
				return false
			default:
			}
			if !p.pendingTracksLock.RWMutex.TryRLock() {
				return false
			}
			p.pendingTracksLock.RWMutex.RUnlock()
			runtime.Gosched()
		}
	})
	p.UpTrackManager.AddPublishedTrack(decoy)

	answer := p.configurePublisherAnswer(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP: strings.Join([]string{
			"v=0",
			"o=- 0 0 IN IP4 127.0.0.1",
			"s=-",
			"t=0 0",
			"m=audio 9 UDP/TLS/RTP/SAVPF 111",
			"c=IN IP4 0.0.0.0",
			"a=mid:0",
			"a=recvonly",
			"a=rtpmap:111 opus/48000/2",
			"a=fmtp:111 minptime=10;stereo=1;useinbandfec=1",
			"",
		}, "\r\n"),
	})

	<-done

	parsed, err := answer.Unmarshal()
	require.NoError(t, err)
	fmtp, ok := parsed.MediaDescriptions[0].Attribute("fmtp")
	require.True(t, ok)
	require.NotContains(t, fmtp, "stereo=1")
	require.Contains(t, fmtp, "usedtx=1")
}

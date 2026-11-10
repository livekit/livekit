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

func newParticipantWithOpusOffer(t *testing.T, cid string, features ...livekit.AudioTrackFeature) *ParticipantImpl {
	p := newParticipantForTest("test")
	p.AddTrack(&livekit.AddTrackRequest{Cid: cid, Name: "mic", Type: livekit.TrackType_AUDIO, AudioFeatures: features})
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
	return p
}

// the opus fmtp of the configured answer as pion parses it, payload type included, or "" when the line was dropped
func publisherAnswerFmtp(t *testing.T, p *ParticipantImpl, answerFmtp string) string {
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
			"a=fmtp:111 " + answerFmtp,
			"",
		}, "\r\n"),
	})
	parsed, err := answer.Unmarshal()
	require.NoError(t, err)
	fmtp, _ := parsed.MediaDescriptions[0].Attribute("fmtp")
	return fmtp
}

// opus at 11 prefixes RED's 111, and RED's fmtp must not go through the opus rewrite
func TestConfigurePublisherAnswerMatchesOpusPayloadTypeExactly(t *testing.T) {
	p := newParticipantWithOpusOffer(t, "C")
	answer := p.configurePublisherAnswer(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP: strings.Join([]string{
			"v=0",
			"o=- 0 0 IN IP4 127.0.0.1",
			"s=-",
			"t=0 0",
			"m=audio 9 UDP/TLS/RTP/SAVPF 11 111",
			"c=IN IP4 0.0.0.0",
			"a=mid:0",
			"a=recvonly",
			"a=rtpmap:11 opus/48000/2",
			"a=fmtp:11 useinbandfec=1",
			"a=rtpmap:111 red/48000/2",
			"a=fmtp:111 11/11",
			"",
		}, "\r\n"),
	})
	parsed, err := answer.Unmarshal()
	require.NoError(t, err)
	var attrs []string
	for _, attr := range parsed.MediaDescriptions[0].Attributes {
		attrs = append(attrs, attr.String())
	}
	require.Equal(t, []string{
		"mid:0",
		"recvonly",
		"rtpmap:11 opus/48000/2",
		"fmtp:11 useinbandfec=1;usedtx=1",
		"rtpmap:111 red/48000/2",
		"fmtp:111 11/11",
	}, attrs)
}

func TestConfigurePublisherAnswerWhileTrackMovesToPublished(t *testing.T) {
	const cid = "C"
	p := newParticipantWithOpusOffer(t, cid)

	done := make(chan struct{})
	started := false
	decoy := &typesfakes.FakeLocalMediaTrack{}
	decoy.IsOpenReturns(true)
	decoy.HasSdpCidCalls(func(string) bool {
		if started {
			return false
		}
		started = true
		go func() {
			defer close(done)
			p.pendingTracksLock.RWMutex.Lock()
			defer p.pendingTracksLock.RWMutex.Unlock()
			signalCid, ti, _, _, _ := p.getPendingTrack(cid, livekit.TrackType_AUDIO, true)
			p.addMediaTrack(signalCid, ti)
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

	fmtp := publisherAnswerFmtp(t, p, "minptime=10;stereo=1;useinbandfec=1")
	<-done
	require.NotContains(t, fmtp, "stereo=1")
	require.Contains(t, fmtp, "usedtx=1")
}

func TestConfigurePublisherAnswerRewritesOpusFmtpByKey(t *testing.T) {
	for _, tc := range []struct {
		name     string
		features []livekit.AudioTrackFeature
		fmtp     string
		want     string
	}{
		{
			name: "leading stereo is dropped",
			fmtp: "stereo=1;useinbandfec=1",
			want: "111 useinbandfec=1;usedtx=1",
		},
		{
			name:     "params keep their order",
			features: []livekit.AudioTrackFeature{livekit.AudioTrackFeature_TF_STEREO},
			fmtp:     "minptime=10;useinbandfec=1",
			want:     "111 minptime=10;useinbandfec=1;usedtx=1;stereo=1;maxaveragebitrate=510000",
		},
		{
			name:     "upper-case stereo and an existing maxaveragebitrate are replaced",
			features: []livekit.AudioTrackFeature{livekit.AudioTrackFeature_TF_STEREO},
			fmtp:     "STEREO=1;maxaveragebitrate=64000;useinbandfec=1",
			want:     "111 useinbandfec=1;usedtx=1;stereo=1;maxaveragebitrate=510000",
		},
		{
			name:     "no params kept drops the fmtp line",
			features: []livekit.AudioTrackFeature{livekit.AudioTrackFeature_TF_NO_DTX},
			fmtp:     "stereo=1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newParticipantWithOpusOffer(t, "C", tc.features...)
			require.Equal(t, tc.want, publisherAnswerFmtp(t, p, tc.fmtp))
		})
	}
}

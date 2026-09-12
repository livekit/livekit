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

package sfu

import (
	"testing"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/sfu/flexfec"
)

type fecTrackContext struct {
	webrtc.TrackLocalContext
	ssrcFEC webrtc.SSRC
}

func (c fecTrackContext) SSRCForwardErrorCorrection() webrtc.SSRC { return c.ssrcFEC }

func TestDownTrackFECNegotiationAndLifecycle(t *testing.T) {
	fecCodec := webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: "video/FLEXFEC-03", ClockRate: 90000}, PayloadType: 118}
	for _, name := range []string{"enabled", "disabled", "audio", "no codec", "no ssrc"} {
		t.Run(name, func(t *testing.T) {
			d := &DownTrack{params: DownTrackParams{EnableFlexFEC: name != "disabled"}, kind: webrtc.RTPCodecTypeVideo, negotiatedCodecParameters: []webrtc.RTPCodecParameters{fecCodec}}
			c := fecTrackContext{ssrcFEC: 456}
			switch name {
			case "audio":
				d.kind = webrtc.RTPCodecTypeAudio
			case "no codec":
				d.negotiatedCodecParameters = nil
			case "no ssrc":
				c.ssrcFEC = 0
			}
			d.SetFECProtection(livekit.FECProtection_FEC_MEDIUM)
			d.bindFEC(c)
			if name != "enabled" {
				require.Nil(t, d.fecEncoder.Load())
				return
			}
			old := d.fecEncoder.Load()
			require.NotNil(t, old)
			p := rtp.Header{Version: 2, SSRC: 123, PayloadType: 96}
			var lastSequenceNumber uint16
			for i := range flexfec.MediaPacketsPerGroup {
				p.SequenceNumber++
				repair := old.Encode(&p, []byte{1, 2, 3})
				if i == flexfec.MediaPacketsPerGroup-1 {
					require.Len(t, repair, 1)
					require.EqualValues(t, 456, repair[0].SSRC)
					require.EqualValues(t, 118, repair[0].PayloadType, "use negotiated PT")
					lastSequenceNumber = repair[0].SequenceNumber
				}
			}
			d.bindFEC(c)
			require.NotSame(t, old, d.fecEncoder.Load())
			for range flexfec.MediaPacketsPerGroup {
				p.SequenceNumber++
				require.Empty(t, old.Encode(&p, []byte{1, 2, 3}), "old queued writes cannot generate repair")
				for _, repair := range d.fecEncoder.Load().Encode(&p, []byte{1, 2, 3}) {
					require.Equal(t, lastSequenceNumber+1, repair.SequenceNumber, "reusing an SSRC must preserve repair sequencing")
				}
			}
			d.closeFEC()
			require.Nil(t, d.fecEncoder.Load())
		})
	}
}

func TestDownTrackFECBandwidthReservation(t *testing.T) {
	rates := Bitrates{{100_000, 200_000}, {300_000, 600_000}}
	receiver := &fecTrackReceiver{rates: rates}
	d := &DownTrack{receiver: receiver}
	_, got := d.getLayeredBitrateWithFEC()
	require.Equal(t, rates, got)
	d.SetFECProtection(livekit.FECProtection_FEC_MEDIUM)
	d.fecEncoder.Store(flexfec.NewEncoder(115, 456, nil))
	layers, got := d.getLayeredBitrateWithFEC()
	require.Equal(t, []int32{0, 1}, layers)
	require.EqualValues(t, 125_000, got[0][0])
	require.EqualValues(t, 750_000, got[1][1])
	_, unchanged := receiver.GetLayeredBitrate()
	require.Equal(t, rates, unchanged, "do not mutate shared upstream bitrate estimates")
}

type fecTrackReceiver struct {
	TrackReceiver
	rates Bitrates
}

func (r *fecTrackReceiver) GetLayeredBitrate() ([]int32, Bitrates) { return []int32{0, 1}, r.rates }

func TestDownTrackFECProtectionChanges(t *testing.T) {
	d := &DownTrack{
		params: DownTrackParams{EnableFlexFEC: true}, kind: webrtc.RTPCodecTypeVideo,
		receiver:                  &fecTrackReceiver{rates: Bitrates{{100_000}}},
		negotiatedCodecParameters: []webrtc.RTPCodecParameters{{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeFlexFEC03}, PayloadType: 115}},
	}
	// Cached settings can arrive before Pion binds the track.
	d.SetFECProtection(livekit.FECProtection_FEC_MEDIUM)
	d.bindFEC(fecTrackContext{ssrcFEC: 456})
	encoder := d.fecEncoder.Load()
	require.NotNil(t, encoder)
	_, rates := d.getLayeredBitrateWithFEC()
	require.EqualValues(t, 125_000, rates[0][0])
	p := rtp.Header{Version: 2, SSRC: 123, PayloadType: 96}
	send := func() int {
		count := 0
		for range 20 {
			p.SequenceNumber++
			count += len(d.fecEncoder.Load().Encode(&p, []byte{1, 2, 3}))
		}
		return count
	}
	require.Equal(t, 5, send())
	for _, tc := range []struct {
		level   livekit.FECProtection
		percent int64
	}{
		{livekit.FECProtection_FEC_NONE, 0},
		{livekit.FECProtection_FEC_LOW, 15},
		{livekit.FECProtection_FEC_MEDIUM, 25},
		{livekit.FECProtection_FEC_HIGH, 35},
		{-1, 0},
		{99, 0},
	} {
		d.SetFECProtection(tc.level)
		require.Same(t, encoder, d.fecEncoder.Load(), "updates do not rebind or reset repair sequencing")
		_, rates = d.getLayeredBitrateWithFEC()
		require.EqualValues(t, 100_000+1000*tc.percent, rates[0][0])
		require.EqualValues(t, tc.percent/5, send())
	}
	d.closeFEC()
	d.SetFECProtection(livekit.FECProtection_FEC_HIGH)
	d.bindFEC(fecTrackContext{ssrcFEC: 456})
	require.NotSame(t, encoder, d.fecEncoder.Load())
	require.Equal(t, 7, send(), "rebind retains the subscriber's preset")
}

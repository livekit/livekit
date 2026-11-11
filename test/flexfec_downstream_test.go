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

package test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/sfu/flexfec"
	"github.com/livekit/livekit-server/pkg/testutils"
	testclient "github.com/livekit/livekit-server/test/client"
)

// fecDownstreamServerPort keeps these tests clear of a server running on the
// default ports.
const fecDownstreamServerPort = 17880

func setupFlexFECDownstreamTest(name string) (*service.LivekitServer, func()) {
	logger.Infow("----------------STARTING TEST----------------", "test", name)
	s := createSingleNodeServer(func(conf *config.Config) {
		conf.Port = fecDownstreamServerPort
		conf.RTC.UDPPort = rtcconfig.PortRange{Start: fecDownstreamServerPort + 1}
		conf.RTC.TCPPort = fecDownstreamServerPort + 2
		conf.RTC.FlexFEC = config.FlexFECConfig{
			DownstreamEnabled: true,
		}
	})
	go func() {
		if err := s.Start(); err != nil {
			logger.Errorw("server returned error", err)
		}
	}()

	waitForServerToStart(s)

	return s, func() {
		s.Stop(true)
		logger.Infow("----------------FINISHING TEST----------------", "test", name)
	}
}

// writeFlexFECTestIVF writes a VP8 IVF file whose frames alternate between a
// single RTP packet and several, so repairs cover both one-packet and
// multi-packet blocks.
func writeFlexFECTestIVF(t *testing.T, numFrames int) string {
	var buf bytes.Buffer
	header := make([]byte, 32)
	copy(header[0:4], "DKIF")
	binary.LittleEndian.PutUint16(header[6:8], 32)
	copy(header[8:12], "VP80")
	binary.LittleEndian.PutUint16(header[12:14], 640)
	binary.LittleEndian.PutUint16(header[14:16], 480)
	binary.LittleEndian.PutUint32(header[16:20], 30) // timebase denominator
	binary.LittleEndian.PutUint32(header[20:24], 1)  // timebase numerator
	binary.LittleEndian.PutUint32(header[24:28], uint32(numFrames))
	buf.Write(header)

	for i := 0; i < numFrames; i++ {
		size := 5
		if i%2 == 1 {
			size = 5000
		}
		// a leading zero byte marks a VP8 key frame, the rest is filler
		frame := bytes.Repeat([]byte{0xff}, size)
		frame[0] = 0
		frameHeader := make([]byte, 12)
		binary.LittleEndian.PutUint32(frameHeader[0:4], uint32(size))
		binary.LittleEndian.PutUint64(frameHeader[4:12], uint64(i))
		buf.Write(frameHeader)
		buf.Write(frame)
	}

	path := filepath.Join(t.TempDir(), "flexfec.ivf")
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600))
	return path
}

// fecLossRecorder drops every n-th media packet before the subscriber's
// receive buffer and recovers the dropped packets from the repairs the SFU
// actually sent, using the same FlexFEC-03 decoder as upstream recovery. RTX
// is dropped while loss is simulated, so retransmissions cannot fill the gaps.
type fecLossRecorder struct {
	mu sync.Mutex

	mediaSSRC, fecSSRC, rtxSSRC uint32
	dropEvery                   int

	decoder      *flexfec.Decoder
	received     map[uint16][]byte
	dropped      map[uint16][]byte
	recovered    map[uint16]bool // value reports a bit-exact match
	mediaPackets int
	fecPackets   int
}

func newFECLossRecorder() *fecLossRecorder {
	r := &fecLossRecorder{}
	r.startWindow(0)
	return r
}

func (r *fecLossRecorder) setSSRCs(media, fec, rtx uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mediaSSRC, r.fecSSRC, r.rtxSSRC = media, fec, rtx
	r.resetLocked()
}

// startWindow resets the counters and sets the simulated loss pattern.
func (r *fecLossRecorder) startWindow(dropEvery int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropEvery = dropEvery
	r.resetLocked()
}

func (r *fecLossRecorder) resetLocked() {
	r.received = make(map[uint16][]byte)
	r.dropped = make(map[uint16][]byte)
	r.recovered = make(map[uint16]bool)
	r.mediaPackets = 0
	r.fecPackets = 0
	r.decoder = flexfec.NewDecoder(r.fecSSRC, r.mediaSSRC, func(sn uint16, dst []byte) (int, error) {
		raw, ok := r.received[sn]
		if !ok {
			return 0, errors.New("media packet not received")
		}
		if len(dst) < len(raw) {
			return 0, io.ErrShortBuffer
		}
		return copy(dst, raw), nil
	}, logger.GetLogger())
}

func (r *fecLossRecorder) filter(pkt *rtp.Packet) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.mediaSSRC == 0:
		return true
	case pkt.SSRC == r.fecSSRC:
		r.fecPackets++
		r.decodeLocked(pkt)
		return true
	case pkt.SSRC == r.rtxSSRC:
		return r.dropEvery == 0
	case pkt.SSRC != r.mediaSSRC:
		return true
	}
	if _, ok := r.dropped[pkt.SequenceNumber]; ok {
		// a retransmission on the media SSRC must not undo the loss
		return false
	}
	raw, err := pkt.Marshal()
	if err != nil {
		return true
	}
	r.mediaPackets++
	if r.dropEvery > 0 && r.mediaPackets%r.dropEvery == 0 {
		r.dropped[pkt.SequenceNumber] = raw
		return false
	}
	r.received[pkt.SequenceNumber] = raw
	r.decodeLocked(pkt)
	return true
}

func (r *fecLossRecorder) decodeLocked(pkt *rtp.Packet) {
	for _, rp := range r.decoder.DecodeFEC(pkt.Clone()) {
		original, ok := r.dropped[rp.SequenceNumber]
		if !ok {
			continue
		}
		raw, err := rp.Marshal()
		r.recovered[rp.SequenceNumber] = err == nil && bytes.Equal(raw, original)
		if err == nil {
			// a recovered packet can complete the next repair
			r.received[rp.SequenceNumber] = raw
		}
	}
}

type fecWindowResult struct {
	mediaPackets, fecPackets, dropped, recovered, mismatched int
}

func (r *fecLossRecorder) result() fecWindowResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	res := fecWindowResult{
		mediaPackets: r.mediaPackets,
		fecPackets:   r.fecPackets,
		dropped:      len(r.dropped),
	}
	for _, exact := range r.recovered {
		res.recovered++
		if !exact {
			res.mismatched++
		}
	}
	return res
}

// subscriberVideoSSRCs returns the media, FlexFEC and RTX SSRCs of the first
// video m-section the SFU offered to the subscriber.
func subscriberVideoSSRCs(sd *webrtc.SessionDescription) (media, fec, rtx uint32, err error) {
	if sd == nil {
		return 0, 0, 0, fmt.Errorf("no subscriber description")
	}
	parsed := &sdp.SessionDescription{}
	if err = parsed.Unmarshal([]byte(sd.SDP)); err != nil {
		return 0, 0, 0, err
	}
	for _, md := range parsed.MediaDescriptions {
		if md.MediaName.Media != "video" {
			continue
		}
		for _, attr := range md.Attributes {
			if attr.Key != "ssrc-group" {
				continue
			}
			fields := strings.Fields(attr.Value)
			if len(fields) != 3 {
				continue
			}
			base, err1 := strconv.ParseUint(fields[1], 10, 32)
			other, err2 := strconv.ParseUint(fields[2], 10, 32)
			if err1 != nil || err2 != nil {
				continue
			}
			switch fields[0] {
			case "FEC-FR":
				media, fec = uint32(base), uint32(other)
			case "FID":
				rtx = uint32(other)
			}
		}
		if media != 0 {
			return media, fec, rtx, nil
		}
	}
	return 0, 0, 0, fmt.Errorf("no FEC-FR ssrc-group in subscriber description")
}

func sendFECSetting(t *testing.T, c *testclient.RTCClient, trackSID livekit.TrackID, level livekit.FECProtection) {
	require.NoError(t, c.SendRequest(&livekit.SignalRequest{
		Message: &livekit.SignalRequest_TrackSetting{
			TrackSetting: &livekit.UpdateTrackSettings{
				TrackSids: []string{string(trackSID)},
				Fec:       level.Enum(),
			},
		},
	}))
}

// TestFlexFECDownstreamRecovery publishes a VP8 track through the SFU to a
// FlexFEC-capable subscriber and checks, with simulated downstream loss, that
// the subscriber recovers dropped packets bit-exactly from the SFU's repairs
// once it selects a FEC preset, and that presets change without renegotiation.
func TestFlexFECDownstreamRecovery(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, finish := setupFlexFECDownstreamTest("TestFlexFECDownstreamRecovery")
	defer finish()

	recorder := newFECLossRecorder()
	pub := createRTCClient("fec_down_pub", fecDownstreamServerPort, testRTCServicePathv0, nil)
	sub := createRTCClient("fec_down_sub", fecDownstreamServerPort, testRTCServicePathv0, &testclient.Options{
		AutoSubscribe:       true,
		EnableFlexFEC:       true,
		SubscriberRTPFilter: recorder.filter,
	})
	defer stopClients(pub, sub)
	waitUntilConnected(t, pub, sub)

	writer, err := pub.AddFileTrack(writeFlexFECTestIVF(t, 900), "video", "fecdownvideo")
	require.NoError(t, err)
	defer writer.Stop()

	var trackSID livekit.TrackID
	testutils.WithTimeout(t, func() string {
		tracks := sub.SubscribedTracks()[pub.ID()]
		if len(tracks) == 0 {
			return "subscriber has not subscribed to the publisher's track"
		}
		_, trackSID = rtc.UnpackStreamID(tracks[0].StreamID())
		if trackSID == "" {
			return "subscribed track has no track SID yet"
		}
		return ""
	})

	var offerSDP string
	testutils.WithTimeout(t, func() string {
		answer := sub.SubscriberLocalDescription()
		if answer == nil || !strings.Contains(answer.SDP, "flexfec-03") {
			return "subscriber did not negotiate flexfec-03"
		}
		sd := sub.SubscriberRemoteDescription()
		media, fec, rtx, err := subscriberVideoSSRCs(sd)
		if err != nil {
			return err.Error()
		}
		recorder.setSSRCs(media, fec, rtx)
		offerSDP = sd.SDP
		return ""
	})

	measure := func(dropEvery int, duration time.Duration) fecWindowResult {
		recorder.startWindow(dropEvery)
		time.Sleep(duration)
		res := recorder.result()
		logger.Infow("flexfec downstream window", "dropEvery", dropEvery, "result", fmt.Sprintf("%+v", res))
		return res
	}

	// FEC_NONE is the default: no repairs, and simulated loss is not recovered.
	res := measure(7, 3*time.Second)
	require.Greater(t, res.mediaPackets, 100, "media is not flowing")
	require.Zero(t, res.fecPackets, "repairs were sent before a preset was selected")
	require.NotZero(t, res.dropped)
	require.Zero(t, res.recovered, "dropped packets were recovered without FEC")

	for _, level := range []livekit.FECProtection{
		livekit.FECProtection_FEC_LOW,
		livekit.FECProtection_FEC_MEDIUM,
		livekit.FECProtection_FEC_HIGH,
	} {
		sendFECSetting(t, sub, trackSID, level)
		testutils.WithTimeout(t, func() string {
			if measure(0, 300*time.Millisecond).fecPackets == 0 {
				return fmt.Sprintf("no repairs after selecting %s", level)
			}
			return ""
		})

		res = measure(7, 3*time.Second)
		require.Greater(t, res.fecPackets, 0, level.String())
		require.NotZero(t, res.dropped, level.String())
		require.Zero(t, res.mismatched, "%s: recovered packets differ from the originals", level)
		// isolated single losses are recoverable at every preset, since each
		// frame or block carries at least one repair
		require.GreaterOrEqual(t, res.recovered*100, res.dropped*90,
			"%s: recovered %d of %d dropped packets", level, res.recovered, res.dropped)
	}

	// Switching back to FEC_NONE stops repairs, still without renegotiation.
	sendFECSetting(t, sub, trackSID, livekit.FECProtection_FEC_NONE)
	testutils.WithTimeout(t, func() string {
		if measure(0, 500*time.Millisecond).fecPackets != 0 {
			return "repairs continue after selecting FEC_NONE"
		}
		return ""
	})
	require.Equal(t, offerSDP, sub.SubscriberRemoteDescription().SDP, "FEC preset changes renegotiated")
}

// TestFlexFECDownstreamNotNegotiated checks that a subscriber without FlexFEC
// support receives media but no repairs, even when it selects a preset.
func TestFlexFECDownstreamNotNegotiated(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, finish := setupFlexFECDownstreamTest("TestFlexFECDownstreamNotNegotiated")
	defer finish()

	// counts packets that are neither media nor RTX once the SSRCs are known
	var mu sync.Mutex
	var mediaSSRC, rtxSSRC uint32
	var mediaPackets, unexpected int
	filter := func(pkt *rtp.Packet) bool {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case mediaSSRC == 0:
		case pkt.SSRC == mediaSSRC:
			mediaPackets++
		case pkt.SSRC != rtxSSRC:
			unexpected++
		}
		return true
	}

	pub := createRTCClient("nofec_down_pub", fecDownstreamServerPort, testRTCServicePathv0, nil)
	sub := createRTCClient("nofec_down_sub", fecDownstreamServerPort, testRTCServicePathv0, &testclient.Options{
		AutoSubscribe:       true,
		SubscriberRTPFilter: filter,
	})
	defer stopClients(pub, sub)
	waitUntilConnected(t, pub, sub)

	writer, err := pub.AddStaticTrack("video/vp8", "video", "nofecdownvideo")
	require.NoError(t, err)
	defer writer.Stop()

	var track *webrtc.TrackRemote
	testutils.WithTimeout(t, func() string {
		tracks := sub.SubscribedTracks()[pub.ID()]
		if len(tracks) == 0 || sub.BytesReceived() == 0 {
			return "subscriber has not received media"
		}
		track = tracks[0]
		return ""
	})
	// the SFU offers flexfec-03, the subscriber declines it
	answer := sub.SubscriberLocalDescription()
	require.NotNil(t, answer)
	require.NotContains(t, answer.SDP, "flexfec-03")
	sd := sub.SubscriberRemoteDescription()
	require.NotNil(t, sd)

	mu.Lock()
	mediaSSRC = uint32(track.SSRC())
	rtxSSRC = subscriberRTXSSRC(sd, mediaSSRC)
	mu.Unlock()

	_, trackSID := rtc.UnpackStreamID(track.StreamID())
	sendFECSetting(t, sub, trackSID, livekit.FECProtection_FEC_HIGH)
	time.Sleep(2 * time.Second)

	mu.Lock()
	defer mu.Unlock()
	require.Greater(t, mediaPackets, 50, "media stopped after selecting a preset")
	require.Zero(t, unexpected, "packets on an unexpected SSRC, likely repairs")
}

// subscriberRTXSSRC returns the RTX SSRC paired with media, or 0.
func subscriberRTXSSRC(sd *webrtc.SessionDescription, media uint32) uint32 {
	prefix := fmt.Sprintf("a=ssrc-group:FID %d ", media)
	for _, line := range strings.Split(sd.SDP, "\r\n") {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			rtx, err := strconv.ParseUint(strings.TrimSpace(rest), 10, 32)
			if err == nil {
				return uint32(rtx)
			}
		}
	}
	return 0
}

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

package flexfec

import (
	"fmt"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

func TestProtectionFactorPercentConversion(t *testing.T) {
	for _, tc := range []struct {
		percent uint32
		factor  uint8
	}{
		{0, 0}, {15, 38}, {25, 63}, {35, 89}, {40, 102}, {100, 255}, {101, 255},
	} {
		require.Equal(t, tc.factor, ProtectionFactorFromPercent(tc.percent))
		require.Equal(t, min(tc.percent, MaxProtectionPercent), protectionFactorToPercent(tc.factor))
	}
}

func TestAdaptiveProtectionFactorsMatchLibwebrtc(t *testing.T) {
	// Expected values were computed from libwebrtc's fec_rate_table.h with an
	// independent implementation of VCMNackFecMethod::UpdateParameters.
	for _, tc := range []struct {
		name               string
		loss               uint8
		rttMs              int64
		bitrateKbps        float64
		frameRate          float64
		width, height      int
		packetsPerFrame    float64
		packetsPerFrameKey float64
		delta, key         uint8
	}{
		{"10% loss", 26, 100, 1000, 30, 704, 576, 3, 15, 95, 255},
		{"low rtt is nack only for delta frames", 26, 10, 1000, 30, 704, 576, 3, 15, 0, 255},
		{"bit rate too low", 26, 100, 100, 30, 704, 576, 3, 15, 0, 0},
		{"no loss", 0, 100, 1000, 30, 704, 576, 3, 15, 0, 0},
		{"720p", 13, 50, 2500, 30, 1280, 720, 4, 20, 64, 170},
		{"high rtt keeps fec at low rate", 64, 300, 300, 15, 320, 180, 1, 4, 253, 255},
		{"loss above table range", 200, 100, 1500, 30, 640, 360, 2, 8, 253, 255},
		{"unknown resolution uses the default", 13, 50, 2500, 30, 0, 0, 4, 20, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			delta, key := adaptiveProtectionFactors(adaptiveParams{
				loss:               tc.loss,
				rttMs:              tc.rttMs,
				bitrateKbps:        tc.bitrateKbps,
				frameRate:          tc.frameRate,
				packetsPerFrame:    tc.packetsPerFrame,
				packetsPerFrameKey: tc.packetsPerFrameKey,
				width:              tc.width,
				height:             tc.height,
			})
			if tc.name == "unknown resolution uses the default" {
				expectedDelta, expectedKey := adaptiveProtectionFactors(adaptiveParams{
					loss: tc.loss, rttMs: tc.rttMs, bitrateKbps: tc.bitrateKbps, frameRate: tc.frameRate,
					packetsPerFrame: tc.packetsPerFrame, packetsPerFrameKey: tc.packetsPerFrameKey,
					width: adaptiveDefaultWidth, height: adaptiveDefaultHeight,
				})
				require.Equal(t, expectedDelta, delta)
				require.Equal(t, expectedKey, key)
				return
			}
			require.Equal(t, tc.delta, delta, "delta")
			require.Equal(t, tc.key, key, "key")
		})
	}
}

func TestLossFilterKeepsMaxOverTenSeconds(t *testing.T) {
	f := newLossFilter()
	require.EqualValues(t, 10, f.filteredLoss(0, 10))
	require.EqualValues(t, 50, f.filteredLoss(500, 50), "max within the short window")
	require.EqualValues(t, 50, f.filteredLoss(1000, 0))
	require.EqualValues(t, 50, f.filteredLoss(5000, 0))
	require.EqualValues(t, 50, f.filteredLoss(10_000, 0))
	// The window holding 50 was recorded at 1000ms and expires 10s later.
	require.EqualValues(t, 50, f.filteredLoss(11_000, 0))
	require.EqualValues(t, 0, f.filteredLoss(12_000, 0))
	require.EqualValues(t, 20, f.filteredLoss(13_000, 20))
}

// adaptiveTestSender sends 30 fps frames of 5 packets at the given offset.
type adaptiveTestSender struct {
	e   *Encoder
	now int64
	sn  uint16
	ts  uint32
}

func (s *adaptiveTestSender) sendFrames(t *testing.T, frames int, keyEvery int) (repairs int) {
	t.Helper()
	base := s.now
	defer func() { s.now = base + int64(frames)*int64(time.Second)/30 }()
	for i := range frames {
		s.now = base + int64(i)*int64(time.Second)/30
		key := keyEvery != 0 && i%keyEvery == 0
		packets := 5
		if key {
			packets = 25
		}
		for j := range packets {
			h := rtp.Header{Version: 2, SSRC: 1, PayloadType: 96, SequenceNumber: s.sn, Timestamp: s.ts, Marker: j == packets-1}
			s.sn++
			repairs += len(s.e.encode(s.now, &h, make([]byte, 1000), key && j == 0))
		}
		s.ts += 3000
	}
	return repairs
}

func TestEncoderAdaptiveProtection(t *testing.T) {
	e := NewEncoder(testFECPT, testFECSSRC, nil)
	start := int64(time.Hour)
	e.mu.Lock()
	e.protection = Protection{Mode: ProtectionAdaptive}
	e.adaptive = newAdaptiveState(start)
	e.mu.Unlock()
	s := &adaptiveTestSender{e: e, now: start}

	require.Zero(t, s.sendFrames(t, 30, 0), "no repairs before a receiver report")
	stats := NetworkStats{FractionLost: 26, RTT: 100 * time.Millisecond, Width: 1280, Height: 720}
	e.onReceiverReport(s.now, stats)
	delta, key := e.ProtectionFactors()
	require.NotZero(t, delta)
	require.GreaterOrEqual(t, key, delta)
	require.Equal(t, protectionFactorToPercent(delta), e.OverheadPercent(), "reserve adaptive overhead")
	require.NotZero(t, s.sendFrames(t, 30, 0))

	// Loss subsides: protection is held by the 10 second max filter.
	for range 9 {
		s.sendFrames(t, 30, 0)
		e.onReceiverReport(s.now, NetworkStats{RTT: stats.RTT, Width: stats.Width, Height: stats.Height})
		held, _ := e.ProtectionFactors()
		require.Equal(t, delta, held)
	}
	for range 3 {
		s.sendFrames(t, 30, 0)
		e.onReceiverReport(s.now, NetworkStats{RTT: stats.RTT, Width: stats.Width, Height: stats.Height})
	}
	delta, key = e.ProtectionFactors()
	require.Zero(t, delta)
	require.Zero(t, key)
	require.Zero(t, e.OverheadPercent())
	require.Zero(t, s.sendFrames(t, 30, 0))

	// At low RTT, NACK protects delta frames and FEC only key frames.
	e.onReceiverReport(s.now, NetworkStats{FractionLost: 26, RTT: 5 * time.Millisecond, Width: 1280, Height: 720})
	delta, key = e.ProtectionFactors()
	require.Zero(t, delta)
	require.NotZero(t, key)
	require.NotZero(t, s.sendFrames(t, 30, 10), "key frames are protected")
	require.Zero(t, s.sendFrames(t, 9, 0), "delta frames are not protected")
}

func TestEncoderMinimumProtection(t *testing.T) {
	e := NewEncoder(testFECPT, testFECSSRC, nil)
	floor := ProtectionFactorFromPercent(15)
	e.SetProtection(Protection{Mode: ProtectionMinimum, Factor: floor})
	start := int64(time.Hour)
	e.mu.Lock()
	e.adaptive = newAdaptiveState(start)
	e.mu.Unlock()
	s := &adaptiveTestSender{e: e, now: start}

	delta, key := e.ProtectionFactors()
	require.Equal(t, floor, delta)
	require.Equal(t, floor, key)
	require.EqualValues(t, 15, e.OverheadPercent())
	require.Equal(t, 30, s.sendFrames(t, 30, 0), "one repair for each 5 packet frame at the floor")

	e.onReceiverReport(s.now, NetworkStats{FractionLost: 26, RTT: 100 * time.Millisecond, Width: 1280, Height: 720})
	delta, _ = e.ProtectionFactors()
	require.Greater(t, delta, floor, "rises adaptively with loss")
	require.Greater(t, s.sendFrames(t, 30, 0), 30)

	for range 12 {
		s.sendFrames(t, 30, 0)
		e.onReceiverReport(s.now, NetworkStats{RTT: 100 * time.Millisecond, Width: 1280, Height: 720})
	}
	delta, key = e.ProtectionFactors()
	require.Equal(t, floor, delta, "returns to the floor once loss subsides")
	require.Equal(t, floor, key)
	require.EqualValues(t, 15, e.OverheadPercent())
}

func TestEncoderProtectionModes(t *testing.T) {
	e := NewEncoder(testFECPT, testFECSSRC, nil)
	report := NetworkStats{FractionLost: 26, RTT: 100 * time.Millisecond, Width: 1280, Height: 720}
	for _, tc := range []struct {
		protection Protection
		want       Protection
	}{
		{Protection{Mode: ProtectionFixed, Factor: 63}, Protection{Mode: ProtectionFixed, Factor: 63}},
		{Protection{Mode: ProtectionDisabled, Factor: 63}, Protection{}},
		{Protection{Mode: ProtectionAdaptive, Factor: 63}, Protection{Mode: ProtectionAdaptive}},
		{Protection{Mode: ProtectionMinimum, Factor: 63}, Protection{Mode: ProtectionMinimum, Factor: 63}},
		{Protection{Mode: 99, Factor: 63}, Protection{}},
	} {
		t.Run(fmt.Sprint(tc.protection.Mode), func(t *testing.T) {
			e.SetProtection(tc.protection)
			e.mu.Lock()
			protection, hasAdaptive := e.protection, e.adaptive != nil
			e.mu.Unlock()
			require.Equal(t, tc.want, protection)
			require.Equal(t, tc.want.Mode != ProtectionDisabled, e.Enabled())
			require.True(t, hasAdaptive, "the adaptive state is kept in every mode")
			// Receiver reports only affect adaptive and minimum protection.
			before, _ := e.ProtectionFactors()
			e.OnReceiverReport(report)
			after, _ := e.ProtectionFactors()
			if !tc.want.usesAdaptive() {
				require.Equal(t, before, after)
			}
		})
	}
	e.Close()
	e.OnReceiverReport(report)
}

func TestEncoderAdaptiveStateTrackedInEveryMode(t *testing.T) {
	for _, from := range []Protection{{}, {Mode: ProtectionFixed, Factor: 38}} {
		t.Run(from.Mode.String(), func(t *testing.T) {
			e := NewEncoder(testFECPT, testFECSSRC, nil)
			start := int64(time.Hour)
			e.mu.Lock()
			e.adaptive = newAdaptiveState(start)
			e.mu.Unlock()
			e.SetProtection(from)
			s := &adaptiveTestSender{e: e, now: start}
			report := NetworkStats{FractionLost: 26, RTT: 100 * time.Millisecond, Width: 1280, Height: 720}
			s.sendFrames(t, 30, 0)
			e.onReceiverReport(s.now, report)
			s.sendFrames(t, 30, 0)
			e.onReceiverReport(s.now, NetworkStats{RTT: report.RTT, Width: report.Width, Height: report.Height})
			before, _ := e.ProtectionFactors()
			require.Equal(t, from.Factor, before, "the override applies on top of the tracked adaptive rate")

			// Switching to adaptive starts from the loss history recorded so far,
			// without waiting for another receiver report.
			e.SetProtection(Protection{Mode: ProtectionAdaptive})
			delta, key := e.ProtectionFactors()
			require.NotZero(t, delta)
			require.GreaterOrEqual(t, key, delta)
			require.NotZero(t, s.sendFrames(t, 30, 0))
		})
	}
}

func TestLossFilterKeepsWindowClosingSample(t *testing.T) {
	f := newLossFilter()
	require.EqualValues(t, 10, f.filteredLoss(0, 10))
	require.EqualValues(t, 10, f.filteredLoss(500, 10))
	// the sample closing a window with a nonzero maximum is kept
	require.EqualValues(t, 100, f.filteredLoss(1000, 100))
	require.EqualValues(t, 100, f.filteredLoss(1500, 0))
	require.EqualValues(t, 100, f.filteredLoss(2000, 0))
}

func TestEncoderKeyOnlyOverheadUsesAllMedia(t *testing.T) {
	e := NewEncoder(testFECPT, testFECSSRC, nil)
	start := int64(time.Hour)
	e.mu.Lock()
	e.protection = Protection{Mode: ProtectionAdaptive}
	e.adaptive = newAdaptiveState(start)
	e.mu.Unlock()
	s := &adaptiveTestSender{e: e, now: start}
	s.sendFrames(t, 30, 0)

	// at low RTT only key frames are protected
	e.onReceiverReport(s.now, NetworkStats{FractionLost: 26, RTT: 5 * time.Millisecond, Width: 1280, Height: 720})
	delta, key := e.ProtectionFactors()
	require.Zero(t, delta)
	require.NotZero(t, key)

	// 29 unprotected delta frames of 5 packets, then a protected key frame of 25
	require.Zero(t, s.sendFrames(t, 29, 0))
	repairs := s.sendFrames(t, 1, 1)
	require.NotZero(t, repairs)

	const mediaPacketBytes = 12 + 1000
	const repairBytes = 1100
	mediaBytes := uint64((29*5 + 25) * mediaPacketBytes)
	e.RecordSent(repairs, repairs*1000, repairs*repairBytes)

	expected := uint32((uint64(repairs*repairBytes)*100 + mediaBytes - 1) / mediaBytes)
	require.Equal(t, expected, e.OverheadPercent(), "repairs relative to all media, not only the key frame")
	require.Less(t, e.OverheadPercent(), uint32(50))
}

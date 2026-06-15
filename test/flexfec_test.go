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
	"fmt"
	"strings"
	"testing"

	prom "github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/testutils"
	testclient "github.com/livekit/livekit-server/test/client"
	"github.com/livekit/protocol/logger"
)

func fecCounterValue(t *testing.T, direction string, typ string) float64 {
	t.Helper()
	families, err := prom.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != "livekit_fec_packets" {
			continue
		}
		for _, metric := range family.GetMetric() {
			matchedDirection, matchedType := false, false
			for _, label := range metric.GetLabel() {
				if label.GetName() == "direction" && label.GetValue() == direction {
					matchedDirection = true
				}
				if label.GetName() == "type" && label.GetValue() == typ {
					matchedType = true
				}
			}
			if matchedDirection && matchedType {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func setupFlexFECTest(name string) (*service.LivekitServer, func()) {
	logger.Infow("----------------STARTING TEST----------------", "test", name)
	s := createSingleNodeServer(func(conf *config.Config) {
		conf.RTC.FlexFEC = config.FlexFECConfig{
			UpstreamEnabled:   true,
			DownstreamEnabled: true,
			PayloadType:       115,
			NumMediaPackets:   5,
			NumFECPackets:     2,
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

// TestFlexFEC verifies flexfec-03 negotiation on both legs and that the SFU
// generates FEC toward subscribers: a publisher with FlexFEC enabled offers
// flexfec + FEC-FR which the SFU accepts (upstream), and the SFU's offer to
// a FlexFEC capable subscriber carries a FEC-FR repair stream that the
// DownTrack populates with repair packets (downstream).
func TestFlexFEC(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, finish := setupFlexFECTest("TestFlexFEC")
	defer finish()

	fecSentBefore := fecCounterValue(t, "outgoing", "sent")

	opts := &testclient.Options{AutoSubscribe: true, EnableFlexFEC: true}
	c1 := createRTCClient("fec_pub", defaultServerPort, testRTCServicePathv0, opts)
	c2 := createRTCClient("fec_sub", defaultServerPort, testRTCServicePathv0, opts)
	defer stopClients(c1, c2)
	waitUntilConnected(t, c1, c2)

	writer, err := c1.AddStaticTrack("video/vp8", "video", "fecvideo")
	require.NoError(t, err)
	defer writer.Stop()

	// publisher leg: the SFU's answer must accept the offered flexfec-03
	testutils.WithTimeout(t, func() string {
		sd := c1.LastAnswer()
		if sd == nil {
			return "no answer received on publisher connection"
		}
		if !strings.Contains(sd.SDP, "flexfec-03") {
			return "SFU answer does not contain flexfec-03"
		}
		return ""
	})

	// subscriber leg: the SFU's offer must announce the FEC repair stream
	// and the DownTrack must generate FEC packets for the forwarded media
	testutils.WithTimeout(t, func() string {
		tracks := c2.SubscribedTracks()
		if len(tracks[c1.ID()]) == 0 {
			return "c2 was not subscribed to c1's tracks"
		}

		sd := c2.LastSubscriberOffer()
		if sd == nil {
			return "no offer received on subscriber connection"
		}
		if !strings.Contains(sd.SDP, "flexfec-03") {
			return "SFU offer does not contain flexfec-03"
		}
		if !strings.Contains(sd.SDP, "FEC-FR") {
			return "SFU offer does not contain a FEC-FR ssrc-group"
		}

		fecSent := fecCounterValue(t, "outgoing", "sent")
		if fecSent <= fecSentBefore {
			return fmt.Sprintf("no FEC packets generated, counter at %f", fecSent)
		}
		return ""
	})
}

// TestFlexFECDisabledClient ensures media still flows when the server has
// FlexFEC enabled but a client does not negotiate it.
func TestFlexFECDisabledClient(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, finish := setupFlexFECTest("TestFlexFECDisabledClient")
	defer finish()

	c1 := createRTCClient("nofec_pub", defaultServerPort, testRTCServicePathv0, nil)
	c2 := createRTCClient("nofec_sub", defaultServerPort, testRTCServicePathv0, nil)
	defer stopClients(c1, c2)
	waitUntilConnected(t, c1, c2)

	writer, err := c1.AddStaticTrack("video/vp8", "video", "plainvideo")
	require.NoError(t, err)
	defer writer.Stop()

	testutils.WithTimeout(t, func() string {
		tracks := c2.SubscribedTracks()
		if len(tracks[c1.ID()]) == 0 {
			return "c2 was not subscribed to c1's tracks"
		}
		if c2.BytesReceived() == 0 {
			return "c2 did not receive any media"
		}
		return ""
	})
}

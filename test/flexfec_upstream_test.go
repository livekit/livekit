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
	"strings"
	"testing"

	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/testutils"
	testclient "github.com/livekit/livekit-server/test/client"
	"github.com/livekit/protocol/logger"
)

func setupFlexFECUpstreamTest(name string) (*service.LivekitServer, func()) {
	logger.Infow("----------------STARTING TEST----------------", "test", name)
	s := createSingleNodeServer(func(conf *config.Config) {
		conf.RTC.FlexFEC = config.FlexFECConfig{
			UpstreamEnabled: true,
			PayloadType:     115,
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

// TestFlexFECUpstreamNegotiation verifies that the SFU accepts flexfec-03
// offered by a publisher when upstream recovery is enabled.
func TestFlexFECUpstreamNegotiation(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, finish := setupFlexFECUpstreamTest("TestFlexFECUpstreamNegotiation")
	defer finish()

	opts := &testclient.Options{AutoSubscribe: true, EnableFlexFEC: true}
	c1 := createRTCClient("fec_pub", defaultServerPort, testRTCServicePathv0, opts)
	defer stopClients(c1)
	waitUntilConnected(t, c1)

	writer, err := c1.AddStaticTrack("video/vp8", "video", "fecvideo")
	require.NoError(t, err)
	defer writer.Stop()

	testutils.WithTimeout(t, func() string {
		sd := c1.LastAnswer()
		if sd == nil {
			return "no answer received on publisher connection"
		}
		if !strings.Contains(sd.SDP, "flexfec-03") {
			return "SFU answer does not contain flexfec-03"
		}
		if !strings.Contains(sd.SDP, "repair-window=2000000") {
			return "SFU answer does not contain the configured FlexFEC repair window"
		}
		return ""
	})
}

// TestFlexFECUpstreamWithE2EE verifies that FlexFEC negotiation and media
// forwarding remain enabled when the published track is marked as GCM E2EE.
// The SFU must treat the media payload as opaque in this mode.
func TestFlexFECUpstreamWithE2EE(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, finish := setupFlexFECUpstreamTest("TestFlexFECUpstreamWithE2EE")
	defer finish()

	pubOpts := &testclient.Options{
		AutoSubscribe: true,
		EnableFlexFEC: true,
		SignalRequestInterceptor: func(msg *livekit.SignalRequest, next testclient.SignalRequestHandler) error {
			if req := msg.GetAddTrack(); req != nil {
				req.Encryption = livekit.Encryption_GCM
			}
			return next(msg)
		},
	}
	c1 := createRTCClient("fec_e2ee_pub", defaultServerPort, testRTCServicePathv0, pubOpts)
	c2 := createRTCClient("fec_e2ee_sub", defaultServerPort, testRTCServicePathv0, nil)
	defer stopClients(c1, c2)
	waitUntilConnected(t, c1, c2)

	writer, err := c1.AddStaticTrack("video/vp8", "video", "fec-e2ee-video")
	require.NoError(t, err)
	defer writer.Stop()

	testutils.WithTimeout(t, func() string {
		sd := c1.LastAnswer()
		if sd == nil || !strings.Contains(sd.SDP, "flexfec-03") {
			return "publisher did not negotiate flexfec-03"
		}

		remote := c2.GetRemoteParticipant(c1.ID())
		if remote == nil || len(remote.Tracks) == 0 {
			return "subscriber has not received the publisher's track metadata"
		}
		if remote.Tracks[0].Encryption != livekit.Encryption_GCM {
			return "subscriber does not see the track as GCM-encrypted"
		}
		if len(c2.SubscribedTracks()[c1.ID()]) == 0 || c2.BytesReceived() == 0 {
			return "subscriber has not received media for the encrypted track"
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

	_, finish := setupFlexFECUpstreamTest("TestFlexFECDisabledClient")
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

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

// Package vnettest sets up real pion peer connections on an in-memory virtual
// network, for integration tests that exercise media paths without a server.
//
// It depends only on pion, so it can be imported both by tests inside pkg/... and by
// the top level test package.
package vnettest

import (
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/logging"
	"github.com/pion/sdp/v3"
	"github.com/pion/transport/v4/packetio"
	"github.com/pion/transport/v4/vnet"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"
)

const (
	VP8PayloadType  = 96
	RTXPayloadType  = VP8PayloadType + 1
	OpusPayloadType = 111
)

// Hosts are the two ends of a started virtual network.
type Hosts struct {
	OfferNet  *vnet.Net
	AnswerNet *vnet.Net
}

// NewHosts returns two hosts on a started virtual network, torn down with the test.
func NewHosts(t *testing.T) *Hosts {
	t.Helper()

	wan, err := vnet.NewRouter(&vnet.RouterConfig{
		CIDR:          "1.2.3.0/24",
		LoggerFactory: logging.NewDefaultLoggerFactory(),
	})
	require.NoError(t, err)

	offerNet, err := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{"1.2.3.4"}})
	require.NoError(t, err)
	require.NoError(t, wan.AddNet(offerNet))

	answerNet, err := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{"1.2.3.5"}})
	require.NoError(t, err)
	require.NoError(t, wan.AddNet(answerNet))

	require.NoError(t, wan.Start())
	t.Cleanup(func() { _ = wan.Stop() })

	return &Hosts{OfferNet: offerNet, AnswerNet: answerNet}
}

// NewSettingEngine returns a setting engine bound to net, with ICE timeouts short
// enough to keep tests quick.
func NewSettingEngine(net *vnet.Net) webrtc.SettingEngine {
	se := webrtc.SettingEngine{}
	se.SetNet(net)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetICETimeouts(5*time.Second, 5*time.Second, 500*time.Millisecond)
	return se
}

// MediaEngineConfig describes what to register on a media engine.
type MediaEngineConfig struct {
	Video               bool // VP8 and its RTX codec; otherwise opus
	HeaderExtensions    bool // abs-send-time + transport-cc
	SimulcastExtensions bool // mid + rid + rsid
}

func VideoRTCPFeedback() []webrtc.RTCPFeedback {
	return []webrtc.RTCPFeedback{
		{Type: webrtc.TypeRTCPFBNACK},
		{Type: webrtc.TypeRTCPFBNACK, Parameter: "pli"},
		{Type: webrtc.TypeRTCPFBTransportCC},
		{Type: webrtc.TypeRTCPFBGoogREMB},
	}
}

func NewMediaEngine(t *testing.T, cfg MediaEngineConfig) *webrtc.MediaEngine {
	t.Helper()

	me := &webrtc.MediaEngine{}
	kind := webrtc.RTPCodecTypeAudio
	if cfg.Video {
		kind = webrtc.RTPCodecTypeVideo

		require.NoError(t, me.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeVP8, ClockRate: 90000, RTCPFeedback: VideoRTCPFeedback(),
			},
			PayloadType: VP8PayloadType,
		}, kind))
		require.NoError(t, me.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:    webrtc.MimeTypeRTX,
				ClockRate:   90000,
				SDPFmtpLine: fmt.Sprintf("apt=%d", VP8PayloadType),
			},
			PayloadType: RTXPayloadType,
		}, kind))
	} else {
		require.NoError(t, me.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
			},
			PayloadType: OpusPayloadType,
		}, kind))
	}

	if cfg.HeaderExtensions {
		require.NoError(t, me.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: sdp.ABSSendTimeURI}, kind))
		require.NoError(t, me.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: sdp.TransportCCURI}, kind))
	}
	if cfg.SimulcastExtensions {
		require.NoError(t, webrtc.ConfigureSimulcastExtensionHeaders(me))
	}

	return me
}

// PCConfig describes a peer connection on the virtual network.
type PCConfig struct {
	Net         *vnet.Net
	MediaEngine MediaEngineConfig

	// BufferFactory is SettingEngine.BufferFactory, e. g. buffer.Factory.GetOrNew.
	// Optional.
	BufferFactory func(packetType packetio.BufferPacketType, ssrc uint32) io.ReadWriteCloser
}

// NewPeerConnection builds a peer connection on the virtual network with no
// interceptors, so nothing rewrites what a test puts on the wire.
func NewPeerConnection(t *testing.T, cfg PCConfig) *webrtc.PeerConnection {
	t.Helper()

	se := NewSettingEngine(cfg.Net)
	se.BufferFactory = cfg.BufferFactory

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(NewMediaEngine(t, cfg.MediaEngine)),
		webrtc.WithSettingEngine(se),
		webrtc.WithInterceptorRegistry(&interceptor.Registry{}),
	)

	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = pc.Close() })

	return pc
}

// GatheredOffer creates an offer and waits for gathering, so the SDP carries every
// candidate and the caller needs no trickle.
func GatheredOffer(t *testing.T, pc *webrtc.PeerConnection) webrtc.SessionDescription {
	t.Helper()

	offer, err := pc.CreateOffer(nil)
	require.NoError(t, err)

	gathered := webrtc.GatheringCompletePromise(pc)
	require.NoError(t, pc.SetLocalDescription(offer))
	<-gathered

	return *pc.LocalDescription()
}

// SignalPair performs a full offer/answer exchange between two peer connections and
// waits for both to connect.
func SignalPair(t *testing.T, offerer, answerer *webrtc.PeerConnection) {
	t.Helper()

	connected := UntilConnected(offerer, answerer)

	require.NoError(t, answerer.SetRemoteDescription(GatheredOffer(t, offerer)))

	answer, err := answerer.CreateAnswer(nil)
	require.NoError(t, err)
	gathered := webrtc.GatheringCompletePromise(answerer)
	require.NoError(t, answerer.SetLocalDescription(answer))
	<-gathered

	require.NoError(t, offerer.SetRemoteDescription(*answerer.LocalDescription()))

	select {
	case <-connected:
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for peer connections to connect")
	}
}

// UntilConnected closes the returned channel once every peer connection is connected.
func UntilConnected(pcs ...*webrtc.PeerConnection) <-chan struct{} {
	var wg sync.WaitGroup
	wg.Add(len(pcs))
	for _, pc := range pcs {
		var once sync.Once
		pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
			if s == webrtc.PeerConnectionStateConnected {
				once.Do(wg.Done)
			}
		})
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	return done
}

// Copyright 2026 LiveKit, Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/moq"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/protocol/livekit"
)

func TestMoQAdmissionBeforeRoomLookup(t *testing.T) {
	// No store or router is installed. Neither a fresh join nor a resume should
	// reach room creation or duplicate-identity eviction on a disabled node.
	r := &RoomManager{config: &config.Config{MoQ: config.MoQConfig{Endpoint: "moqt://node:443/moq"}}}
	pi := routing.ParticipantInit{Identity: "existing", CreateRoom: &livekit.CreateRoomRequest{Name: "test"}, UseMoQTransport: true}
	for _, reconnect := range []bool{false, true} {
		pi.Reconnect = reconnect
		require.ErrorIs(t, r.StartSession(t.Context(), pi, nil, nil, false), ErrMoQUnavailable)
	}

	r.config.MoQ.Enabled = true
	require.ErrorContains(t, r.StartSession(t.Context(), pi, nil, nil, true), "WebSocket")
	pi.PublisherOffer = &livekit.SessionDescription{}
	require.ErrorContains(t, r.StartSession(t.Context(), pi, nil, nil, false), "WebRTC offer")
}

func TestRoomNodeQUICProtocols(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
	}{{"disabled", false}, {"enabled", true}} {
		t.Run(tc.name, func(t *testing.T) {
			conf := &config.Config{Development: true, MoQ: config.MoQConfig{Enabled: tc.enabled, Endpoint: "moqt://localhost:443/moq"}}
			server, err := newRoomNodeQUICServer(conf, &RoomManager{})
			require.NoError(t, err)
			server.H3.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
			addrs, err := server.Listen([]string{"127.0.0.1"}, 0)
			require.NoError(t, err)
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				require.NoError(t, server.Shutdown(ctx))
			})
			addr := addrs[0].String()
			cert, err := x509.ParseCertificate(server.H3.TLSConfig.Certificates[0].Certificate[0])
			require.NoError(t, err)
			roots := x509.NewCertPool()
			roots.AddCert(cert)
			tlsConf := &tls.Config{RootCAs: roots, NextProtos: []string{moq.ALPN}}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			conn, err := quic.DialAddr(ctx, addr, tlsConf, nil)
			if tc.enabled {
				require.NoError(t, err)
				require.Equal(t, moq.ALPN, conn.ConnectionState().TLS.NegotiatedProtocol)
				require.NoError(t, conn.CloseWithError(0, "done"))
			} else {
				require.ErrorContains(t, err, "application protocol")
			}

			transport := &http3.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}
			defer transport.Close()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+addr+"/", nil)
			require.NoError(t, err)
			res, err := transport.RoundTrip(req)
			require.NoError(t, err)
			defer res.Body.Close()
			require.Equal(t, http.StatusNoContent, res.StatusCode)
		})
	}
}

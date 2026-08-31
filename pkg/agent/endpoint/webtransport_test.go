// Copyright 2026 LiveKit, Inc.

package endpoint_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/agent/endpoint"
	"github.com/livekit/livekit-server/pkg/agent/endpoint/conformance"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

func selfSignedTLS(t *testing.T) *tls.Config {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		NextProtos:   []string{http3.NextProtoH3},
	}
}

// startWTServer runs a WebTransport /agent server that registers each session's
// worker (read off the control stream) into reg and keeps the session alive.
func startWTServer(t *testing.T, reg *endpoint.Registry) string {
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	require.NoError(t, err)

	mux := http.NewServeMux()
	srv := &webtransport.Server{H3: &http3.Server{TLSConfig: selfSignedTLS(t), Handler: mux}}
	mux.HandleFunc("/agent", func(w http.ResponseWriter, r *http.Request) {
		sess, err := srv.Upgrade(w, r)
		if err != nil {
			return
		}
		go handleSession(reg, sess)
	})
	go func() { _ = srv.Serve(udp) }()
	t.Cleanup(func() { _ = srv.Close(); _ = udp.Close() })
	return "https://" + udp.LocalAddr().String() + "/agent"
}

func handleSession(reg *endpoint.Registry, sess *webtransport.Session) {
	ctx := sess.Context()
	control, err := sess.AcceptStream(ctx)
	if err != nil {
		return
	}
	var msg livekit.WorkerMessage
	if err := endpoint.ReadControlMessage(control, &msg); err != nil {
		return
	}
	rw := msg.GetRegister()
	if rw == nil {
		return
	}
	manifest, err := endpoint.ParseManifest(rw.GetEndpoints())
	if err != nil {
		return
	}
	registration := &endpoint.Registration{
		WorkerID:   rw.GetInstanceId(),
		APIKey:     "test",
		AgentName:  rw.GetAgentName(),
		Deployment: rw.GetDeployment(),
		Manifest:   manifest,
		Endpoints:  rw.GetEndpoints(),
	}
	registration.SetSession(endpoint.NewWebTransportSession(sess, endpoint.DefaultMaxStreams))
	_ = reg.Register(registration)
	_ = endpoint.WriteControlMessage(control, &livekit.ServerMessage{
		Message: &livekit.ServerMessage_Register{
			Register: &livekit.RegisterWorkerResponse{WorkerId: registration.WorkerID},
		},
	})
	// keep the session alive; drain further control messages until it dies
	for {
		var m livekit.WorkerMessage
		if err := endpoint.ReadControlMessage(control, &m); err != nil {
			return
		}
	}
}

func TestWebTransportEndpointRoundTrip(t *testing.T) {
	// the worker's local app
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hello":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true}`)
		case "/echo":
			body, _ := io.ReadAll(r.Body)
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer target.Close()

	reg := endpoint.NewRegistry()
	base := startWTServer(t, reg)

	front := endpoint.NewFront(reg, func(*http.Request) (string, bool) { return "", false }, logger.GetLogger()).
		WithSingleKeyFallback()
	ts := httptest.NewServer(front)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	w := conformance.New(conformance.Config{
		ServerURL:  base,
		APIKey:     "APIkey",
		APISecret:  "secret-that-is-long-enough-to-sign",
		AgentName:  "myagent",
		Deployment: "production",
		TargetAddr: strings.TrimPrefix(target.URL, "http://"),
		Insecure:   true,
		Endpoints: []*livekit.AgentHttp_AgentEndpoint{
			{Path: "/hello", Methods: []string{"GET"}, Public: true},
			{Path: "/echo", Methods: []string{"POST"}, Public: true},
		},
	})
	require.NoError(t, w.Start(ctx))
	t.Cleanup(w.Close)
	require.NoError(t, w.WaitRegistered(ctx))

	agentBase := ts.URL + "/agents/myagent/production"

	t.Run("GET json", func(t *testing.T) {
		resp, err := http.Get(agentBase + "/hello")
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		body, _ := io.ReadAll(resp.Body)
		require.JSONEq(t, `{"ok":true}`, string(body))
	})

	t.Run("POST echo", func(t *testing.T) {
		payload := strings.Repeat("x", 64<<10)
		resp, err := http.Post(agentBase+"/echo", "application/octet-stream", strings.NewReader(payload))
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		body, _ := io.ReadAll(resp.Body)
		require.Equal(t, payload, string(body))
	})

	t.Run("unknown path 404", func(t *testing.T) {
		resp, err := http.Get(agentBase + "/nope")
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}

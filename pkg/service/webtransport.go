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

package service

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
	"github.com/urfave/negroni/v3"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/logger"
)

// WebTransportTLS builds the listener's TLS config from cert files, or a
// generated self-signed cert in dev mode.
func WebTransportTLS(certFile, keyFile string, dev bool) (*tls.Config, error) {
	if certFile != "" && keyFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, err
		}
		return &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{http3.NextProtoH3}}, nil
	}
	if !dev {
		return nil, errors.New("webtransport requires webtransport.tls_cert_file/tls_key_file")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "livekit-webtransport-dev"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	logger.Warnw("webtransport using a generated self-signed cert (dev mode); clients must skip verification", nil)
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		NextProtos:   []string{http3.NextProtoH3},
	}, nil
}

// WebTransportServer's embedded Close releases sessions but not the sockets;
// Shutdown is the teardown path.
type WebTransportServer struct {
	*webtransport.Server

	mu      sync.Mutex
	conns   []*net.UDPConn
	lns     []*quic.EarlyListener
	cancel  context.CancelFunc
	serving sync.WaitGroup
}

// NewWebTransportServer wraps an HTTP/3 WebTransport server around tlsConf (the
// h3 ALPN is set here if absent). The caller must assign wt.H3.Handler.
func NewWebTransportServer(tlsConf *tls.Config) *WebTransportServer {
	tlsConf = tlsConf.Clone()
	if len(tlsConf.NextProtos) == 0 {
		tlsConf.NextProtos = []string{http3.NextProtoH3}
	}
	return &WebTransportServer{Server: &webtransport.Server{H3: &http3.Server{TLSConfig: tlsConf}}}
}

// NewWebTransportHandler builds the listener's handler: mux behind api-key auth,
// with wt in each request's context.
func NewWebTransportHandler(keyProvider auth.KeyProvider, wt *WebTransportServer, mux http.Handler) http.Handler {
	middlewares := []negroni.Handler{negroni.NewRecovery()}
	if keyProvider != nil {
		middlewares = append(middlewares, NewAPIKeyAuthMiddleware(keyProvider))
	}
	return WithWebTransportServer(wt, WithPathNormalization(configureMiddlewares(mux, middlewares...)))
}

type webTransportServerKey struct{}

// WithWebTransportServer puts wt in each request's context, where a route that
// upgrades reads it. Apply it outermost, ahead of the listener's middleware chain.
func WithWebTransportServer(wt *WebTransportServer, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), webTransportServerKey{}, wt)))
	})
}

// GetWebTransportServer returns the server serving this request, or nil when the
// request did not arrive over a WebTransport listener.
func GetWebTransportServer(ctx context.Context) *WebTransportServer {
	wt, _ := ctx.Value(webTransportServerKey{}).(*WebTransportServer)
	return wt
}

// UpgradeWebTransport upgrades a request on the server carried in its context.
func UpgradeWebTransport(w http.ResponseWriter, r *http.Request) (*webtransport.Session, error) {
	wt := GetWebTransportServer(r.Context())
	if wt == nil {
		return nil, errors.New("no webtransport server serving this request")
	}
	return wt.Upgrade(unwrapResponseWriter(w), r)
}

// unwrapResponseWriter reaches the ResponseWriter net/http handed in. An upgrade
// type-asserts it to http3.Settingser and http3.HTTPStreamer without checking, so
// it must not be a middleware's wrapper.
func unwrapResponseWriter(w http.ResponseWriter) http.ResponseWriter {
	for {
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return w
		}
		w = u.Unwrap()
	}
}

// Listen binds one UDP socket per address. Empty addrs binds all interfaces.
func (s *WebTransportServer) Listen(addrs []string, port uint32) ([]net.Addr, error) {
	if len(addrs) == 0 {
		addrs = []string{""}
	}

	// Server.Serve takes a reference on the WaitGroup Server.Close waits on;
	// Server.ServeQUICConn does not.
	quicConf := &quic.Config{}
	if s.H3.QUICConfig != nil {
		quicConf = s.H3.QUICConfig.Clone()
	}
	quicConf.EnableDatagrams = true
	quicConf.EnableStreamResetPartialDelivery = true

	var (
		conns []*net.UDPConn
		lns   []*quic.EarlyListener
		bound []net.Addr
	)
	closeAll := func() {
		for _, ln := range lns {
			_ = ln.Close()
		}
		for _, c := range conns {
			_ = c.Close()
		}
	}
	for _, addr := range addrs {
		udpAddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(addr, strconv.Itoa(int(port))))
		if err != nil {
			closeAll()
			return nil, err
		}
		udp, err := net.ListenUDP("udp", udpAddr)
		if err != nil {
			closeAll()
			return nil, err
		}
		conns = append(conns, udp)
		bound = append(bound, udp.LocalAddr())

		ln, err := quic.ListenEarly(udp, s.H3.TLSConfig, quicConf)
		if err != nil {
			closeAll()
			return nil, err
		}
		lns = append(lns, ln)
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.conns, s.lns, s.cancel = conns, lns, cancel
	s.mu.Unlock()

	for _, ln := range lns {
		s.serving.Go(func() { s.accept(ctx, ln) })
	}
	logger.Infow("webtransport listener started", "addresses", bound)

	return bound, nil
}

// Shutdown is safe on a server that never listened, and safe to call more than
// once. Peers get a GOAWAY, then whatever has not drained by the ctx deadline is
// closed under it.
func (s *WebTransportServer) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	cancel, conns, lns := s.cancel, s.conns, s.lns
	s.cancel, s.conns, s.lns = nil, nil, nil
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	err := s.H3.Shutdown(ctx)
	_ = s.Server.Close()
	s.serving.Wait()

	// the sockets close last, so every CONNECTION_CLOSE frame still reaches its peer
	for _, ln := range lns {
		_ = ln.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
	return err
}

func (s *WebTransportServer) accept(ctx context.Context, ln *quic.EarlyListener) {
	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			logger.Infow("webtransport listener stopped", "error", err)
			return
		}
		s.serving.Go(func() {
			if err := s.ServeQUICConn(conn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Infow("webtransport connection stopped", "error", err)
			}
		})
	}
}

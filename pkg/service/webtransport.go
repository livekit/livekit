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
	"time"

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

// NewWebTransportServer wraps an HTTP/3 WebTransport server around tlsConf (the
// h3 ALPN is set here if absent). The caller must assign wt.H3.Handler.
func NewWebTransportServer(tlsConf *tls.Config) *webtransport.Server {
	tlsConf = tlsConf.Clone()
	if len(tlsConf.NextProtos) == 0 {
		tlsConf.NextProtos = []string{http3.NextProtoH3}
	}
	return &webtransport.Server{H3: &http3.Server{TLSConfig: tlsConf}}
}

// NewWebTransportHandler builds the listener's handler: mux behind api-key auth,
// with wt in each request's context.
func NewWebTransportHandler(keyProvider auth.KeyProvider, wt *webtransport.Server, mux http.Handler) http.Handler {
	middlewares := []negroni.Handler{negroni.NewRecovery()}
	if keyProvider != nil {
		middlewares = append(middlewares, NewAPIKeyAuthMiddleware(keyProvider))
	}
	return WithWebTransportServer(wt, WithPathNormalization(configureMiddlewares(mux, middlewares...)))
}

type webTransportServerKey struct{}

// WithWebTransportServer puts wt in each request's context, where a route that
// upgrades reads it. Apply it outermost, ahead of the listener's middleware chain.
func WithWebTransportServer(wt *webtransport.Server, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), webTransportServerKey{}, wt)))
	})
}

// GetWebTransportServer returns the server serving this request, or nil when the
// request did not arrive over a WebTransport listener.
func GetWebTransportServer(ctx context.Context) *webtransport.Server {
	wt, _ := ctx.Value(webTransportServerKey{}).(*webtransport.Server)
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

// ListenWebTransport binds a UDP socket per address and serves wt on each,
// returning the bound addresses and a stop func. Empty addrs binds all interfaces.
func ListenWebTransport(wt *webtransport.Server, addrs []string, port uint32) ([]net.Addr, func(), error) {
	if len(addrs) == 0 {
		addrs = []string{""}
	}

	conns := make([]*net.UDPConn, 0, len(addrs))
	bound := make([]net.Addr, 0, len(addrs))
	closeAll := func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}
	for _, addr := range addrs {
		udpAddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(addr, strconv.Itoa(int(port))))
		if err != nil {
			closeAll()
			return nil, nil, err
		}
		udp, err := net.ListenUDP("udp", udpAddr)
		if err != nil {
			closeAll()
			return nil, nil, err
		}
		conns = append(conns, udp)
		bound = append(bound, udp.LocalAddr())
	}

	for _, udp := range conns {
		go func() {
			if err := wt.Serve(udp); err != nil {
				logger.Infow("webtransport listener stopped", "error", err)
			}
		}()
	}
	logger.Infow("webtransport listener started", "addresses", bound)

	return bound, func() {
		_ = wt.Close()
		closeAll()
	}, nil
}

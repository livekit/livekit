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
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	webtransport "github.com/quic-go/webtransport-go"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/rtc/types"
)

type MoQService struct {
	config      config.MoQConfig
	development bool
	keyProvider auth.KeyProvider
	roomManager *RoomManager
	tracks      *moqTrackRegistry
	logger      logger.Logger

	lock    sync.Mutex
	servers []*webtransport.Server
	wg      sync.WaitGroup
	stopped atomic.Bool
}

type moqResolvedTrack struct {
	room        *rtc.Room
	participant types.Participant
	track       types.MediaTrack
	tap         *moqTrackTap
}

func NewMoQService(conf *config.Config, keyProvider auth.KeyProvider, roomManager *RoomManager) (*MoQService, error) {
	if keyProvider == nil {
		return nil, errors.New("moq requires an API key provider")
	}
	if conf.MoQ.Port == 0 {
		return nil, errors.New("moq.port must be configured")
	}

	lgr := logger.GetLogger().WithComponent("moq")
	s := &MoQService{
		config:      conf.MoQ,
		development: conf.Development,
		keyProvider: keyProvider,
		roomManager: roomManager,
		logger:      lgr,
	}
	s.tracks = newMoQTrackRegistry(moqTrackRegistryParams{
		Config: s.config,
		Logger: lgr,
	})
	roomManager.AddOnRoomCreated(s.tracks.AttachRoom)
	return s, nil
}

func (s *MoQService) Start() error {
	tlsConf, err := s.tlsConfig()
	if err != nil {
		return err
	}

	type listenTarget struct {
		server *webtransport.Server
		conn   *net.UDPConn
		addr   string
	}
	targets := make([]listenTarget, 0, len(s.config.BindAddresses))
	for _, addr := range s.config.BindAddresses {
		bindAddr := net.JoinHostPort(addr, strconv.Itoa(int(s.config.Port)))
		udpAddr, err := net.ResolveUDPAddr("udp", bindAddr)
		if err != nil {
			for _, target := range targets {
				_ = target.conn.Close()
			}
			return err
		}
		conn, err := net.ListenUDP("udp", udpAddr)
		if err != nil {
			for _, target := range targets {
				_ = target.conn.Close()
			}
			return err
		}

		h3Server := &http3.Server{
			Addr:      bindAddr,
			TLSConfig: http3.ConfigureTLSConfig(tlsConf.Clone()),
			QUICConfig: &quic.Config{
				EnableDatagrams:                  true,
				EnableStreamResetPartialDelivery: true,
			},
		}
		webtransport.ConfigureHTTP3Server(h3Server)

		wtServer := &webtransport.Server{
			H3:                   h3Server,
			ApplicationProtocols: []string{moqLiteProtocol, moqWireProtocol},
			CheckOrigin:          func(*http.Request) bool { return true },
		}

		mux := http.NewServeMux()
		mux.HandleFunc(s.config.Path, func(w http.ResponseWriter, r *http.Request) {
			s.handleWebTransport(w, r, wtServer)
		})
		h3Server.Handler = mux

		targets = append(targets, listenTarget{
			server: wtServer,
			conn:   conn,
			addr:   bindAddr,
		})
	}

	s.lock.Lock()
	defer s.lock.Unlock()

	for _, target := range targets {
		s.servers = append(s.servers, target.server)
		s.wg.Add(1)
		go func(target listenTarget) {
			defer s.wg.Done()
			defer target.conn.Close()
			s.logger.Infow("starting moq webtransport listener", "addr", target.addr, "path", s.config.Path)
			if err := target.server.Serve(target.conn); err != nil && !s.stopped.Load() {
				s.logger.Errorw("moq webtransport listener stopped", err, "addr", target.addr)
			}
		}(target)
	}

	return nil
}

func (s *MoQService) Stop(ctx context.Context) error {
	s.stopped.Store(true)

	s.lock.Lock()
	servers := append([]*webtransport.Server(nil), s.servers...)
	s.lock.Unlock()

	var closeErr error
	for _, server := range servers {
		if err := server.Close(); err != nil && closeErr == nil {
			closeErr = err
		}
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return closeErr
	}
}

func (s *MoQService) handleWebTransport(w http.ResponseWriter, r *http.Request, wtServer *webtransport.Server) {
	claims, err := s.authenticate(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if claims.Video == nil || !claims.Video.RoomJoin || !claims.Video.GetCanSubscribe() {
		http.Error(w, ErrPermissionDenied.Error(), http.StatusForbidden)
		return
	}

	roomName := livekit.RoomName(claims.Video.Room)
	if roomName == "" {
		roomName = livekit.RoomName(r.URL.Query().Get("room"))
	}
	if roomName == "" {
		http.Error(w, ErrNoRoomName.Error(), http.StatusBadRequest)
		return
	}

	layer := int32(0)
	if layerValue := r.URL.Query().Get("layer"); layerValue != "" {
		parsed, err := strconv.ParseInt(layerValue, 10, 32)
		if err != nil || parsed < 0 {
			http.Error(w, "invalid layer", http.StatusBadRequest)
			return
		}
		layer = int32(parsed)
	}

	sess, err := wtServer.Upgrade(w, r)
	if err != nil {
		s.logger.Warnw("could not upgrade moq webtransport session", err)
		return
	}
	defer func() {
		_ = sess.CloseWithError(0, "")
	}()

	switch protocol := sess.SessionState().ApplicationProtocol; protocol {
	case moqLiteProtocol:
		s.serveMoQLiteSession(sess, roomName, claims, layer)
	case "", moqWireProtocol:
		resolved, catalog, _, err := s.resolveTrack(sess.Context(), roomName, livekit.TrackID(r.URL.Query().Get("track_id")), claims)
		if err != nil {
			_ = sess.CloseWithError(1, err.Error())
			return
		}
		s.serveSubscription(sess, resolved, catalog, layer)
	default:
		s.logger.Warnw("unsupported moq webtransport protocol", nil, "protocol", protocol)
		_ = sess.CloseWithError(1, "unsupported moq protocol")
	}
}

func (s *MoQService) authenticate(r *http.Request) (*auth.ClaimGrants, error) {
	authHeader := r.Header.Get(authorizationHeader)
	var authToken string
	if authHeader != "" {
		if !strings.HasPrefix(authHeader, bearerPrefix) {
			return nil, ErrMissingAuthorization
		}
		authToken = authHeader[len(bearerPrefix):]
	} else {
		authToken = r.FormValue(accessTokenParam)
	}
	if authToken == "" {
		return nil, ErrMissingAuthorization
	}

	verifier, err := auth.ParseAPIToken(authToken)
	if err != nil {
		return nil, ErrInvalidAuthorizationToken
	}
	secret := s.keyProvider.GetSecret(verifier.APIKey())
	if secret == "" {
		return nil, ErrInvalidAPIKey
	}
	_, claims, err := verifier.Verify(secret)
	if err != nil {
		return nil, ErrInvalidAuthorizationToken
	}
	return claims, nil
}

func (s *MoQService) resolveTrack(
	ctx context.Context,
	roomName livekit.RoomName,
	trackID livekit.TrackID,
	claims *auth.ClaimGrants,
) (moqResolvedTrack, []moqWireCatalogTrack, int, error) {
	var resolved moqResolvedTrack
	room := s.roomManager.GetRoom(ctx, roomName)
	if room == nil {
		return resolved, nil, http.StatusNotFound, ErrRoomNotFound
	}

	var candidates []moqResolvedTrack
	var catalog []moqWireCatalogTrack
	for _, participant := range room.GetParticipants() {
		for _, track := range participant.GetPublishedTracks() {
			if !isMoQSupportedTrack(track) {
				continue
			}
			if !participant.HasPermission(track.ID(), livekit.ParticipantIdentity(claims.Identity)) {
				continue
			}
			tap := s.tracks.AttachTrack(room, participant, track)
			if tap == nil {
				continue
			}
			catalog = append(catalog, tap.CatalogTracks()...)
			candidate := moqResolvedTrack{room: room, participant: participant, track: track, tap: tap}
			if trackID == "" || track.ID() == trackID {
				candidates = append(candidates, candidate)
			}
		}
	}

	if trackID != "" && len(candidates) == 0 {
		return resolved, catalog, http.StatusNotFound, fmt.Errorf("track not found or not available for moq: %s", trackID)
	}
	if trackID == "" && len(candidates) != 1 {
		return resolved, catalog, http.StatusBadRequest, fmt.Errorf("track_id is required when %d moq-compatible tracks are available", len(candidates))
	}

	return candidates[0], catalog, http.StatusOK, nil
}

func (s *MoQService) serveSubscription(
	sess *webtransport.Session,
	resolved moqResolvedTrack,
	catalog []moqWireCatalogTrack,
	layer int32,
) {
	ctx := sess.Context()
	stream, err := sess.OpenUniStreamSync(ctx)
	if err != nil {
		s.logger.Warnw("could not open moq media stream", err, "trackID", resolved.track.ID())
		return
	}
	defer func() {
		_ = stream.Close()
	}()

	sub, cached, unsubscribe := resolved.tap.Subscribe(layer)
	defer unsubscribe()

	if err := s.writeCatalog(stream, resolved.room.Name(), resolved.track.ID(), catalog); err != nil {
		s.logger.Debugw("could not write moq catalog", "error", err)
		return
	}
	if cached != nil {
		if err := s.writeSample(stream, cached); err != nil {
			s.logger.Debugw("could not write cached moq keyframe", "error", err)
			return
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.done:
			return
		case sample := <-sub.queue:
			if err := s.writeSample(stream, sample); err != nil {
				s.logger.Debugw("could not write moq sample", "error", err)
				return
			}
		}
	}
}

func (s *MoQService) writeCatalog(w interface {
	SetWriteDeadline(time.Time) error
	Write([]byte) (int, error)
}, roomName livekit.RoomName, trackID livekit.TrackID, tracks []moqWireCatalogTrack) error {
	if err := w.SetWriteDeadline(time.Now().Add(s.config.WriteTimeout)); err != nil {
		return err
	}
	return writeMoQWireMessage(w, moqWireMessage{
		Type:     "catalog",
		Protocol: moqWireProtocol,
		Room:     string(roomName),
		TrackID:  string(trackID),
		Tracks:   tracks,
	}, nil)
}

func (s *MoQService) writeSample(w interface {
	SetWriteDeadline(time.Time) error
	Write([]byte) (int, error)
}, sample *moqSample) error {
	if err := w.SetWriteDeadline(time.Now().Add(s.config.WriteTimeout)); err != nil {
		return err
	}
	return writeMoQWireMessage(w, moqWireMessage{
		Type:            "sample",
		Protocol:        moqWireProtocol,
		TrackID:         string(sample.TrackID),
		TrackName:       sample.TrackName,
		PublisherID:     string(sample.PublisherID),
		Publisher:       string(sample.Publisher),
		MimeType:        sample.MimeType,
		PayloadFormat:   "annexb",
		Width:           sample.Width,
		Height:          sample.Height,
		Layer:           sample.Layer,
		Sequence:        sample.Sequence,
		RTPTime:         sample.RTPTime,
		KeyFrame:        sample.KeyFrame,
		Cached:          sample.Cached,
		SentAtUnixNs:    sample.SentAtUnixNs,
		UserTimestampUs: sample.UserTimestampUs,
		FrameID:         sample.FrameID,
	}, sample.Payload)
}

func (s *MoQService) tlsConfig() (*tls.Config, error) {
	if s.config.CertFile != "" || s.config.KeyFile != "" {
		if s.config.CertFile == "" || s.config.KeyFile == "" {
			return nil, errors.New("moq cert_file and key_file must be configured together")
		}
		cert, err := tls.LoadX509KeyPair(s.config.CertFile, s.config.KeyFile)
		if err != nil {
			return nil, err
		}
		return &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{cert},
		}, nil
	}

	if !s.development {
		return nil, errors.New("moq cert_file and key_file must be configured outside development mode")
	}

	cert, err := generateMoQSelfSignedCert()
	if err != nil {
		return nil, err
	}
	s.logger.Warnw("using ephemeral self-signed certificate for moq; configure moq.cert_file and moq.key_file for production", nil)
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
	}, nil
}

func generateMoQSelfSignedCert() (tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"LiveKit Development"},
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})
	return tls.X509KeyPair(certPEM, keyPEM)
}

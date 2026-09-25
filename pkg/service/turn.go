// Copyright 2023 LiveKit, Inc.
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
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/jxskiss/base62"
	"github.com/pion/stun/v3"
	"github.com/pion/turn/v5"
	"github.com/pires/go-proxyproto"
	"github.com/pkg/errors"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/logger/pionlogger"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
)

const (
	LivekitRealm = "livekit"

	allocateRetries = 50
)

var ErrExpired = errors.New("expired")

// parsePeerCIDRs compiles a list of CIDR strings, failing with a field-specific
// error on any invalid entry so a malformed peer policy is never silently ignored.
func parsePeerCIDRs(field string, cidrs []string) ([]*net.IPNet, error) {
	parsed := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, ipnet, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q in %s: %w", cidr, field, err)
		}
		parsed = append(parsed, ipnet)
	}
	return parsed, nil
}

func NewTurnServer(conf *config.Config, authHandler turn.AuthHandler, standalone bool) (*turn.Server, error) {
	turnConf := conf.TURN
	if !turnConf.Enabled {
		return nil, nil
	}

	if turnConf.TLSPort <= 0 && turnConf.UDPPort <= 0 {
		return nil, errors.New("invalid TURN ports")
	} else if turnConf.TLSPort > 0 {
		if turnConf.Domain == "" {
			return nil, errors.New("TURN domain required")
		}

		if !IsValidDomain(turnConf.Domain) {
			return nil, errors.New("TURN domain is not correct")
		}
	}

	// parse peer CIDR policies once at startup so a malformed entry fails loudly
	// instead of being silently skipped on every permission decision (fail-open)
	allowRestrictedPeerCIDRs, err := parsePeerCIDRs("turn.allow_restricted_peer_cidrs", turnConf.AllowRestrictedPeerCIDRs)
	if err != nil {
		return nil, err
	}
	denyPeerCIDRs, err := parsePeerCIDRs("turn.deny_peer_cidrs", turnConf.DenyPeerCIDRs)
	if err != nil {
		return nil, err
	}

	serverConfig := turn.ServerConfig{
		Realm:         LivekitRealm,
		AuthHandler:   authHandler,
		LoggerFactory: pionlogger.NewLoggerFactory(logger.GetLogger()),
	}

	// cap concurrent relay allocations per participant so one credential cannot
	// exhaust the shared relay-port range (a value <= 0 disables the quota)
	if turnConf.PerUserRelayAllocationLimit > 0 {
		quota := newTURNAllocationQuota(turnConf.PerUserRelayAllocationLimit)
		serverConfig.QuotaHandler = quota.Allow
		serverConfig.EventHandler = quota.eventHandler()
	}

	var logValues []any
	logValues = append(logValues, "turn.relay_range_start", turnConf.RelayPortRangeStart)
	logValues = append(logValues, "turn.relay_range_end", turnConf.RelayPortRangeEnd)
	logValues = append(logValues, "turn.per_user_relay_allocation_limit", turnConf.PerUserRelayAllocationLimit)

	for _, addr := range turnConf.BindAddresses {
		relayAddrGen, err := newTURNRelayAddressGenerator(conf, addr)
		if err != nil {
			return nil, err
		}
		if standalone {
			relayAddrGen = telemetry.NewRelayAddressGenerator(relayAddrGen)
		}

		permissionHandler := func(_clientAddr net.Addr, peerIP net.IP) bool {
			// restricted peer IP is denied by default, unless allowed by the allow list,
			if peerIP.IsLoopback() ||
				peerIP.IsLinkLocalUnicast() ||
				peerIP.IsLinkLocalMulticast() ||
				peerIP.IsMulticast() ||
				peerIP.IsPrivate() ||
				peerIP.IsUnspecified() {
				allowed := false
				for _, ipnet := range allowRestrictedPeerCIDRs {
					if ipnet.Contains(peerIP) {
						allowed = true
						break
					}
				}
				if !allowed {
					return false
				}

				// if allowed, check deny list for overrides
			}

			for _, ipnet := range denyPeerCIDRs {
				if ipnet.Contains(peerIP) {
					return false
				}
			}

			return true
		}

		if turnConf.TLSPort > 0 {
			listener, err := newTURNTCPListener(turnConf, net.JoinHostPort(addr, strconv.Itoa(turnConf.TLSPort)))
			if err != nil {
				return nil, err
			}
			if standalone {
				listener = telemetry.NewListener(listener)
			}

			listenerConfig := turn.ListenerConfig{
				Listener:              listener,
				RelayAddressGenerator: relayAddrGen,
				PermissionHandler:     permissionHandler,
			}
			serverConfig.ListenerConfigs = append(serverConfig.ListenerConfigs, listenerConfig)

			logValues = append(logValues, "turn.portTLS", turnConf.TLSPort, "turn.externalTLS", turnConf.ExternalTLS, "turn.proxyProtocol", turnConf.ProxyProtocol)
		}

		if turnConf.UDPPort > 0 {
			udpListener, err := net.ListenPacket("udp", net.JoinHostPort(addr, strconv.Itoa(turnConf.UDPPort)))
			if err != nil {
				return nil, errors.Wrap(err, "could not listen on TURN UDP port")
			}

			if standalone {
				udpListener = telemetry.NewPacketConn(udpListener, prometheus.Incoming)
			}

			packetConfig := turn.PacketConnConfig{
				PacketConn:            udpListener,
				RelayAddressGenerator: relayAddrGen,
				PermissionHandler:     permissionHandler,
			}
			serverConfig.PacketConnConfigs = append(serverConfig.PacketConnConfigs, packetConfig)
			logValues = append(logValues, "turn.portUDP", turnConf.UDPPort)
		}
	}

	logger.Infow("Starting TURN server", logValues...)
	return turn.NewServer(serverConfig)
}

var errTURNRelayFamilyUnavailable = errors.New("no node IP for requested TURN relay address family")

// newTURNRelayAddressGenerator creates the relay address generator for a TURN
// bind address. Relays are allocated in the address family requested by the
// client (RFC 6156, defaulting to the family the client connected with), so
// each family needs its own relay socket address and advertised node IP.
// Wildcard bind addresses relay on the wildcard of every family the node has
// an IP for; specific bind addresses relay on that address only.
func newTURNRelayAddressGenerator(conf *config.Config, bindAddr string) (turn.RelayAddressGenerator, error) {
	ip := net.ParseIP(bindAddr)
	if ip == nil {
		return nil, fmt.Errorf("invalid TURN bind address %q, must be an IP address", bindAddr)
	}

	nodeIP := conf.RTC.NodeIP
	newGen := func(relayIP string, listenAddr string) turn.RelayAddressGenerator {
		return &turn.RelayAddressGeneratorPortRange{
			RelayAddress: net.ParseIP(relayIP),
			Address:      listenAddr,
			MinPort:      conf.TURN.RelayPortRangeStart,
			MaxPort:      conf.TURN.RelayPortRangeEnd,
			MaxRetries:   allocateRetries,
		}
	}

	gen := &familyRelayAddressGenerator{}
	isV4 := ip.To4() != nil
	switch {
	case ip.IsUnspecified():
		if nodeIP.V4 != "" {
			gen.v4 = newGen(nodeIP.V4, net.IPv4zero.String())
		}
		if nodeIP.V6 != "" {
			gen.v6 = newGen(nodeIP.V6, net.IPv6unspecified.String())
		}
	case isV4:
		if nodeIP.V4 != "" {
			gen.v4 = newGen(nodeIP.V4, bindAddr)
		}
	default:
		if nodeIP.V6 != "" {
			gen.v6 = newGen(nodeIP.V6, bindAddr)
		}
	}

	// clients of an IPv4 bind address get IPv4 relays by default, while an IPv6
	// wildcard bind address accepts clients of both families
	if (isV4 && gen.v4 == nil) || (gen.v4 == nil && gen.v6 == nil) {
		return nil, fmt.Errorf(
			"no matching node IP for TURN relay on bind address %s (rtc.node_ip: ipv4=%q, ipv6=%q), set rtc.node_ip or turn.bind_addresses accordingly",
			bindAddr, nodeIP.V4, nodeIP.V6,
		)
	}
	if gen.v4 == nil {
		logger.Warnw("TURN relay has no IPv4 node IP, IPv4 relay allocations will be rejected", nil, "bindAddress", bindAddr)
	}
	return gen, nil
}

// familyRelayAddressGenerator dispatches relay allocations to the generator of
// the requested address family, so the advertised relay address always matches
// the family of the relay socket.
type familyRelayAddressGenerator struct {
	v4 turn.RelayAddressGenerator
	v6 turn.RelayAddressGenerator
}

func (g *familyRelayAddressGenerator) forNetwork(network string) (turn.RelayAddressGenerator, error) {
	gen := g.v4
	if strings.HasSuffix(network, "6") {
		gen = g.v6
	}
	if gen == nil {
		return nil, fmt.Errorf("%w: %s", errTURNRelayFamilyUnavailable, network)
	}
	return gen, nil
}

func (g *familyRelayAddressGenerator) Validate() error {
	if g.v4 == nil && g.v6 == nil {
		return errTURNRelayFamilyUnavailable
	}
	for _, gen := range []turn.RelayAddressGenerator{g.v4, g.v6} {
		if gen == nil {
			continue
		}
		if err := gen.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (g *familyRelayAddressGenerator) AllocatePacketConn(c turn.AllocateListenerConfig) (net.PacketConn, net.Addr, error) {
	gen, err := g.forNetwork(c.Network)
	if err != nil {
		return nil, nil, err
	}
	return gen.AllocatePacketConn(c)
}

func (g *familyRelayAddressGenerator) AllocateListener(c turn.AllocateListenerConfig) (net.Listener, net.Addr, error) {
	gen, err := g.forNetwork(c.Network)
	if err != nil {
		return nil, nil, err
	}
	return gen.AllocateListener(c)
}

func (g *familyRelayAddressGenerator) AllocateConn(c turn.AllocateConnConfig) (net.Conn, error) {
	gen, err := g.forNetwork(c.Network)
	if err != nil {
		return nil, err
	}
	return gen.AllocateConn(c)
}

// newTURNTCPListener returns the TCP listener for TURN/TLS. The PROXY protocol
// header, when enabled, is read before TLS so the client address is known to
// the TLS layer and to TURN regardless of who terminates TLS.
func newTURNTCPListener(turnConf config.TURNConfig, address string) (net.Listener, error) {
	var tlsConfig *tls.Config
	if !turnConf.ExternalTLS {
		cert, err := tls.LoadX509KeyPair(turnConf.CertFile, turnConf.KeyFile)
		if err != nil {
			return nil, errors.Wrap(err, "TURN tls cert required")
		}
		tlsConfig = &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
		}
	}

	var proxyPolicy proxyproto.ConnPolicyFunc
	if turnConf.ProxyProtocol {
		trusted, err := parsePeerCIDRs("turn.proxy_protocol_trusted_cidrs", turnConf.ProxyProtocolTrustedCIDRs)
		if err != nil {
			return nil, err
		}
		if len(trusted) == 0 {
			return nil, errors.New("turn.proxy_protocol requires at least one entry in turn.proxy_protocol_trusted_cidrs")
		}
		proxyPolicy = proxyProtocolPolicy(trusted)
	}

	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, errors.Wrap(err, "could not listen on TURN TCP port")
	}
	if proxyPolicy != nil {
		listener = &proxyproto.Listener{Listener: listener, ConnPolicy: proxyPolicy}
	}
	if tlsConfig != nil {
		listener = tls.NewListener(listener, tlsConfig)
	}
	return listener, nil
}

// proxyProtocolPolicy requires the PROXY header from trusted proxies and closes
// every other connection, so the header cannot be forged by a direct client.
func proxyProtocolPolicy(trusted []*net.IPNet) proxyproto.ConnPolicyFunc {
	return func(opts proxyproto.ConnPolicyOptions) (proxyproto.Policy, error) {
		tcpAddr, ok := opts.Upstream.(*net.TCPAddr)
		if !ok {
			return proxyproto.REJECT, fmt.Errorf("%w: unexpected address %v", proxyproto.ErrInvalidUpstream, opts.Upstream)
		}
		for _, ipnet := range trusted {
			if ipnet.Contains(tcpAddr.IP) {
				return proxyproto.REQUIRE, nil
			}
		}
		// wrapping ErrInvalidUpstream closes this connection and keeps the listener accepting
		return proxyproto.REJECT, fmt.Errorf("%w: %s is not a trusted proxy", proxyproto.ErrInvalidUpstream, tcpAddr.IP)
	}
}

func getTURNAuthHandlerFunc(handler *TURNAuthHandler) turn.AuthHandler {
	return handler.HandleAuth
}

type TURNAuthHandler struct {
	keyProvider auth.KeyProvider
}

func NewTURNAuthHandler(keyProvider auth.KeyProvider) *TURNAuthHandler {
	return &TURNAuthHandler{
		keyProvider: keyProvider,
	}
}

func (h *TURNAuthHandler) CreateUsername(apiKey string, pID livekit.ParticipantID, ttlSeconds int) (string, int64) {
	// clamp defensively: non-positive TTLs fall back to the default and overflowing ones are capped
	ttlSeconds, _ = config.ClampTURNTTLSeconds(ttlSeconds)
	expiry := time.Now().Add(time.Duration(ttlSeconds) * time.Second).Unix()
	return base62.EncodeToString(fmt.Appendf(nil, "%s|%s|%d", apiKey, pID, expiry)), expiry
}

func (h *TURNAuthHandler) ParseUsername(username string) (string, livekit.ParticipantID, int64, error) {
	decoded, err := base62.DecodeString(username)
	if err != nil {
		return "", "", 0, err
	}
	parts := strings.Split(string(decoded), "|")
	if len(parts) != 3 {
		return "", "", 0, errors.New("invalid username")
	}
	expiry, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", "", 0, err
	}
	if expiry == 0 {
		return "", "", 0, ErrExpired
	}

	return parts[0], livekit.ParticipantID(parts[1]), expiry, nil
}

func (h *TURNAuthHandler) CreatePassword(apiKey string, pID livekit.ParticipantID, expiry int64) (string, error) {
	if expiry == 0 || time.Now().After(time.Unix(expiry, 0)) {
		return "", ErrExpired
	}
	return h.computePassword(apiKey, pID, expiry)
}

func (h *TURNAuthHandler) computePassword(apiKey string, pID livekit.ParticipantID, expiry int64) (string, error) {
	secret := h.keyProvider.GetSecret(apiKey)
	if secret == "" {
		return "", ErrInvalidAPIKey
	}

	keyInput := fmt.Sprintf("%s|%s|%d", secret, pID, expiry)

	sum := sha256.Sum256([]byte(keyInput))
	return base62.EncodeToString(sum[:]), nil
}

func (h *TURNAuthHandler) HandleAuth(ra *turn.RequestAttributes) (userID string, key []byte, ok bool) {
	username := ra.Username
	decoded, err := base62.DecodeString(username)
	if err != nil {
		return "", nil, false
	}
	parts := strings.Split(string(decoded), "|")
	if len(parts) != 3 {
		return "", nil, false
	}
	expiry, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", nil, false
	}
	if expiry == 0 {
		return "", nil, false
	}
	expiryTime := time.Unix(expiry, 0)
	if time.Now().After(expiryTime) {
		// TTL only applies to initial allocation. Refresh / CreatePermission /
		// ChannelBind / Send / Data requests are still authenticated against the
		// username/password but skip the TTL check so long-running sessions can
		// keep refreshing past the credential expiry.
		if ra.Method == stun.MethodAllocate {
			logger.Infow("TURN credential expired", "username", decoded, "participantID", parts[1], "expiry", expiryTime, "method", ra.Method)
			return "", nil, false
		}
	}
	password, err := h.computePassword(parts[0], livekit.ParticipantID(parts[1]), expiry)
	if err != nil {
		logger.Warnw("could not create TURN password", err, "username", decoded)
		return "", nil, false
	}
	return parts[1], turn.GenerateAuthKey(username, LivekitRealm, password), true
}

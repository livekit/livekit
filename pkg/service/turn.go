package service

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"strconv"
	"time"

	"github.com/pion/turn/v2"
	"github.com/pkg/errors"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/routing"
)

const (
	allocateRetries = 1000
	livekitRealm    = "livekit"
)

func NewTurnServer(conf *config.Config, roomStore RoomStore, node routing.LocalNode) (*turn.Server, error) {
	turnConf := conf.TURN
	if !turnConf.Enabled {
		return nil, nil
	}
	serverConfig := turn.ServerConfig{
		Realm:       livekitRealm,
		AuthHandler: newTurnAuthHandler(roomStore),
	}

	if turnConf.TCPPort > 0 {
		cert, err := generateTLSCert(conf, node.Ip)
		if err != nil {
			return nil, errors.Wrap(err, "failed to load tls cert")
		}

		tlsListener, err := tls.Listen("tcp4", "0.0.0.0:"+strconv.Itoa(turnConf.TCPPort), &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
		})
		if err != nil {
			return nil, errors.Wrap(err, "could not listen on TURN TCP port")
		}

		serverConfig.ListenerConfigs = []turn.ListenerConfig{
			{
				Listener: tlsListener,
				RelayAddressGenerator: &turn.RelayAddressGeneratorPortRange{
					RelayAddress: net.ParseIP(node.Ip),
					Address:      "0.0.0.0",
					MinPort:      turnConf.PortRangeStart,
					MaxPort:      turnConf.PortRangeEnd,
					MaxRetries:   allocateRetries,
				},
			},
		}
	}

	logger.Infow("Starting TURN server",
		"TCP port", turnConf.TCPPort,
		"portRange", fmt.Sprintf("%d-%d", turnConf.PortRangeStart, turnConf.PortRangeEnd))
	return turn.NewServer(serverConfig)
}

func generateTLSCert(conf *config.Config, ip string) (tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}

	maxBigInt := new(big.Int)
	maxBigInt.Exp(big.NewInt(2), big.NewInt(130), nil).Sub(maxBigInt, big.NewInt(1))
	serialNumber, err := rand.Int(rand.Reader, maxBigInt)
	if err != nil {
		return tls.Certificate{}, err
	}

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Livekit"},
		},
		IPAddresses:           []net.IP{net.ParseIP(ip)},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	// if conf.Development {
	// 	template.DNSNames = []string{"localhost"}
	// } else if conf.TURN.Domain != "" {
	// 	template.DNSNames = []string{conf.TURN.Domain}
	// }

	b, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, err
	}

	certPEMBlock := &bytes.Buffer{}
	err = pem.Encode(certPEMBlock, &pem.Block{Type: "CERTIFICATE", Bytes: b})
	if err != nil {
		return tls.Certificate{}, err
	}

	b, err = x509.MarshalECPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEMBlock := &bytes.Buffer{}
	err = pem.Encode(keyPEMBlock, &pem.Block{Type: "EC PRIVATE KEY", Bytes: b})
	if err != nil {
		return tls.Certificate{}, err
	}

	return tls.X509KeyPair(certPEMBlock.Bytes(), keyPEMBlock.Bytes())
}

func newTurnAuthHandler(roomStore RoomStore) turn.AuthHandler {
	return func(username, realm string, srcAddr net.Addr) (key []byte, ok bool) {
		// room id should be the username, create a hashed room id
		rm, err := roomStore.GetRoom(username)
		if err != nil {
			return nil, false
		}

		return turn.GenerateAuthKey(username, livekitRealm, rm.TurnPassword), true
	}
}

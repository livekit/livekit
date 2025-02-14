package service

import (
	"fmt"
	"github.com/inconshreveable/go-vhost"
	"github.com/livekit/livekit-server/pkg/config"
	"golang.org/x/crypto/acme/autocert"
	"net"
	"net/http"
	"time"
)

func NewCertManager(conf *config.Config) (*autocert.Manager, error) {
	if conf.Domain != "" && conf.TURN.Domain != "" {
		certManager := autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(conf.Domain, conf.TURN.Domain),
		}

		dir := cacheDir()
		if dir != "" {
			certManager.Cache = autocert.DirCache(dir)
		}

		go http.ListenAndServe("0.0.0.0:80", certManager.HTTPHandler(nil))
		return &certManager, nil
	}
	return nil, fmt.Errorf("domains not set")
}

func NewVhostMuxer(conf *config.Config) (*vhost.TLSMuxer, error) {
	if conf.Domain != "" && conf.TURN.Domain != "" {
		listener, err := net.Listen("tcp4", "0.0.0.0:443")

		if err != nil {
			return nil, err
		}

		return vhost.NewTLSMuxer(listener, 5*time.Second)
	} else {
		return nil, fmt.Errorf("domain or turn domain not set")
	}
}

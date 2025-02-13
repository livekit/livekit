package service

import (
	"crypto/tls"
	"fmt"
	"github.com/inconshreveable/go-vhost"
	"github.com/livekit/livekit-server/pkg/config"
	"golang.org/x/crypto/acme/autocert"
	"net/http"
	"time"
)

func NewVhostMuxer(conf *config.Config) (*vhost.TLSMuxer, error) {
	addresses := conf.BindAddresses
	if addresses == nil {
		addresses = []string{""}
	}
	if len(addresses) != 1 {
		return nil, fmt.Errorf("single bind address not set")
	}

	if conf.Domain != "" && conf.TURN.Domain != "" {
		certManager := autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(conf.Domain, conf.TURN.Domain),
		}

		dir := cacheDir()
		if dir != "" {
			certManager.Cache = autocert.DirCache(dir)
		}

		tlsListener, err := tls.Listen("tcp4", addresses[0]+":443",
			&tls.Config{
				GetCertificate: certManager.GetCertificate,
			})
		if err != nil {
			return nil, err
		}
		go http.ListenAndServe(addresses[0]+":80", certManager.HTTPHandler(nil))
		return vhost.NewTLSMuxer(tlsListener, 5*time.Second)
	} else {
		return nil, fmt.Errorf("domain or turn domain not set")
	}
}

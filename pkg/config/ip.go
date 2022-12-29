package config

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/pion/stun"
	"github.com/pkg/errors"

	"github.com/livekit/protocol/logger"
)

func (conf *Config) determineIP() (string, error) {
	if conf.RTC.UseExternalIP {
		stunServers := conf.RTC.STUNServers
		if len(stunServers) == 0 {
			stunServers = DefaultStunServers
		}
		var err error
		for i := 0; i < 3; i++ {
			var ip string
			ip, err = GetExternalIP(stunServers, nil)
			if err == nil {
				return ip, nil
			} else {
				time.Sleep(500 * time.Millisecond)
			}
		}
		return "", errors.Errorf("could not resolve external IP: %v", err)
	}

	// use local ip instead
	addresses, err := GetLocalIPAddresses(false)
	if len(addresses) > 0 {
		return addresses[0], err
	}
	return "", err
}

func GetLocalIPAddresses(includeLoopback bool) ([]string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	loopBacks := make([]string, 0)
	addresses := make([]string, 0)
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch typedAddr := addr.(type) {
			case *net.IPNet:
				ip = typedAddr.IP.To4()
			case *net.IPAddr:
				ip = typedAddr.IP.To4()
			default:
				continue
			}
			if ip == nil {
				continue
			}
			if ip.IsLoopback() {
				loopBacks = append(loopBacks, ip.String())
			} else {
				addresses = append(addresses, ip.String())
			}
		}
	}

	if includeLoopback {
		addresses = append(addresses, loopBacks...)
	}

	if len(addresses) > 0 {
		return addresses, nil
	}
	if len(loopBacks) > 0 {
		return loopBacks, nil
	}
	return nil, fmt.Errorf("could not find local IP address")
}

// GetExternalIP return external IP for localAddr from stun server. If localAddr is nil, a local address is chosen automatically.
func GetExternalIP(stunServers []string, localAddr net.Addr) (string, error) {
	if len(stunServers) == 0 {
		return "", errors.New("STUN servers are required but not defined")
	}
	dialer := &net.Dialer{
		LocalAddr: localAddr,
	}
	conn, err := dialer.Dial("udp4", stunServers[0])
	if err != nil {
		return "", err
	}
	c, err := stun.NewClient(conn)
	if err != nil {
		return "", err
	}
	defer c.Close()

	message, err := stun.Build(stun.TransactionID, stun.BindingRequest)
	if err != nil {
		return "", err
	}

	var stunErr error
	// sufficiently large buffer to not block it
	ipChan := make(chan string, 20)
	err = c.Start(message, func(res stun.Event) {
		if res.Error != nil {
			stunErr = res.Error
			return
		}

		var xorAddr stun.XORMappedAddress
		if err := xorAddr.GetFrom(res.Message); err != nil {
			stunErr = err
			return
		}
		ip := xorAddr.IP.To4()
		if ip != nil {
			ipChan <- ip.String()
		}
	})
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	select {
	case nodeIP := <-ipChan:
		_ = c.Close()
		return nodeIP, validateExternalIP(nodeIP, localAddr.(*net.UDPAddr))
	case <-ctx.Done():
		msg := "could not determine public IP"
		if stunErr != nil {
			return "", errors.Wrap(stunErr, msg)
		} else {
			return "", fmt.Errorf(msg)
		}
	}
}

func validateExternalIP(nodeIP string, addr *net.UDPAddr) error {
	srv, err := net.ListenUDP("udp", addr)
	if err != nil {
		return err
	}
	defer srv.Close()

	magicString := "9#B8D2Nvg2xg5P$ZRwJ+f)*^Nne6*W3WamGY"

	validCh := make(chan struct{})
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := srv.Read(buf)
			if err != nil {
				logger.Infow("error reading from UDP socket", "err", err)
				return
			}
			if string(buf[:n]) == magicString {
				close(validCh)
				return
			}
		}
	}()

	cli, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.ParseIP(nodeIP), Port: srv.LocalAddr().(*net.UDPAddr).Port})
	if err != nil {
		return err
	}
	defer cli.Close()

	if _, err = cli.Write([]byte(magicString)); err != nil {
		return err
	}

	select {
	case <-validCh:
		return nil
	case <-time.After(3 * time.Second):
		break
	}
	return fmt.Errorf("could not validate external IP")
}

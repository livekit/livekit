package utils

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/protocol/logger"
	"github.com/pion/webrtc/v3"
)

func IceServerForStunServers(servers []string) webrtc.ICEServer {
	iceServer := webrtc.ICEServer{}
	for _, stunServer := range servers {
		iceServer.URLs = append(iceServer.URLs, fmt.Sprintf("stun:%s", stunServer))
	}
	return iceServer
}

func GetNAT1to1IPsForConf(conf *config.Config, ipFilter func(net.IP) bool) ([]string, error) {
	stunServers := conf.RTC.STUNServers
	if len(stunServers) == 0 {
		stunServers = config.DefaultStunServers
	}
	localIPs, err := config.GetLocalIPAddresses(conf.RTC.EnableLoopbackCandidate)
	if err != nil {
		return nil, err
	}
	type ipmapping struct {
		externalIP string
		localIP    string
	}
	addrCh := make(chan ipmapping, len(localIPs))

	var udpPorts []int
	if conf.RTC.ICEPortRangeStart != 0 && conf.RTC.ICEPortRangeEnd != 0 {
		portRangeStart, portRangeEnd := uint16(conf.RTC.ICEPortRangeStart), uint16(conf.RTC.ICEPortRangeEnd)
		for i := 0; i < 5; i++ {
			udpPorts = append(udpPorts, rand.Intn(int(portRangeEnd-portRangeStart))+int(portRangeStart))
		}
	} else if conf.RTC.UDPPort != 0 {
		udpPorts = append(udpPorts, int(conf.RTC.UDPPort))
	} else {
		udpPorts = append(udpPorts, 0)
	}

	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	for _, ip := range localIPs {
		if ipFilter != nil && !ipFilter(net.ParseIP(ip)) {
			continue
		}

		wg.Add(1)
		go func(localIP string) {
			defer wg.Done()
			for _, port := range udpPorts {
				addr, err := config.GetExternalIP(ctx, stunServers, &net.UDPAddr{IP: net.ParseIP(localIP), Port: port})
				if err != nil {
					if strings.Contains(err.Error(), "address already in use") {
						logger.Debugw("failed to get external ip, address already in use", "local", localIP, "port", port)
						continue
					}
					logger.Infow("failed to get external ip", "local", localIP, "err", err)
					return
				}
				addrCh <- ipmapping{externalIP: addr, localIP: localIP}
				return
			}
			logger.Infow("failed to get external ip after all ports tried", "local", localIP, "ports", udpPorts)
		}(ip)
	}

	var firstResolved bool
	natMapping := make(map[string]string)
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()

done:
	for {
		select {
		case mapping := <-addrCh:
			if !firstResolved {
				firstResolved = true
				timeout.Reset(1 * time.Second)
			}
			if local, ok := natMapping[mapping.externalIP]; ok {
				logger.Infow("external ip already solved, ignore duplicate",
					"external", mapping.externalIP,
					"local", local,
					"ignore", mapping.localIP)
			} else {
				natMapping[mapping.externalIP] = mapping.localIP
			}

		case <-timeout.C:
			break done
		}
	}
	cancel()
	wg.Wait()

	if len(natMapping) == 0 {
		// no external ip resolved
		return nil, nil
	}

	// mapping unresolved local ip to itself
	for _, local := range localIPs {
		var found bool
		for _, localIPMapping := range natMapping {
			if local == localIPMapping {
				found = true
				break
			}
		}
		if !found {
			natMapping[local] = local
		}
	}

	nat1to1IPs := make([]string, 0, len(natMapping))
	for external, local := range natMapping {
		nat1to1IPs = append(nat1to1IPs, fmt.Sprintf("%s/%s", external, local))
	}
	return nat1to1IPs, nil
}

func InterfaceFilterFromConf(ifs config.InterfacesConfig) func(string) bool {
	includes := ifs.Includes
	excludes := ifs.Excludes
	return func(s string) bool {
		// filter by include interfaces
		if len(includes) > 0 {
			for _, iface := range includes {
				if iface == s {
					return true
				}
			}
			return false
		}

		// filter by exclude interfaces
		if len(excludes) > 0 {
			for _, iface := range excludes {
				if iface == s {
					return false
				}
			}
		}
		return true
	}
}

func IPFilterFromConf(ips config.IPsConfig) (func(ip net.IP) bool, error) {
	var ipnets [2][]*net.IPNet
	var err error
	for i, ips := range [][]string{ips.Includes, ips.Excludes} {
		ipnets[i], err = func(fromIPs []string) ([]*net.IPNet, error) {
			var toNets []*net.IPNet
			for _, ip := range fromIPs {
				_, ipnet, err := net.ParseCIDR(ip)
				if err != nil {
					return nil, err
				}
				toNets = append(toNets, ipnet)
			}
			return toNets, nil
		}(ips)

		if err != nil {
			return nil, err
		}
	}

	includes, excludes := ipnets[0], ipnets[1]

	return func(ip net.IP) bool {
		if len(includes) > 0 {
			for _, ipn := range includes {
				if ipn.Contains(ip) {
					return true
				}
			}
			return false
		}

		if len(excludes) > 0 {
			for _, ipn := range excludes {
				if ipn.Contains(ip) {
					return false
				}
			}
		}
		return true
	}, nil
}
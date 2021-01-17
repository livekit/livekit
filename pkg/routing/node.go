package routing

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"runtime"
	"time"

	"github.com/pion/stun"
	"github.com/pkg/errors"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/livekit-server/proto/livekit"
)

type NodeStats struct {
	NumRooms         int32
	NumClients       int32
	NumVideoChannels int32
	NumAudioChannels int32
	BytesPerMin      int64
}

type LocalNode *livekit.Node

func NewLocalNode(conf *config.Config) (LocalNode, error) {
	ip, err := GetLocalIP(conf.RTC.StunServers)
	if err != nil {
		return nil, err
	}
	return &livekit.Node{
		Id:      fmt.Sprintf("%s%16.16X", utils.NodePrefix, macUint64()),
		Ip:      ip,
		NumCpus: uint32(runtime.NumCPU()),
	}, nil
}

func GetLocalIP(stunServers []string) (string, error) {
	if len(stunServers) == 0 {
		return "", errors.New("STUN servers are required but not defined")
	}
	c, err := stun.Dial("udp4", stunServers[0])
	if err != nil {
		return "", err
	}
	defer c.Close()

	message, err := stun.Build(stun.TransactionID, stun.BindingRequest)
	if err != nil {
		return "", err
	}

	var stunErr error
	var nodeIp string
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
			nodeIp = ip.String()
		}
	})
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for nodeIp == "" {
		select {
		case <-ctx.Done():
			msg := "could not determine public IP"
			if stunErr != nil {
				return "", errors.Wrap(stunErr, msg)
			} else {
				return "", fmt.Errorf(msg)
			}
		case <-time.After(100 * time.Millisecond):
			continue
		}
	}

	return nodeIp, nil
}

func macUint64() uint64 {
	interfaces, err := net.Interfaces()
	if err != nil {
		return 0
	}

	for _, i := range interfaces {
		if i.Flags&net.FlagUp != 0 && bytes.Compare(i.HardwareAddr, nil) != 0 {

			// Skip locally administered addresses
			if i.HardwareAddr[0]&2 == 2 {
				continue
			}

			var mac uint64
			for j, b := range i.HardwareAddr {
				if j >= 8 {
					break
				}
				mac <<= 8
				mac += uint64(b)
			}

			return mac
		}
	}

	return 0
}

package node

import (
	"context"
	"fmt"
	"time"

	"github.com/google/wire"
	"github.com/pion/stun"
	"github.com/pkg/errors"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/livekit-server/proto/livekit"
)

var NodeSet = wire.NewSet(NewLocalNode)

type Node struct {
	livekit.Node
	config *config.Config
}

type NodeStats struct {
	NumRooms         int32
	NumClients       int32
	NumVideoChannels int32
	NumAudioChannels int32
	BytesPerMin      int64
}

func NewLocalNode(conf *config.Config) (*Node, error) {
	n := &Node{
		Node: livekit.Node{
			Id:      utils.NewGuid(utils.NodePrefix),
			RtcPort: conf.RTCPort,
		},
		config: conf,
	}
	if err := n.DiscoverNetworkInfo(); err != nil {
		return nil, err
	}
	return n, nil
}

func (n *Node) DiscoverNetworkInfo() error {
	if len(n.config.RTC.StunServers) == 0 {
		return errors.New("STUN servers are required but not defined")
	}
	c, err := stun.Dial("udp4", n.config.RTC.StunServers[0])
	if err != nil {
		return err
	}
	defer c.Close()

	message, err := stun.Build(stun.TransactionID, stun.BindingRequest)
	if err != nil {
		return err
	}

	var stunErr error
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
			n.Ip = ip.String()
		}
	})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for n.Ip == "" {
		select {
		case <-ctx.Done():
			msg := "could not determine public IP"
			if stunErr != nil {
				return errors.Wrap(stunErr, msg)
			} else {
				return fmt.Errorf(msg)
			}
		case <-time.After(100 * time.Millisecond):
			continue
		}
	}

	return nil
}

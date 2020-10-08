package node

import (
	"errors"

	"github.com/google/uuid"
	"github.com/google/wire"
	"github.com/pion/stun"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/proto/livekit"
)

const ()

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
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	n := &Node{
		Node: livekit.Node{
			Id:      id.String(),
			RtcPort: conf.RTCPort,
		},
		config: conf,
	}
	if err = n.DiscoverNetworkInfo(); err != nil {
		return nil, err
	}
	return n, nil
}

func (n *Node) DiscoverNetworkInfo() error {
	if len(n.config.StunServers) == 0 {
		return errors.New("STUN servers are required but not defined")
	}
	c, err := stun.Dial("udp", n.config.StunServers[0])
	if err != nil {
		return err
	}

	message, err := stun.Build(stun.TransactionID, stun.BindingRequest)
	if err != nil {
		return err
	}

	var stunErr error
	err = c.Do(message, func(res stun.Event) {
		if res.Error != nil {
			stunErr = res.Error
			return
		}

		var xorAddr stun.XORMappedAddress
		if err := xorAddr.GetFrom(res.Message); err != nil {
			stunErr = err
			return
		}
		n.Ip = xorAddr.IP.String()
	})

	if stunErr != nil {
		err = stunErr
	}
	return err
}

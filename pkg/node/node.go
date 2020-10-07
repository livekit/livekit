package node

import (
	"github.com/google/uuid"
	"github.com/google/wire"
	"github.com/pion/stun"

	"github.com/livekit/livekit-server/proto"
)

const (
	googleStunServer = "stun.l.google.com:19302"
)

var NodeSet = wire.NewSet(NewLocalNode)

type Node struct {
	proto.Node
}

type NodeStats struct {
	NumRooms         int32
	NumClients       int32
	NumVideoChannels int32
	NumAudioChannels int32
	BytesPerMin      int64
}

func NewLocalNode() (*Node, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	n := &Node{
		proto.Node{
			Id: id.String(),
		},
	}
	if err = n.DiscoverNetworkInfo(); err != nil {
		return nil, err
	}
	return n, nil
}

func (n *Node) DiscoverNetworkInfo() error {
	c, err := stun.Dial("udp", googleStunServer)
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

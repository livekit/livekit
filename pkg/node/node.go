package node

import (
	"github.com/google/uuid"
	"github.com/pion/stun"
)

const (
	googleStunServer = "stun.l.google.com:19302"
)

type Node struct {
	ID string
	IP string
	Stats NodeStats
}

type NodeStats struct {
	NumRooms int32
	NumClients int32
	NumVideoChannels int32
	NumAudioChannels int32
	BytesPerMin int64
}

func NewLocalNode() (*Node, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	return &Node{
		ID: id.String(),
	}, nil
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
		n.IP = xorAddr.IP.String()
	})

	if stunErr != nil {
		err = stunErr
	}
	return err
}
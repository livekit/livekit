package node

import (
	"fmt"

	"github.com/livekit/livekit-server/proto"
)

// Router selects the best node for room interactions
type Chooser interface {
	ChooseNodeForRoom(roomId string) (*proto.Node, error)
	GetNodeForRoom(roomId string) (*proto.Node, error)
	ClearRoom(roomId string) error
}

func NewSingleNodeChooser(n *Node) *SingleNodeChooser {
	return &SingleNodeChooser{
		localNode: n,
		rooms: make(map[string]bool),
	}
}

type SingleNodeChooser struct {
	localNode *Node
	rooms map[string]bool
}

func (c *SingleNodeChooser) ChooseNodeForRoom(roomId string) (n *proto.Node, err error) {
	if c.rooms[roomId] {
		err = fmt.Errorf("Room already exists")
		return
	}
	c.rooms[roomId] = true
	n = &c.localNode.Node
	return
}

func (c *SingleNodeChooser) GetNodeForRoom(roomId string) (*proto.Node, error) {
	if !c.rooms[roomId] {
		return nil, fmt.Errorf("room %d had not been created", roomId)
	}
	return &c.localNode.Node, nil
}

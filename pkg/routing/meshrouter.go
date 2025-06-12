package routing

import (
	"github.com/livekit/livekit-server/pkg/mesh"
	"github.com/livekit/protocol/livekit"
)

// MeshRouter is a minimal router that registers nodes with a Mesh.
type MeshRouter struct {
	*LocalRouter
	mesh *mesh.Mesh
}

var _ Router = (*MeshRouter)(nil)

// NewMeshRouter creates a MeshRouter using the provided LocalRouter and mesh.
func NewMeshRouter(lr *LocalRouter, m *mesh.Mesh) *MeshRouter {
	return &MeshRouter{LocalRouter: lr, mesh: m}
}

func (r *MeshRouter) RegisterNode() error {
	r.mesh.AddNode(r.currentNode.NodeID(), r.currentNode.NodeIP())
	return r.LocalRouter.RegisterNode()
}

func (r *MeshRouter) UnregisterNode() error {
	r.mesh.RemoveNode(r.currentNode.NodeID())
	return r.LocalRouter.UnregisterNode()
}

func (r *MeshRouter) ListNodes() ([]*livekit.Node, error) {
	infos := r.mesh.ListNodes()
	nodes := make([]*livekit.Node, 0, len(infos)+1)
	for _, inf := range infos {
		nodes = append(nodes, &livekit.Node{Id: string(inf.ID), Ip: inf.Address})
	}
	lrNodes, _ := r.LocalRouter.ListNodes()
	nodes = append(nodes, lrNodes...)
	return nodes, nil
}

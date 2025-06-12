package mesh

import (
	"sync"

	"github.com/livekit/protocol/livekit"
)

// NodeInfo describes a node participating in the mesh.
type NodeInfo struct {
	ID      livekit.NodeID
	Address string
}

// Mesh provides a simple in-memory registry of nodes forming a mesh network.
type Mesh struct {
	mu    sync.RWMutex
	nodes map[livekit.NodeID]*NodeInfo
}

// NewMesh returns an initialized Mesh instance.
func NewMesh() *Mesh {
	return &Mesh{
		nodes: make(map[livekit.NodeID]*NodeInfo),
	}
}

// AddNode registers a node with the mesh.
func (m *Mesh) AddNode(id livekit.NodeID, address string) {
	m.mu.Lock()
	m.nodes[id] = &NodeInfo{ID: id, Address: address}
	m.mu.Unlock()
}

// RemoveNode unregisters a node from the mesh.
func (m *Mesh) RemoveNode(id livekit.NodeID) {
	m.mu.Lock()
	delete(m.nodes, id)
	m.mu.Unlock()
}

// ListNodes returns a copy of the currently registered nodes.
func (m *Mesh) ListNodes() []*NodeInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*NodeInfo, 0, len(m.nodes))
	for _, n := range m.nodes {
		out = append(out, &NodeInfo{ID: n.ID, Address: n.Address})
	}
	return out
}

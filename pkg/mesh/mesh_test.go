package mesh

import "testing"

func TestMeshAddList(t *testing.T) {
	m := NewMesh()
	m.AddNode("node1", "addr1")
	nodes := m.ListNodes()
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	if nodes[0].ID != "node1" || nodes[0].Address != "addr1" {
		t.Fatalf("unexpected node data")
	}
}

package selector

import "github.com/livekit/protocol/livekit"

func FilterNodesByCriteria(nodes []*livekit.Node, criteriaThreashold float32, calculateCriteriaFunc func(*livekit.Node) float32) ([]*livekit.Node, error) {
	nodes = GetAvailableNodes(nodes)
	if len(nodes) == 0 {
		return nil, ErrNoAvailableNodes
	}

	filteredNodes := make([]*livekit.Node, 0)
	for _, node := range nodes {
		if calculateCriteriaFunc(node) < criteriaThreashold {
			filteredNodes = append(filteredNodes, node)
		}
	}
	if len(filteredNodes) > 0 {
		nodes = filteredNodes
	}
	return nodes, nil
}

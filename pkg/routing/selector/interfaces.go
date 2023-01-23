package selector

import (
	"errors"
	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

var ErrUnsupportedSelector = errors.New("unsupported node selector")

type NodeSelection int

const (
	AssignMeeting NodeSelection = iota
	Draining
)

// NodeSelector selects an appropriate node to run the current session
type NodeSelector interface {
	SelectNode(nodes []*livekit.Node, selectionType NodeSelection) (*livekit.Node, error)
}

type NodeFilter interface {
	FilterNodes(nodes []*livekit.Node, selectionType NodeSelection) ([]*livekit.Node, error)
}

type NodeSelectorBase struct {
	Selectors []NodeFilter
	SortBy    string
}

func (s *NodeSelectorBase) SelectNode(nodes []*livekit.Node, selectionType NodeSelection) (*livekit.Node, error) {
	nodes = GetAvailableNodes(nodes)
	if len(nodes) == 0 {
		return nil, ErrNoAvailableNodes
	}
	var err error
	for _, selector := range s.Selectors {
		nodes, err = selector.FilterNodes(nodes, selectionType)
		if err != nil {
			return nil, err
		}
		if len(nodes) == 0 {
			return nil, ErrNoAvailableNodes
		}
	}
	return SelectSortedNode(nodes, s.SortBy)
}

func createSingleNodeSelector(selectorConfig *config.NodeSelectorConfig, region string) (NodeFilter, error) {

	kind := selectorConfig.Kind
	if kind == "" {
		kind = "any"
	}
	switch kind {
	case "any":
		return &AnySelector{}, nil
	case "cpuload":
		return &CPULoadSelector{
			CPULoadLimit: selectorConfig.CPULoadLimit, HardCPULoadLimit: selectorConfig.HardCPULoadLimit}, nil
	case "sysload":
		return &SystemLoadSelector{
			SysloadLimit: selectorConfig.SysloadLimit, HardSysloadLimit: selectorConfig.HardSysloadLimit}, nil
	case "simpleregionaware":
		s, err := NewSimpleRegionAwareSelector(region, selectorConfig.Regions)
		if err != nil {
			return nil, err
		}
		return s, nil
	case "regionaware":
		s, err := NewRegionAwareSelector(region, selectorConfig.Regions)
		if err != nil {
			return nil, err
		}
		s.SysloadLimit = selectorConfig.SysloadLimit
		s.HardSysloadLimit = selectorConfig.HardSysloadLimit
		return s, nil
	case "random":
		logger.Warnw("random node selector is deprecated, please switch to \"any\" or another selector", nil)
		return &AnySelector{}, nil
	default:
		return nil, ErrUnsupportedSelector
	}
}
func CreateNodeSelector(conf *config.Config) (NodeSelector, error) {
	var nodeFilters []NodeFilter
	if len(conf.NodeSelectors.Selectors) != 0 {
		selectors := conf.NodeSelectors.Selectors
		for i := len(selectors) - 1; i >= 0; i-- {
			ns, err := createSingleNodeSelector(&selectors[i], conf.Region)
			if err != nil {
				return nil, err
			}
			nodeFilters = append(nodeFilters, ns)
		}
		return &NodeSelectorBase{SortBy: conf.NodeSelectors.SortBy, Selectors: nodeFilters}, nil
	} else {
		ns, err := createSingleNodeSelector(&conf.NodeSelector, conf.Region)
		if err != nil {
			return nil, err
		}
		nodeFilters = append(nodeFilters, ns)
		return &NodeSelectorBase{SortBy: conf.NodeSelector.SortBy, Selectors: nodeFilters}, nil
	}
}

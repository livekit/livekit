package selector

import (
	"errors"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/config"
)

var ErrUnsupportedSelector = errors.New("unsupported node selector")

// NodeSelector selects an appropriate node to run the current session
type NodeSelector interface {
	SelectNode(nodes []*livekit.Node) (*livekit.Node, error)
}

func CreateNodeSelector(conf *config.Config) (NodeSelector, error) {
	kind := conf.NodeSelector.Kind
	if kind == "" {
		kind = "random"
	}
	switch kind {
	case "cpuload":
		return &CPULoadSelector{
			CPULoadLimit: conf.NodeSelector.CPULoadLimit,
			SortBy: conf.NodeSelector.SortBy,
		}, nil
	case "sysload":
		return &SystemLoadSelector{
			SysloadLimit: conf.NodeSelector.SysloadLimit,
			SortBy: conf.NodeSelector.SortBy,
		}, nil
	case "regionaware":
		s, err := NewRegionAwareSelector(conf.Region, conf.NodeSelector.Regions, conf.NodeSelector.SortBy)
		if err != nil {
			return nil, err
		}
		s.SysloadLimit = conf.NodeSelector.SysloadLimit
		return s, nil
	case "random":
		return &RandomSelector{conf.NodeSelector.SortBy}, nil
	default:
		return nil, ErrUnsupportedSelector
	}
}

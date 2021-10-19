package selector

import (
	"errors"

	livekit "github.com/livekit/protocol/proto"

	"github.com/livekit/livekit-server/pkg/config"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate

// NodeSelector selects an appropriate node to run the current session
//counterfeiter:generate . NodeSelector
type NodeSelector interface {
	SelectNode(nodes []*livekit.Node) (*livekit.Node, error)
}

var ErrUnsupportedSelector = errors.New("unsupported node selector")

func CreateNodeSelector(conf *config.Config) (NodeSelector, error) {
	kind := conf.NodeSelector.Kind
	if kind == "" {
		kind = "random"
	}
	switch kind {
	case "sysload":
		return &SystemLoadSelector{
			SysloadLimit: conf.NodeSelector.SysloadLimit,
		}, nil
	case "regionaware":
		s, err := NewRegionAwareSelector(conf.Region, conf.NodeSelector.Regions)
		if err != nil {
			return nil, err
		}
		s.SysloadLimit = conf.NodeSelector.SysloadLimit
		return s, nil
	case "random":
		return &RandomSelector{}, nil
	default:
		return nil, ErrUnsupportedSelector
	}
}

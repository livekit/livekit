// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package selector

import (
	"errors"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/config"
)

var ErrUnsupportedSelector = errors.New("unsupported node selector")

// NodeSelector selects an appropriate node to run the current session
type NodeSelector interface {
	SelectNode(nodes []*livekit.Node) (*livekit.Node, error)
	filterNodes(nodes []*livekit.Node) ([]*livekit.Node, error)
}

func CreateNodeSelector(conf *config.Config) (NodeSelector, error) {
	kind := conf.NodeSelector.Kind
	if kind == "" {
		kind = "any"
	}
	switch kind {
	case "any":
		return &AnySelector{conf.NodeSelector.SortBy}, nil
	case "cpuload":
		return &CPULoadSelector{
			CPULoadLimit: conf.NodeSelector.CPULoadLimit,
			SortBy:       conf.NodeSelector.SortBy,
		}, nil
	case "sysload":
		return &SystemLoadSelector{
			SysloadLimit: conf.NodeSelector.SysloadLimit,
			SortBy:       conf.NodeSelector.SortBy,
		}, nil
	case "regionaware":
		s, err := NewRegionAwareSelector(conf.Region, conf.NodeSelector.Regions, conf.NodeSelector.SortBy, &SystemLoadSelector{
			SysloadLimit: conf.NodeSelector.SysloadLimit,
		})
		if err != nil {
			return nil, err
		}
		return s, nil
	case "random":
		logger.Warnw("random node selector is deprecated, please switch to \"any\" or another selector", nil)
		return &AnySelector{conf.NodeSelector.SortBy}, nil
	default:
		return nil, ErrUnsupportedSelector
	}
}

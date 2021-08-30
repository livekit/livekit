// +build !linux

package stats

import livekit "github.com/livekit/protocol/proto"

func updateCurrentNodeSystemStats(nodeStats *livekit.NodeStats) error {
	return nil
}

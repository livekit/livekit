//go:build !linux
// +build !linux

package prometheus

import livekit "github.com/livekit/protocol/livekit"

func updateCurrentNodeSystemStats(nodeStats *livekit.NodeStats) error {
	return nil
}

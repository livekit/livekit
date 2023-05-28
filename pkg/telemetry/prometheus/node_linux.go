//go:build linux
// +build linux

package prometheus

import (
	"fmt"

	"github.com/florianl/go-tc"
)

func getTCStats() (packets, drops uint32, err error) {
	rtnl, err := tc.Open(&tc.Config{})
	if err != nil {
		err = fmt.Errorf("could not open rtnetlink socket: %v", err)
		return
	}
	defer rtnl.Close()

	qdiscs, err := rtnl.Qdisc().Get()
	if err != nil {
		err = fmt.Errorf("could not get qdiscs: %v", err)
		return
	}

	for _, qdisc := range qdiscs {
		packets = packets + qdisc.Stats.Packets
		drops = drops + qdisc.Stats.Drops
	}

	return
}

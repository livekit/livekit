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
		// Newer kernels report stats via TCA_STATS2 (Stats2), while older kernels
		// only populate the legacy TCA_STATS attribute (Stats). Prefer Stats2 and
		// fall back to Stats so counters are collected on both.
		//
		// However,  go-tc (through v0.4.8 and current main) mis-parses the nested TCA_STATS2
		// as a flat struct, so Stats2.Packets/.Drops are garbage. The legacy
		// TCA_STATS blob is a real flat struct and is parsed correctly, and modern
		// kernels populate BOTH — so prefer Stats.
		if qdisc.Stats != nil {
			packets += qdisc.Stats.Packets
			drops += qdisc.Stats.Drops
		}
	}

	return
}

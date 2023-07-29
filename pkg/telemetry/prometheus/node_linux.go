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
		packets = packets + qdisc.Stats.Packets
		drops = drops + qdisc.Stats.Drops
	}

	return
}

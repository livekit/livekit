// Copyright 2026 LiveKit, Inc.
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

package prometheus

import (
	"regexp"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
)

// livekit_room_included_total is an additional, opt-in gauge. It counts active rooms whose name
// does NOT match prometheus.room_exclude_regex, so synthetic rooms (load tests, health checks)
var (
	roomExcludeRegex        atomic.Pointer[regexp.Regexp]
	promRoomIncludedCurrent prometheus.Gauge
	roomIncludedInitOnce    sync.Once
)

// InitRoomIncludedStats registers livekit_room_included_total when pattern is non-empty.
// Call once at startup, before any room is created, so start/end decisions use the same regex.
// An empty pattern is a no-op and the metric is not exported.
func InitRoomIncludedStats(nodeID string, nodeType livekit.NodeType, pattern string) error {
	if pattern == "" {
		return nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return err
	}

	var regErr error
	roomIncludedInitOnce.Do(func() {
		promRoomIncludedCurrent = prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace:   livekitNamespace,
			Subsystem:   "room",
			Name:        "included_total",
			Help:        "Number of active rooms whose name does not match prometheus.room_exclude_regex",
			ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
		})
		regErr = prometheus.Register(promRoomIncludedCurrent)
	})
	if regErr != nil {
		return regErr
	}
	roomExcludeRegex.Store(re)
	return nil
}

func roomIncluded(roomName string) bool {
	re := roomExcludeRegex.Load()
	return re != nil && !re.MatchString(roomName)
}

// RoomIncludedStarted increments livekit_room_included_total unless the room is excluded.
func RoomIncludedStarted(roomName livekit.RoomName) {
	if roomIncluded(string(roomName)) {
		promRoomIncludedCurrent.Inc()
	}
}

// RoomIncludedEnded decrements livekit_room_included_total unless the room is excluded.
func RoomIncludedEnded(roomName livekit.RoomName) {
	if roomIncluded(string(roomName)) {
		promRoomIncludedCurrent.Dec()
	}
}

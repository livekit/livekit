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

package telemetry_test

import (
	"context"
	"testing"

	prom "github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
)

type participantCounts struct {
	current       int32   // NodeStats.NumClients, mirrors livekit_participant_total
	signal        float64 // livekit_participant_signal_total
	rtcConnected  uint64
	joinedUnknown float64 // livekit_participant_join_total{state="rtc_connected",sdk="UNKNOWN"}
	joinedAndroid float64 // livekit_participant_join_total{state="rtc_connected",sdk="ANDROID"}
}

func readParticipantCounts(t *testing.T) participantCounts {
	t.Helper()
	stats, err := prometheus.GetNodeStats(0, nil, nil)
	require.NoError(t, err)
	c := participantCounts{current: stats.NumClients, rtcConnected: stats.ParticipantRtcConnected}

	families, err := prom.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, f := range families {
		switch f.GetName() {
		case "livekit_participant_signal_total":
			for _, m := range f.GetMetric() {
				c.signal += m.GetGauge().GetValue()
			}
		case "livekit_participant_join_total":
			for _, m := range f.GetMetric() {
				labels := map[string]string{}
				for _, l := range m.GetLabel() {
					labels[l.GetName()] = l.GetValue()
				}
				if labels["state"] != "rtc_connected" {
					continue
				}
				switch labels["sdk"] {
				case livekit.ClientInfo_UNKNOWN.String():
					c.joinedUnknown += m.GetCounter().GetValue()
				case livekit.ClientInfo_ANDROID.String():
					c.joinedAndroid += m.GetCounter().GetValue()
				}
			}
		}
	}
	return c
}

func (c participantCounts) sub(base participantCounts) participantCounts {
	return participantCounts{
		current:       c.current - base.current,
		signal:        c.signal - base.signal,
		rtcConnected:  c.rtcConnected - base.rtcConnected,
		joinedUnknown: c.joinedUnknown - base.joinedUnknown,
		joinedAndroid: c.joinedAndroid - base.joinedAndroid,
	}
}

type participantCountFixture struct {
	*telemetryServiceFixture
	t           *testing.T
	ctx         context.Context
	room        *livekit.Room
	participant *livekit.ParticipantInfo
	clientInfo  *livekit.ClientInfo
	base        participantCounts
}

func newParticipantCountFixture(t *testing.T, name string) *participantCountFixture {
	return &participantCountFixture{
		telemetryServiceFixture: createFixture(),
		t:                       t,
		ctx:                     context.Background(),
		room:                    &livekit.Room{Sid: "RM_" + name, Name: name},
		participant:             &livekit.ParticipantInfo{Sid: "PA_" + name, Identity: "p"},
		clientInfo:              &livekit.ClientInfo{Sdk: livekit.ClientInfo_ANDROID},
		base:                    readParticipantCounts(t),
	}
}

func (f *participantCountFixture) requireCounts(expected participantCounts) {
	f.t.Helper()
	f.flush()
	require.Equal(f.t, expected, readParticipantCounts(f.t).sub(f.base))
}

func (f *participantCountFixture) signalConnected(guard *telemetry.ReferenceGuard) {
	f.sut.ParticipantSignalConnected(f.ctx, f.room, f.participant, guard)
}

func (f *participantCountFixture) signalDisconnected(guard *telemetry.ReferenceGuard) {
	f.sut.ParticipantSignalDisconnected(f.ctx, f.room, f.participant, guard)
}

func (f *participantCountFixture) joined(guard *telemetry.ReferenceGuard) {
	f.sut.ParticipantJoined(f.ctx, f.room, f.participant, f.clientInfo, nil, true, guard)
}

func (f *participantCountFixture) active(guard *telemetry.ReferenceGuard) {
	f.sut.ParticipantActive(f.ctx, f.room, f.participant, &livekit.AnalyticsClientMeta{}, false, false, guard)
}

func (f *participantCountFixture) left(guard *telemetry.ReferenceGuard) {
	f.sut.ParticipantLeft(f.ctx, f.room, f.participant, true, guard)
}

var joinedAndroid = participantCounts{current: 1, rtcConnected: 1, joinedAndroid: 1}

func Test_ParticipantCount_SignalNode(t *testing.T) {
	// The node serving the participant's signal connection does not host the participant
	// (multi-node deployment): it counts the signal connection, not the participant.
	f := newParticipantCountFixture(t, "signal_node")
	signalGuard := &telemetry.ReferenceGuard{}

	f.signalConnected(signalGuard)
	f.requireCounts(participantCounts{signal: 1})

	f.signalDisconnected(signalGuard)
	f.requireCounts(participantCounts{})
}

func Test_ParticipantCount_HostingNode(t *testing.T) {
	f := newParticipantCountFixture(t, "hosting_node")
	guard := &telemetry.ReferenceGuard{}

	f.joined(guard)
	f.active(guard)
	f.requireCounts(joinedAndroid)

	f.left(guard)
	f.requireCounts(participantCounts{rtcConnected: 1, joinedAndroid: 1})
}

func Test_ParticipantCount_SameNodeSignalFirst(t *testing.T) {
	// Signal connection and participant on the same node, signal first: counted once, with the
	// SDK from ParticipantJoined, and uncounted when it leaves even with the signal still open.
	f := newParticipantCountFixture(t, "same_node_signal_first")
	signalGuard, guard := &telemetry.ReferenceGuard{}, &telemetry.ReferenceGuard{}

	f.signalConnected(signalGuard)
	f.joined(guard)
	f.active(guard)
	f.requireCounts(participantCounts{current: 1, signal: 1, rtcConnected: 1, joinedAndroid: 1})

	f.left(guard)
	f.requireCounts(participantCounts{signal: 1, rtcConnected: 1, joinedAndroid: 1})

	f.signalDisconnected(signalGuard)
	f.requireCounts(participantCounts{rtcConnected: 1, joinedAndroid: 1})
}

func Test_ParticipantCount_SameNodeSignalClosesFirst(t *testing.T) {
	f := newParticipantCountFixture(t, "same_node_signal_closes_first")
	signalGuard, guard := &telemetry.ReferenceGuard{}, &telemetry.ReferenceGuard{}

	f.joined(guard)
	f.signalConnected(signalGuard)
	f.requireCounts(participantCounts{current: 1, signal: 1, rtcConnected: 1, joinedAndroid: 1})

	// the participant is still in the room
	f.signalDisconnected(signalGuard)
	f.requireCounts(joinedAndroid)

	f.left(guard)
	f.requireCounts(participantCounts{rtcConnected: 1, joinedAndroid: 1})
}

func Test_ParticipantCount_LateActiveAfterLeft(t *testing.T) {
	// ParticipantActive overtaken by the close, with the worker still held open by the signal
	// connection on the same node: the participant is not counted again.
	f := newParticipantCountFixture(t, "late_active")
	signalGuard, guard := &telemetry.ReferenceGuard{}, &telemetry.ReferenceGuard{}

	f.signalConnected(signalGuard)
	f.joined(guard)
	f.left(guard)
	f.active(guard)
	f.requireCounts(participantCounts{signal: 1, rtcConnected: 1, joinedAndroid: 1})

	f.signalDisconnected(signalGuard)
	f.requireCounts(participantCounts{rtcConnected: 1, joinedAndroid: 1})
}

func Test_ParticipantCount_DuplicateLeft(t *testing.T) {
	f := newParticipantCountFixture(t, "duplicate_left")
	signalGuard, guard := &telemetry.ReferenceGuard{}, &telemetry.ReferenceGuard{}

	f.signalConnected(signalGuard)
	f.joined(guard)
	f.left(guard)
	f.left(guard)
	f.requireCounts(participantCounts{signal: 1, rtcConnected: 1, joinedAndroid: 1})

	f.signalDisconnected(signalGuard)
	f.requireCounts(participantCounts{rtcConnected: 1, joinedAndroid: 1})
}

func Test_ParticipantCount_JoinedAgainWhileSignalHoldsWorker(t *testing.T) {
	// A new participant session on the node (e.g. migrated back) while the previous one's
	// worker is still held open by its signal connection is counted again.
	f := newParticipantCountFixture(t, "joined_again")
	signalGuard, guard1, guard2 := &telemetry.ReferenceGuard{}, &telemetry.ReferenceGuard{}, &telemetry.ReferenceGuard{}

	f.signalConnected(signalGuard)
	f.joined(guard1)
	f.left(guard1)
	f.joined(guard2)
	f.requireCounts(participantCounts{current: 1, signal: 1, rtcConnected: 2, joinedAndroid: 2})

	f.left(guard2)
	f.signalDisconnected(signalGuard)
	f.requireCounts(participantCounts{rtcConnected: 2, joinedAndroid: 2})
}

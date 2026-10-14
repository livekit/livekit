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

package streamallocator

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/bwe"
	"github.com/livekit/livekit-server/pkg/sfu/ccutils"
	"github.com/livekit/livekit-server/pkg/sfu/pacer"
)

// bwe.NullBWE is meant to be embedded and lacks Type
type testBWE struct {
	*bwe.NullBWE

	lock  sync.Mutex
	state bwe.CongestionState

	finalizeCapacity  int64
	finalizeFinalized bool
}

func (t *testBWE) Type() bwe.BWEType { return bwe.BWETypeNone }

func (t *testBWE) CongestionState() bwe.CongestionState {
	t.lock.Lock()
	defer t.lock.Unlock()

	return t.state
}

func (t *testBWE) setCongestionState(state bwe.CongestionState) {
	t.lock.Lock()
	defer t.lock.Unlock()

	t.state = state
}

func (t *testBWE) ProbeClusterFinalize() (ccutils.ProbeSignal, int64, bool) {
	if t.finalizeFinalized {
		return ccutils.ProbeSignalNotCongesting, t.finalizeCapacity, true
	}
	return ccutils.ProbeSignalInconclusive, 0, false
}

// recordingPacer records the bitrates pushed to it
type recordingPacer struct {
	lock     sync.Mutex
	bitrates []int
}

func (r *recordingPacer) Enqueue(*pacer.Packet)                                          {}
func (r *recordingPacer) Stop()                                                          {}
func (r *recordingPacer) SetInterval(time.Duration)                                      {}
func (r *recordingPacer) TimeSinceLastSentPacket() time.Duration                         { return 0 }
func (r *recordingPacer) SetPacerProbeObserverListener(pacer.PacerProbeObserverListener) {}
func (r *recordingPacer) StartProbeCluster(ccutils.ProbeClusterInfo)                     {}
func (r *recordingPacer) EndProbeCluster(ccutils.ProbeClusterId) ccutils.ProbeClusterInfo {
	return ccutils.ProbeClusterInfoInvalid
}

func (r *recordingPacer) SetBitrate(bitrate int) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.bitrates = append(r.bitrates, bitrate)
}

func (r *recordingPacer) hasBitrate(bitrate int) bool {
	r.lock.Lock()
	defer r.lock.Unlock()

	return slices.Contains(r.bitrates, bitrate)
}

func newTestAllocator(t *testing.T, p pacer.Pacer, b bwe.BWE) *StreamAllocator {
	t.Helper()

	return NewStreamAllocator(
		StreamAllocatorParams{
			Config: DefaultStreamAllocatorConfig,
			BWE:    b,
			Pacer:  p,
			Logger: logger.GetLogger(),
		},
		true,
		false,
	)
}

func TestPacerBitrateFollowsCongestionCapacity(t *testing.T) {
	p := &recordingPacer{}
	b := &testBWE{NullBWE: &bwe.NullBWE{}}
	s := newTestAllocator(t, p, b)
	s.Start()
	defer s.Stop()

	// congestion: pace at the estimated capacity
	b.setCongestionState(bwe.CongestionStateCongested)
	s.OnCongestionStateChange(bwe.CongestionStateNone, bwe.CongestionStateCongested, 2_500_000)

	require.Eventually(t, func() bool {
		return p.hasBitrate(2_500_000)
	}, 2*time.Second, 10*time.Millisecond)

	// clear of congestion: the allocator is no longer constraining egress,
	// so neither is the pacer, even though a capacity was committed earlier
	b.setCongestionState(bwe.CongestionStateNone)
	s.OnCongestionStateChange(bwe.CongestionStateCongested, bwe.CongestionStateNone, 2_500_000)

	require.Eventually(t, func() bool {
		return p.hasBitrate(pacer.InitialBitrate)
	}, 2*time.Second, 10*time.Millisecond)
}

func TestPacerBitrateFollowsProbeResult(t *testing.T) {
	p := &recordingPacer{}
	b := &testBWE{
		NullBWE:           &bwe.NullBWE{},
		finalizeCapacity:  4_000_000,
		finalizeFinalized: true,
	}
	b.setCongestionState(bwe.CongestionStateEarlyWarning)
	s := newTestAllocator(t, p, b)
	s.Start()
	defer s.Stop()

	// probe finalize is only consulted while a probe cluster is active, start
	// one the way a real probe starts, then ask for the result on a ping
	s.postEvent(Event{
		signal: streamAllocatorSignalProbeClusterSwitch,
		probeClusterInfo: ccutils.ProbeClusterInfo{
			Id: 1,
			Goal: ccutils.ProbeClusterGoal{
				DesiredBps:   4_000_000,
				Duration:     time.Second,
				DesiredBytes: 500_000,
			},
		},
	})
	s.postEvent(Event{signal: streamAllocatorSignalPeriodicPing})

	require.Eventually(t, func() bool {
		return p.hasBitrate(4_000_000)
	}, 2*time.Second, 10*time.Millisecond)
}

func TestPacerBitrateFollowsChannelCapacityOverride(t *testing.T) {
	p := &recordingPacer{}
	s := newTestAllocator(t, p, &testBWE{NullBWE: &bwe.NullBWE{}})
	s.Start()
	defer s.Stop()

	s.SetChannelCapacity(3_000_000)

	require.Eventually(t, func() bool {
		return p.hasBitrate(3_000_000)
	}, 2*time.Second, 10*time.Millisecond)

	// clearing the override must not leave the pacer at the old rate
	s.SetChannelCapacity(0)

	require.Eventually(t, func() bool {
		return p.hasBitrate(pacer.InitialBitrate)
	}, 2*time.Second, 10*time.Millisecond)
}

func TestPacerUnconstrainedUsesInitialBitrate(t *testing.T) {
	p := &recordingPacer{}
	s := newTestAllocator(t, p, &testBWE{NullBWE: &bwe.NullBWE{}})

	// no congestion estimate and no override: egress is not capped
	s.updatePacerBitrate()

	p.lock.Lock()
	defer p.lock.Unlock()
	require.Equal(t, []int{pacer.InitialBitrate}, p.bitrates)
}

package streamallocator

import (
	"testing"
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/bwe"
	"github.com/livekit/livekit-server/pkg/sfu/ccutils"
	"github.com/livekit/protocol/logger"
)

func TestEncoderDemandRetainsAndWithdrawsDeficiency(t *testing.T) {
	for _, mode := range []ProbeMode{ProbeModePadding, ProbeModeMedia} {
		t.Run(string(mode), func(t *testing.T) {
			demand := int64(300_000)
			estimator := &encoderDemandBWE{}
			s := &StreamAllocator{params: StreamAllocatorParams{
				Config: DefaultStreamAllocatorConfig, BWE: estimator, Logger: logger.GetDiscardLogger(),
				AdditionalProbeDemand: func() int64 { return demand },
			}, enabled: true, activeProbeClusterId: ccutils.ProbeClusterIdInvalid, committedChannelCapacity: 100_000}
			s.params.Config.ProbeMode = mode
			// Exercise transitions synchronously without starting the ping worker.
			s.isStopped.Store(true)
			s.adjustState()
			want := streamAllocatorStateStable
			if mode == ProbeModePadding {
				want = streamAllocatorStateDeficient
			}
			if s.state != want || s.committedChannelCapacity != 100_000 {
				t.Fatalf("demand was lost or treated as capacity: state=%v capacity=%d", s.state, s.committedChannelCapacity)
			}
			demand = 0
			s.adjustState()
			if s.state != streamAllocatorStateStable {
				t.Fatal("withdrawn demand kept the allocator deficient")
			}
			s.params.AdditionalProbeDemand = nil
			s.adjustState()
			if s.state != streamAllocatorStateStable {
				t.Fatal("nil callback changed ordinary forwarding")
			}
		})
	}
}

func TestEncoderDemandUsesExistingProbeGates(t *testing.T) {
	for _, scenario := range []string{"needed", "satisfied", "negative", "nil", "disabled", "backoff", "capacity-override"} {
		t.Run(scenario, func(t *testing.T) {
			demand := int64(300_000)
			estimator := &encoderDemandBWE{canProbe: true}
			recorder := &encoderDemandProbe{clusters: make(chan ccutils.ProbeClusterInfo, 4)}
			s := &StreamAllocator{params: StreamAllocatorParams{
				Config: DefaultStreamAllocatorConfig, BWE: estimator, Logger: logger.GetDiscardLogger(),
				AdditionalProbeDemand: func() int64 { return demand },
			}, enabled: true, committedChannelCapacity: 100_000}
			s.prober = ccutils.NewProber(ccutils.ProberParams{Listener: recorder, Logger: s.params.Logger})
			defer s.prober.Reset(ccutils.ProbeClusterInfoInvalid)
			switch scenario {
			case "satisfied":
				demand = 0
			case "negative":
				demand = -1
			case "nil":
				s.params.AdditionalProbeDemand = nil
			case "disabled":
				s.enabled = false
			case "backoff":
				estimator.canProbe = false
			case "capacity-override":
				s.overriddenChannelCapacity = 200_000
			}
			s.maybeProbe()
			if s.prober.IsRunning() != (scenario == "needed") {
				t.Fatal("encoder demand bypassed a probe gate")
			}
			if scenario != "needed" {
				return
			}
			select {
			case info := <-recorder.clusters:
				if info.Goal.AvailableBandwidthBps != 100_000 || info.Goal.ExpectedUsageBps != 0 ||
					info.Goal.DesiredBps != 360_000 || info.Goal.Duration != estimator.ProbeDuration() {
					t.Fatalf("demand replaced measured usage, capacity, or probe policy: %+v", info)
				}
			case <-time.After(time.Second):
				t.Fatal("missing encoder-demand probe")
			}
		})
	}
}

type encoderDemandBWE struct {
	bwe.BWE
	canProbe bool
}

func (*encoderDemandBWE) Reset()                       {}
func (e *encoderDemandBWE) CanProbe() bool             { return e.canProbe }
func (*encoderDemandBWE) ProbeDuration() time.Duration { return 200 * time.Millisecond }

type encoderDemandProbe struct {
	clusters chan ccutils.ProbeClusterInfo
}

func (p *encoderDemandProbe) OnProbeClusterSwitch(info ccutils.ProbeClusterInfo) {
	p.clusters <- info
}
func (*encoderDemandProbe) OnSendProbe(int) {}

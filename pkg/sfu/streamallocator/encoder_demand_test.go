package streamallocator

import (
	"testing"
	"time"

	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/bwe"
	"github.com/livekit/livekit-server/pkg/sfu/ccutils"
	"github.com/livekit/livekit-server/pkg/sfu/pacer"
	"github.com/livekit/livekit-server/pkg/sfu/sfufakes"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/pion/webrtc/v4"
)

func TestEncoderProbeDemandDoesNotBlockLayerUpgrade(t *testing.T) {
	log := logger.GetDiscardLogger()
	receiver := &sfufakes.FakeTrackReceiver{}
	receiver.TrackIDReturns("upgrade")
	receiver.TrackInfoReturns(&livekit.TrackInfo{})
	receiver.GetLayeredBitrateReturns([]int32{0, 1}, sfu.Bitrates{{100_000}, {300_000}})
	down, err := sfu.NewDownTrack(sfu.DownTrackParams{
		Codecs:   []webrtc.RTPCodecParameters{{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90_000}}},
		Receiver: receiver, Logger: log, Listener: &sfufakes.FakeDownTrackListener{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer down.Close()
	down.UpTrackMaxPublishedLayerChange(1)
	down.UpTrackMaxTemporalLayerSeenChange(0)
	down.SetMaxSpatialLayer(0)
	down.SetMaxTemporalLayer(0)
	down.AllocateOptimal(false, false)
	if down.BandwidthRequested() != 100_000 {
		t.Fatal("fixture did not allocate its low layer")
	}
	track := NewTrack(down, livekit.TrackSource_SCREEN_SHARE, true, "publisher", log)
	s := &StreamAllocator{params: StreamAllocatorParams{
		Config: DefaultStreamAllocatorConfig, BWE: &encoderDemandBWE{}, Logger: log,
		AdditionalProbeDemand: func() int64 { return 300_000 },
	}, enabled: true, activeProbeClusterId: ccutils.ProbeClusterIdInvalid,
		committedChannelCapacity: 1_000_000, videoTracksShadow: []*Track{track}}
	s.isStopped.Store(true)
	s.adjustState()
	down.SetMaxSpatialLayer(1)
	s.allocateTrack(track)
	if down.BandwidthRequested() != 300_000 {
		t.Fatalf("encoder probe demand blocked a track upgrade with spare capacity: allocated=%d", down.BandwidthRequested())
	}
}

func TestEncoderDemandPollingDoesNotChangeTrackAllocationState(t *testing.T) {
	for _, mode := range []ProbeMode{ProbeModePadding, ProbeModeMedia} {
		t.Run(string(mode), func(t *testing.T) {
			demand := int64(300_000)
			recorder := &encoderDemandProbe{clusters: make(chan ccutils.ProbeClusterInfo, 4)}
			s := &StreamAllocator{params: StreamAllocatorParams{
				Config: DefaultStreamAllocatorConfig, BWE: &encoderDemandBWE{canProbe: true}, Logger: logger.GetDiscardLogger(),
				AdditionalProbeDemand: func() int64 { return demand },
			}, enabled: true, activeProbeClusterId: ccutils.ProbeClusterIdInvalid, committedChannelCapacity: 100_000, lastRTTTime: time.Now()}
			s.params.Config.ProbeMode = mode
			s.prober = ccutils.NewProber(ccutils.ProberParams{Listener: recorder, Logger: s.params.Logger})
			defer s.prober.Reset(ccutils.ProbeClusterInfoInvalid)
			s.handleSignalPeriodicPing(Event{})
			if s.prober.IsRunning() != (mode == ProbeModePadding) || s.state != streamAllocatorStateStable || s.committedChannelCapacity != 100_000 {
				t.Fatal("encoder demand did not probe independently of track allocation")
			}
			s.prober.Reset(ccutils.ProbeClusterInfoInvalid)
			demand = 0
			s.handleSignalPeriodicPing(Event{})
			if s.prober.IsRunning() {
				t.Fatal("withdrawn encoder demand started a new probe")
			}
			s.params.AdditionalProbeDemand = nil
			s.handleSignalPeriodicPing(Event{})
			if s.prober.IsRunning() {
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

func TestIdleEncoderDemandUsesPausedRecovery(t *testing.T) {
	for _, scenario := range []string{"idle", "recent-packet", "pause-disabled", "withdrawn", "nil", "disabled", "media-mode"} {
		t.Run(scenario, func(t *testing.T) {
			estimator := &encoderDemandBWE{congestion: bwe.CongestionStateCongested}
			queue := &encoderDemandPacer{idle: DefaultStreamAllocatorConfig.PausedMinWait + time.Second}
			recorder := &encoderDemandProbe{clusters: make(chan ccutils.ProbeClusterInfo, 4)}
			s := &StreamAllocator{params: StreamAllocatorParams{
				Config: DefaultStreamAllocatorConfig, BWE: estimator, Pacer: queue, Logger: logger.GetDiscardLogger(),
				AdditionalProbeDemand: func() int64 { return 300_000 },
			}, enabled: true, allowPause: true, activeProbeClusterId: ccutils.ProbeClusterIdInvalid,
				committedChannelCapacity: 100_000, lastRTTTime: time.Now()}
			s.prober = ccutils.NewProber(ccutils.ProberParams{Listener: recorder, Logger: s.params.Logger})
			defer s.prober.Reset(ccutils.ProbeClusterInfoInvalid)
			switch scenario {
			case "recent-packet":
				queue.idle = s.params.Config.PausedMinWait
			case "pause-disabled":
				s.allowPause = false
			case "withdrawn":
				s.params.AdditionalProbeDemand = func() int64 { return 0 }
			case "nil":
				s.params.AdditionalProbeDemand = nil
			case "disabled":
				s.enabled = false
			case "media-mode":
				s.params.Config.ProbeMode = ProbeModeMedia
			}
			s.handleSignalPeriodicPing(Event{})
			if (estimator.resets > 0) != (scenario == "idle") || s.prober.IsRunning() != (scenario == "idle") {
				t.Fatalf("paused recovery ignored encoder demand or its gates: resets=%d probing=%v", estimator.resets, s.prober.IsRunning())
			}
			if s.state != streamAllocatorStateStable || s.committedChannelCapacity != 100_000 {
				t.Fatal("idle recovery changed track state or assumed more capacity")
			}
		})
	}
}

func TestWithdrawnEncoderDemandStopsOnlyUnneededProbe(t *testing.T) {
	for _, scenario := range []string{"withdrawn", "encoder-needed", "track-needed"} {
		t.Run(scenario, func(t *testing.T) {
			demand := int64(300_000)
			estimator := &encoderDemandBWE{canProbe: true}
			queue := &encoderDemandPacer{}
			recorder := &encoderDemandProbe{clusters: make(chan ccutils.ProbeClusterInfo, 4)}
			s := &StreamAllocator{params: StreamAllocatorParams{
				Config: DefaultStreamAllocatorConfig, BWE: estimator, Pacer: queue, Logger: logger.GetDiscardLogger(),
				AdditionalProbeDemand: func() int64 { return demand },
			}, enabled: true, activeProbeClusterId: ccutils.ProbeClusterIdInvalid,
				committedChannelCapacity: 100_000, lastRTTTime: time.Now()}
			s.prober = ccutils.NewProber(ccutils.ProberParams{Listener: recorder, Logger: s.params.Logger})
			defer s.prober.Reset(ccutils.ProbeClusterInfoInvalid)
			s.handleSignalPeriodicPing(Event{})
			select {
			case info := <-recorder.clusters:
				s.handleSignalProbeClusterSwitch(Event{probeClusterInfo: info})
			case <-time.After(time.Second):
				t.Fatal("encoder-demand probe did not start")
			}
			if scenario != "encoder-needed" {
				demand = 0
			}
			if scenario == "track-needed" {
				s.state = streamAllocatorStateDeficient
			}
			s.handleSignalPeriodicPing(Event{})
			if s.prober.IsRunning() != (scenario != "withdrawn") || (queue.ends > 0) != (scenario == "withdrawn") {
				t.Fatalf("probe lifetime does not follow remaining demand: running=%v ends=%d", s.prober.IsRunning(), queue.ends)
			}
			if estimator.resets != 0 || s.committedChannelCapacity != 100_000 {
				t.Fatal("probe retirement reset BWE or invented capacity")
			}
		})
	}
}

type encoderDemandBWE struct {
	bwe.BWE
	canProbe   bool
	congestion bwe.CongestionState
	resets     int
}

func (e *encoderDemandBWE) Reset() {
	e.resets++
	e.congestion = bwe.CongestionStateNone
	e.canProbe = true
}
func (e *encoderDemandBWE) CongestionState() bwe.CongestionState { return e.congestion }
func (e *encoderDemandBWE) CanProbe() bool                       { return e.canProbe }
func (*encoderDemandBWE) ProbeDuration() time.Duration           { return 200 * time.Millisecond }
func (e *encoderDemandBWE) ProbeClusterStarting(ccutils.ProbeClusterInfo) {
	e.canProbe = false
}
func (*encoderDemandBWE) ProbeClusterIsGoalReached() bool { return false }
func (*encoderDemandBWE) ProbeClusterFinalize() (ccutils.ProbeSignal, int64, bool) {
	return ccutils.ProbeSignalInconclusive, 0, false
}
func (*encoderDemandBWE) ProbeClusterDone(ccutils.ProbeClusterInfo) {}

type encoderDemandPacer struct {
	pacer.Pacer
	idle time.Duration
	info ccutils.ProbeClusterInfo
	ends int
}

func (p *encoderDemandPacer) TimeSinceLastSentPacket() time.Duration          { return p.idle }
func (p *encoderDemandPacer) StartProbeCluster(info ccutils.ProbeClusterInfo) { p.info = info }
func (p *encoderDemandPacer) EndProbeCluster(ccutils.ProbeClusterId) ccutils.ProbeClusterInfo {
	p.ends++
	return p.info
}

type encoderDemandProbe struct {
	clusters chan ccutils.ProbeClusterInfo
}

func (p *encoderDemandProbe) OnProbeClusterSwitch(info ccutils.ProbeClusterInfo) {
	p.clusters <- info
}
func (*encoderDemandProbe) OnSendProbe(int) {}

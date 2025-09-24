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

package streamallocator

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/bwe"
	"github.com/livekit/livekit-server/pkg/sfu/ccutils"
	"github.com/livekit/livekit-server/pkg/sfu/pacer"
	"github.com/livekit/livekit-server/pkg/utils"
)

const (
	cChannelCapacityInfinity = 100 * 1000 * 1000 // 100 Mbps

	cPriorityMin                = uint8(1)
	cPriorityMax                = uint8(255)
	cPriorityDefaultScreenshare = cPriorityMax
	cPriorityDefaultVideo       = cPriorityMin

	cFlagAllowOvershootWhileOptimal              = true
	cFlagAllowOvershootWhileDeficient            = false
	cFlagAllowOvershootExemptTrackWhileDeficient = true
	cFlagAllowOvershootInProbe                   = true
	cFlagAllowOvershootInCatchup                 = false
	cFlagAllowOvershootInBoost                   = true

	cRTTPullInterval = 30 * time.Second

	cPingLong  = cRTTPullInterval / 2
	cPingShort = 100 * time.Millisecond
)

// ---------------------------------------------------------------------------

type streamAllocatorState int

const (
	streamAllocatorStateStable streamAllocatorState = iota
	streamAllocatorStateDeficient
)

func (s streamAllocatorState) String() string {
	switch s {
	case streamAllocatorStateStable:
		return "STABLE"
	case streamAllocatorStateDeficient:
		return "DEFICIENT"
	default:
		return fmt.Sprintf("UNKNOWN: %d", int(s))
	}
}

// ---------------------------------------------------------------------------

type streamAllocatorSignal int

const (
	streamAllocatorSignalAllocateTrack streamAllocatorSignal = iota
	streamAllocatorSignalAllocateAllTracks
	streamAllocatorSignalAdjustState
	streamAllocatorSignalEstimate
	streamAllocatorSignalFeedback
	streamAllocatorSignalPeriodicPing
	streamAllocatorSignalProbeClusterSwitch
	streamAllocatorSignalSendProbe
	streamAllocatorSignalPacerProbeObserverClusterComplete
	streamAllocatorSignalResume
	streamAllocatorSignalSetAllowPause
	streamAllocatorSignalSetChannelCapacity
	streamAllocatorSignalCongestionStateChange
)

func (s streamAllocatorSignal) String() string {
	switch s {
	case streamAllocatorSignalAllocateTrack:
		return "ALLOCATE_TRACK"
	case streamAllocatorSignalAllocateAllTracks:
		return "ALLOCATE_ALL_TRACKS"
	case streamAllocatorSignalAdjustState:
		return "ADJUST_STATE"
	case streamAllocatorSignalEstimate:
		return "ESTIMATE"
	case streamAllocatorSignalFeedback:
		return "FEEDBACK"
	case streamAllocatorSignalPeriodicPing:
		return "PERIODIC_PING"
	case streamAllocatorSignalProbeClusterSwitch:
		return "PROBE_CLUSTER_SWITCH"
	case streamAllocatorSignalSendProbe:
		return "SEND_PROBE"
	case streamAllocatorSignalPacerProbeObserverClusterComplete:
		return "PACER_PROBE_OBSERVER_CLUSTER_COMPLETE"
	case streamAllocatorSignalResume:
		return "RESUME"
	case streamAllocatorSignalSetAllowPause:
		return "SET_ALLOW_PAUSE"
	case streamAllocatorSignalSetChannelCapacity:
		return "SET_CHANNEL_CAPACITY"
	case streamAllocatorSignalCongestionStateChange:
		return "CONGESTION_STATE_CHANGE"
	default:
		return fmt.Sprintf("%d", int(s))
	}
}

// ---------------------------------------------------------------------------

type Event struct {
	*StreamAllocator
	Signal  streamAllocatorSignal
	TrackID livekit.TrackID
	Data    interface{}
}

func (e Event) String() string {
	return fmt.Sprintf("StreamAllocator:Event{signal: %s, trackID: %s, data: %+v}", e.Signal, e.TrackID, e.Data)
}

// ---------------------------------------------------------------------------

type (
	ProbeMode string
)

const (
	ProbeModePadding ProbeMode = "padding"
	ProbeModeMedia   ProbeMode = "media"
)

type StreamAllocatorConfig struct {
	MinChannelCapacity               int64 `yaml:"min_channel_capacity,omitempty"`
	DisableEstimationUnmanagedTracks bool  `yaml:"disable_etimation_unmanaged_tracks,omitempty"`

	ProbeMode       ProbeMode `yaml:"probe_mode,omitempty"`
	ProbeOveragePct int64     `yaml:"probe_overage_pct,omitempty"`
	ProbeMinBps     int64     `yaml:"probe_min_bps,omitempty"`

	PausedMinWait time.Duration `yaml:"paused_min_wait,omitempty"`
}

var (
	DefaultStreamAllocatorConfig = StreamAllocatorConfig{
		MinChannelCapacity:               0,
		DisableEstimationUnmanagedTracks: false,

		ProbeMode:       ProbeModePadding,
		ProbeOveragePct: 120,
		ProbeMinBps:     200_000,

		PausedMinWait: 5 * time.Second,
	}
)

// ---------------------------------------------------------------------------

type StreamAllocatorParams struct {
	Config    StreamAllocatorConfig
	BWE       bwe.BWE
	Pacer     pacer.Pacer
	RTTGetter func() (float64, bool)
	Logger    logger.Logger
}

type StreamAllocator struct {
	params StreamAllocatorParams

	onStreamStateChange func(update *StreamStateUpdate) error

	sendSideBWEInterceptor cc.BandwidthEstimator

	enabled    bool
	allowPause bool

	committedChannelCapacity  int64
	overriddenChannelCapacity int64

	prober *ccutils.Prober

	videoTracksMu        sync.RWMutex
	videoTracks          map[livekit.TrackID]*Track
	isAllocateAllPending bool
	rembTrackingSSRC     uint32

	state streamAllocatorState

	activeProbeClusterId   ccutils.ProbeClusterId
	activeProbeGoalReached bool
	activeProbeCongesting  bool

	eventsQueue *utils.TypedOpsQueue[Event]

	lastRTTTime time.Time

	pingGeneration atomic.Uint32

	isStopped atomic.Bool
}

func NewStreamAllocator(params StreamAllocatorParams, enabled bool, allowPause bool) *StreamAllocator {
	s := &StreamAllocator{
		params:               params,
		enabled:              enabled,
		allowPause:           allowPause,
		videoTracks:          make(map[livekit.TrackID]*Track),
		state:                streamAllocatorStateStable,
		activeProbeClusterId: ccutils.ProbeClusterIdInvalid,
		eventsQueue: utils.NewTypedOpsQueue[Event](utils.OpsQueueParams{
			Name:    "stream-allocator",
			MinSize: 64,
			Logger:  params.Logger,
		}),
		lastRTTTime: time.Now().Add(-cRTTPullInterval),
	}

	s.prober = ccutils.NewProber(ccutils.ProberParams{
		Listener: s,
		Logger:   params.Logger,
	})

	s.params.BWE.SetBWEListener(s)
	s.params.Pacer.SetPacerProbeObserverListener(s)

	return s
}

func (s *StreamAllocator) Start() {
	s.eventsQueue.Start()
	go s.ping(s.pingGeneration.Inc(), cPingLong)
}

func (s *StreamAllocator) Stop() {
	if s.isStopped.Swap(true) {
		return
	}

	// wait for eventsQueue to be done
	<-s.eventsQueue.Stop()

	s.maybeStopProbe()
}

func (s *StreamAllocator) OnStreamStateChange(f func(update *StreamStateUpdate) error) {
	s.onStreamStateChange = f
}

func (s *StreamAllocator) SetSendSideBWEInterceptor(sendSideBWEInterceptor cc.BandwidthEstimator) {
	if sendSideBWEInterceptor != nil {
		sendSideBWEInterceptor.OnTargetBitrateChange(s.onTargetBitrateChange)
	}
	s.sendSideBWEInterceptor = sendSideBWEInterceptor
}

type AddTrackParams struct {
	Source         livekit.TrackSource
	Priority       uint8
	IsMultiLayered bool
	PublisherID    livekit.ParticipantID
}

func (s *StreamAllocator) AddTrack(downTrack *sfu.DownTrack, params AddTrackParams) {
	if downTrack.Kind() != webrtc.RTPCodecTypeVideo {
		return
	}

	track := NewTrack(downTrack, params.Source, params.IsMultiLayered, params.PublisherID, s.params.Logger)
	track.SetPriority(params.Priority)

	trackID := livekit.TrackID(downTrack.ID())
	s.videoTracksMu.Lock()
	oldTrack := s.videoTracks[trackID]
	s.videoTracks[trackID] = track
	s.videoTracksMu.Unlock()

	if oldTrack != nil {
		oldTrack.DownTrack().SetStreamAllocatorListener(nil)
	}

	downTrack.SetStreamAllocatorListener(s)
	downTrack.SetProbeClusterId(s.activeProbeClusterId)

	s.maybePostEventAllocateTrack(downTrack)
}

func (s *StreamAllocator) RemoveTrack(downTrack *sfu.DownTrack) {
	s.videoTracksMu.Lock()
	if existing := s.videoTracks[livekit.TrackID(downTrack.ID())]; existing != nil && existing.DownTrack() == downTrack {
		delete(s.videoTracks, livekit.TrackID(downTrack.ID()))
	}
	s.videoTracksMu.Unlock()

	// STREAM-ALLOCATOR-TODO: use any saved bandwidth to re-distribute
	s.postEvent(Event{
		Signal: streamAllocatorSignalAdjustState,
	})
}

func (s *StreamAllocator) SetTrackPriority(downTrack *sfu.DownTrack, priority uint8) {
	s.videoTracksMu.Lock()
	if track := s.videoTracks[livekit.TrackID(downTrack.ID())]; track != nil {
		changed := track.SetPriority(priority)
		if changed && !s.isAllocateAllPending {
			// do a full allocation on a track priority change to keep it simple
			s.isAllocateAllPending = true
			s.postEvent(Event{
				Signal: streamAllocatorSignalAllocateAllTracks,
			})
		}
	}
	s.videoTracksMu.Unlock()
}

func (s *StreamAllocator) SetAllowPause(allowPause bool) {
	s.postEvent(Event{
		Signal: streamAllocatorSignalSetAllowPause,
		Data:   allowPause,
	})
}

func (s *StreamAllocator) SetChannelCapacity(channelCapacity int64) {
	s.postEvent(Event{
		Signal: streamAllocatorSignalSetChannelCapacity,
		Data:   channelCapacity,
	})
}

// called when a new REMB is received (receive side bandwidth estimation)
func (s *StreamAllocator) OnREMB(downTrack *sfu.DownTrack, remb *rtcp.ReceiverEstimatedMaximumBitrate) {
	//
	// Channel capacity is estimated at a peer connection level. All down tracks
	// in the peer connection will end up calling this for a REMB report with
	// the same estimated channel capacity. Use a tracking SSRC to lock onto to
	// one report. As SSRCs can be dropped over time, update tracking SSRC as needed
	//
	// A couple of things to keep in mind
	//   - REMB reports could be sent gratuitously as a way of providing
	//     periodic feedback, i.e. even if the estimated capacity does not
	//     change, there could be REMB packets on the wire. Those gratuitous
	//     REMBs should not trigger anything bad.
	//   - As each down track will issue this callback for the same REMB packet
	//     from the wire, theoretically it is possible that one down track's
	//     callback from previous REMB comes after another down track's callback
	//     from the new REMB. REMBs could fire very quickly especially when
	//     the network is entering congestion.
	// STREAM-ALLOCATOR-TODO-START
	// Need to check if the same SSRC reports can somehow race, i.e. does pion send
	// RTCP dispatch for same SSRC on different threads? If not, the tracking SSRC
	// should prevent racing
	// STREAM-ALLOCATOR-TODO-END
	//

	// if there are no video tracks, ignore any straggler REMB
	s.videoTracksMu.Lock()
	if len(s.videoTracks) == 0 {
		s.videoTracksMu.Unlock()
		return
	}

	downTrackSSRC := uint32(0)
	downTrackSSRCRTX := uint32(0)
	track := s.videoTracks[livekit.TrackID(downTrack.ID())]
	if track != nil {
		downTrackSSRC = track.DownTrack().SSRC()
		downTrackSSRCRTX = track.DownTrack().SSRCRTX()
	}

	found := false
	for _, ssrc := range remb.SSRCs {
		if ssrc == s.rembTrackingSSRC {
			found = true
			break
		}
	}
	if !found {
		if len(remb.SSRCs) == 0 {
			s.params.Logger.Warnw("stream allocator: no SSRC to track REMB", nil)
			s.videoTracksMu.Unlock()
			return
		}

		// try to lock to track which is sending this update
		for _, ssrc := range remb.SSRCs {
			if ssrc == 0 {
				continue
			}

			if ssrc == downTrackSSRC {
				s.rembTrackingSSRC = downTrackSSRC
				found = true
				break
			}
			if ssrc == downTrackSSRCRTX {
				s.rembTrackingSSRC = downTrackSSRCRTX
				found = true
				break
			}
		}

		if !found {
			s.rembTrackingSSRC = remb.SSRCs[0]
		}
	}

	if s.rembTrackingSSRC == 0 || (s.rembTrackingSSRC != downTrackSSRC && s.rembTrackingSSRC != downTrackSSRCRTX) {
		s.videoTracksMu.Unlock()
		return
	}
	s.videoTracksMu.Unlock()

	s.postEvent(Event{
		Signal: streamAllocatorSignalEstimate,
		Data:   int64(remb.Bitrate),
	})
}

// called when a new transport-cc feedback is received
func (s *StreamAllocator) OnTransportCCFeedback(downTrack *sfu.DownTrack, fb *rtcp.TransportLayerCC) {
	s.postEvent(Event{
		Signal: streamAllocatorSignalFeedback,
		Data:   fb,
	})
}

// called when target bitrate changes (send side bandwidth estimation)
func (s *StreamAllocator) onTargetBitrateChange(bitrate int) {
	s.postEvent(Event{
		Signal: streamAllocatorSignalEstimate,
		Data:   int64(bitrate),
	})
}

// called when congestion state changes (send side bandwidth estimation)
type congestionStateChangeData struct {
	fromState                         bwe.CongestionState
	toState                           bwe.CongestionState
	estimatedAvailableChannelCapacity int64
}

// BWEListener implementation
func (s *StreamAllocator) OnCongestionStateChange(fromState bwe.CongestionState, toState bwe.CongestionState, estimatedAvailableChannelCapacity int64) {
	s.postEvent(Event{
		Signal: streamAllocatorSignalCongestionStateChange,
		Data:   congestionStateChangeData{fromState, toState, estimatedAvailableChannelCapacity},
	})
}

// called when feeding track's layer availability changes
func (s *StreamAllocator) OnAvailableLayersChanged(downTrack *sfu.DownTrack) {
	s.maybePostEventAllocateTrack(downTrack)
}

// called when feeding track's bitrate measurement of any layer is available
func (s *StreamAllocator) OnBitrateAvailabilityChanged(downTrack *sfu.DownTrack) {
	s.maybePostEventAllocateTrack(downTrack)
}

// called when feeding track's max published spatial layer changes
func (s *StreamAllocator) OnMaxPublishedSpatialChanged(downTrack *sfu.DownTrack) {
	s.maybePostEventAllocateTrack(downTrack)
}

// called when feeding track's max published temporal layer changes
func (s *StreamAllocator) OnMaxPublishedTemporalChanged(downTrack *sfu.DownTrack) {
	s.maybePostEventAllocateTrack(downTrack)
}

// called when subscription settings changes (muting/unmuting of track)
func (s *StreamAllocator) OnSubscriptionChanged(downTrack *sfu.DownTrack) {
	s.maybePostEventAllocateTrack(downTrack)
}

// called when subscribed layer changes (limiting max layer)
func (s *StreamAllocator) OnSubscribedLayerChanged(downTrack *sfu.DownTrack, layer buffer.VideoLayer) {
	shouldPost := false
	s.videoTracksMu.Lock()
	if track := s.videoTracks[livekit.TrackID(downTrack.ID())]; track != nil {
		if track.SetMaxLayer(layer) && track.SetDirty(true) {
			shouldPost = true
		}
	}
	s.videoTracksMu.Unlock()

	if shouldPost {
		s.postEvent(Event{
			Signal:  streamAllocatorSignalAllocateTrack,
			TrackID: livekit.TrackID(downTrack.ID()),
		})
	}
}

// called when forwarder resumes a track
func (s *StreamAllocator) OnResume(downTrack *sfu.DownTrack) {
	s.postEvent(Event{
		Signal:  streamAllocatorSignalResume,
		TrackID: livekit.TrackID(downTrack.ID()),
	})
}

// called when probe cluster changes
func (s *StreamAllocator) OnProbeClusterSwitch(pci ccutils.ProbeClusterInfo) {
	s.postEvent(Event{
		Signal: streamAllocatorSignalProbeClusterSwitch,
		Data:   pci,
	})
}

// called when prober wants to send packet(s)
func (s *StreamAllocator) OnSendProbe(bytesToSend int) {
	s.postEvent(Event{
		Signal: streamAllocatorSignalSendProbe,
		Data:   bytesToSend,
	})
}

// called when pacer probe observer observes a cluster completion
func (s *StreamAllocator) OnPacerProbeObserverClusterComplete(probeClusterId ccutils.ProbeClusterId) {
	s.postEvent(Event{
		Signal: streamAllocatorSignalPacerProbeObserverClusterComplete,
		Data:   probeClusterId,
	})
}

// called to check if track should participate in BWE
func (s *StreamAllocator) IsBWEEnabled(downTrack *sfu.DownTrack) bool {
	if !s.params.Config.DisableEstimationUnmanagedTracks {
		return true
	}

	s.videoTracksMu.Lock()
	defer s.videoTracksMu.Unlock()

	if track := s.videoTracks[livekit.TrackID(downTrack.ID())]; track != nil {
		return track.IsManaged()
	}

	return true
}

func (s *StreamAllocator) BWEType() bwe.BWEType {
	return s.params.BWE.Type()
}

// called to check if track subscription mute can be applied
func (s *StreamAllocator) IsSubscribeMutable(downTrack *sfu.DownTrack) bool {
	s.videoTracksMu.Lock()
	defer s.videoTracksMu.Unlock()

	if track := s.videoTracks[livekit.TrackID(downTrack.ID())]; track != nil {
		return track.IsSubscribeMutable()
	}

	return true
}

func (s *StreamAllocator) maybePostEventAllocateTrack(downTrack *sfu.DownTrack) {
	shouldPost := false
	s.videoTracksMu.Lock()
	if track := s.videoTracks[livekit.TrackID(downTrack.ID())]; track != nil {
		shouldPost = track.SetDirty(true)
	}
	s.videoTracksMu.Unlock()

	if shouldPost {
		s.postEvent(Event{
			Signal:  streamAllocatorSignalAllocateTrack,
			TrackID: livekit.TrackID(downTrack.ID()),
		})
	}
}

func (s *StreamAllocator) ping(pingGeneration uint32, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		<-ticker.C
		if s.isStopped.Load() || (pingGeneration != s.pingGeneration.Load()) {
			return
		}

		s.postEvent(Event{
			Signal: streamAllocatorSignalPeriodicPing,
		})
	}
}

func (s *StreamAllocator) postEvent(event Event) {
	event.StreamAllocator = s
	s.eventsQueue.Enqueue(func(event Event) {
		switch event.Signal {
		case streamAllocatorSignalAllocateTrack:
			event.handleSignalAllocateTrack(event)
		case streamAllocatorSignalAllocateAllTracks:
			event.handleSignalAllocateAllTracks(event)
		case streamAllocatorSignalAdjustState:
			event.handleSignalAdjustState(event)
		case streamAllocatorSignalEstimate:
			event.handleSignalEstimate(event)
		case streamAllocatorSignalFeedback:
			event.handleSignalFeedback(event)
		case streamAllocatorSignalPeriodicPing:
			event.handleSignalPeriodicPing(event)
		case streamAllocatorSignalProbeClusterSwitch:
			event.handleSignalProbeClusterSwitch(event)
		case streamAllocatorSignalSendProbe:
			event.handleSignalSendProbe(event)
		case streamAllocatorSignalPacerProbeObserverClusterComplete:
			event.handleSignalPacerProbeObserverClusterComplete(event)
		case streamAllocatorSignalResume:
			event.handleSignalResume(event)
		case streamAllocatorSignalSetAllowPause:
			event.handleSignalSetAllowPause(event)
		case streamAllocatorSignalSetChannelCapacity:
			event.handleSignalSetChannelCapacity(event)
		case streamAllocatorSignalCongestionStateChange:
			s.handleSignalCongestionStateChange(event)
		}
	}, event)
}

func (s *StreamAllocator) handleSignalAllocateTrack(event Event) {
	s.videoTracksMu.Lock()
	track := s.videoTracks[event.TrackID]
	if track != nil {
		track.SetDirty(false)
	}
	s.videoTracksMu.Unlock()

	if track != nil {
		s.allocateTrack(track)
	}
}

func (s *StreamAllocator) handleSignalAllocateAllTracks(Event) {
	s.videoTracksMu.Lock()
	s.isAllocateAllPending = false
	s.videoTracksMu.Unlock()

	if s.state == streamAllocatorStateDeficient {
		s.allocateAllTracks()
	}
}

func (s *StreamAllocator) handleSignalAdjustState(Event) {
	s.adjustState()
}

func (s *StreamAllocator) handleSignalEstimate(event Event) {
	receivedEstimate := event.Data.(int64)

	// always update NACKs
	packetDelta, repeatedNackDelta := s.getNackDelta()

	s.params.BWE.HandleREMB(
		receivedEstimate,
		s.getExpectedBandwidthUsage(),
		packetDelta,
		repeatedNackDelta,
	)
}

func (s *StreamAllocator) handleSignalFeedback(event Event) {
	fb := event.Data.(*rtcp.TransportLayerCC)
	if s.sendSideBWEInterceptor != nil {
		s.sendSideBWEInterceptor.WriteRTCP([]rtcp.Packet{fb}, nil)
	}

	s.params.BWE.HandleTWCCFeedback(fb)
}

func (s *StreamAllocator) handleSignalPeriodicPing(Event) {
	// if pause is allowed, there may be no packets sent and BWE could be in congested state,
	// reset BWE if that persists for a while
	if s.allowPause && s.state == streamAllocatorStateDeficient && s.params.BWE.CongestionState() != bwe.CongestionStateNone && s.params.Pacer.TimeSinceLastSentPacket() > s.params.Config.PausedMinWait {
		s.params.Logger.Infow("stream allocator: resetting bwe to enable probing")
		s.maybeStopProbe()
		s.params.BWE.Reset()

		// as BWE is reset, there is no finalizing for active cluster, so reset active cluster id
		s.activeProbeClusterId = ccutils.ProbeClusterIdInvalid
	}

	if s.activeProbeClusterId != ccutils.ProbeClusterIdInvalid {
		if !s.activeProbeCongesting && !s.activeProbeGoalReached && s.params.BWE.ProbeClusterIsGoalReached() {
			s.params.Logger.Debugw(
				"stream allocator: probe goal reached",
				"activeProbeClusterId", s.activeProbeClusterId,
			)
			s.activeProbeGoalReached = true
			s.maybeStopProbe()
		}

		// finalize any probe that may have finished/aborted
		if probeSignal, channelCapacity, isFinalized := s.params.BWE.ProbeClusterFinalize(); isFinalized {
			s.params.Logger.Debugw(
				"stream allocator: probe result",
				"activeProbeClusterId", s.activeProbeClusterId,
				"probeSignal", probeSignal,
				"channelCapacity", channelCapacity,
			)

			s.activeProbeClusterId = ccutils.ProbeClusterIdInvalid

			if probeSignal != ccutils.ProbeSignalCongesting {
				if channelCapacity > s.committedChannelCapacity {
					s.committedChannelCapacity = channelCapacity
				}

				s.maybeBoostDeficientTracks()
			}
		}
	}

	// probe if necessary and timing is right
	if s.state == streamAllocatorStateDeficient {
		s.maybeProbe()
	}

	if time.Since(s.lastRTTTime) > cRTTPullInterval {
		s.lastRTTTime = time.Now()

		if s.params.RTTGetter != nil {
			if rtt, ok := s.params.RTTGetter(); ok {
				s.params.BWE.UpdateRTT(rtt)
			}
		}
	}
}

func (s *StreamAllocator) handleSignalProbeClusterSwitch(event Event) {
	pci := event.Data.(ccutils.ProbeClusterInfo)
	s.activeProbeClusterId = pci.Id
	s.activeProbeGoalReached = false
	s.activeProbeCongesting = false

	s.params.BWE.ProbeClusterStarting(pci)

	s.params.Pacer.StartProbeCluster(pci)

	for _, t := range s.getTracks() {
		t.DownTrack().SetProbeClusterId(pci.Id)
	}
}

func (s *StreamAllocator) handleSignalSendProbe(event Event) {
	bytesToSend := event.Data.(int)
	if bytesToSend <= 0 {
		return
	}

	bytesSent := 0
	for _, track := range s.getTracks() {
		sent := track.WriteProbePackets(bytesToSend)
		bytesSent += sent
		bytesToSend -= sent
		if bytesToSend <= 0 {
			break
		}
	}

	s.prober.ProbesSent(bytesSent)
}

func (s *StreamAllocator) handleSignalPacerProbeObserverClusterComplete(event Event) {
	probeClusterId, _ := event.Data.(ccutils.ProbeClusterId)
	pci := s.params.Pacer.EndProbeCluster(probeClusterId)

	for _, t := range s.getTracks() {
		t.DownTrack().SwapProbeClusterId(pci.Id, ccutils.ProbeClusterIdInvalid)
	}

	s.params.BWE.ProbeClusterDone(pci)
	s.prober.ClusterDone(pci)
}

func (s *StreamAllocator) handleSignalResume(event Event) {
	s.videoTracksMu.Lock()
	track := s.videoTracks[event.TrackID]
	updated := track != nil && track.SetStreamState(StreamStateActive)
	s.videoTracksMu.Unlock()

	if updated {
		update := NewStreamStateUpdate()
		update.HandleStreamingChange(track, StreamStateActive)
		s.maybeSendUpdate(update)
	}
}

func (s *StreamAllocator) handleSignalSetAllowPause(event Event) {
	s.allowPause = event.Data.(bool)
}

func (s *StreamAllocator) handleSignalSetChannelCapacity(event Event) {
	s.overriddenChannelCapacity = event.Data.(int64)
	if s.overriddenChannelCapacity > 0 {
		s.params.Logger.Infow("allocating on override channel capacity", "override", s.overriddenChannelCapacity)
		s.allocateAllTracks()
	} else {
		s.params.Logger.Infow("clearing override channel capacity")
	}
}

func (s *StreamAllocator) handleSignalCongestionStateChange(event Event) {
	cscd := event.Data.(congestionStateChangeData)
	if cscd.toState != bwe.CongestionStateNone {
		// end/abort any running probe if channel is not clear
		s.maybeStopProbe()
	}

	// some tracks may have been held at sub-optimal allocation
	// during early warning hold (if there was one)
	if isHoldableCongestionState(cscd.fromState) && cscd.toState == bwe.CongestionStateNone && s.state == streamAllocatorStateStable {
		update := NewStreamStateUpdate()
		for _, track := range s.getTracks() {
			allocation := track.AllocateOptimal(cFlagAllowOvershootWhileOptimal, false)
			updateStreamStateChange(track, allocation, update)
		}
		s.maybeSendUpdate(update)
	}

	if cscd.toState == bwe.CongestionStateCongested {
		if s.activeProbeClusterId != ccutils.ProbeClusterIdInvalid {
			if !s.activeProbeCongesting {
				s.activeProbeCongesting = true
				s.params.Logger.Debugw(
					"stream allocator: channel congestion detected, not updating channel capacity in active probe",
					"old(bps)", s.committedChannelCapacity,
					"new(bps)", cscd.estimatedAvailableChannelCapacity,
					"expectedUsage(bps)", s.getExpectedBandwidthUsage(),
				)
			}
		} else {
			s.params.Logger.Debugw(
				"stream allocator: channel congestion detected, updating channel capacity",
				"old(bps)", s.committedChannelCapacity,
				"new(bps)", cscd.estimatedAvailableChannelCapacity,
				"expectedUsage(bps)", s.getExpectedBandwidthUsage(),
			)
			s.committedChannelCapacity = cscd.estimatedAvailableChannelCapacity

			s.allocateAllTracks()
		}
	}
}

func (s *StreamAllocator) setState(state streamAllocatorState) {
	if s.state == state {
		return
	}

	s.params.Logger.Infow("stream allocator: state change", "from", s.state, "to", state)
	s.state = state

	// restart everything when state is STABLE
	if state == streamAllocatorStateStable {
		s.maybeStopProbe()

		s.params.BWE.Reset()

		s.activeProbeClusterId = ccutils.ProbeClusterIdInvalid
		go s.ping(s.pingGeneration.Inc(), cPingLong)
	} else {
		go s.ping(s.pingGeneration.Inc(), cPingShort)
	}
}

func (s *StreamAllocator) adjustState() {
	for _, track := range s.getTracks() {
		if track.IsDeficient() {
			s.setState(streamAllocatorStateDeficient)
			return
		}
	}

	s.setState(streamAllocatorStateStable)
}

func (s *StreamAllocator) allocateTrack(track *Track) {
	// end/abort any probe that may be running when a track specific change needs allocation
	s.maybeStopProbe()

	// if not deficient, free pass allocate track
	bweCongestionState := s.params.BWE.CongestionState()
	if !s.enabled || (s.state == streamAllocatorStateStable && !isDeficientCongestionState(bweCongestionState)) || !track.IsManaged() {
		update := NewStreamStateUpdate()
		allocation := track.AllocateOptimal(cFlagAllowOvershootWhileOptimal, isHoldableCongestionState(bweCongestionState))
		updateStreamStateChange(track, allocation, update)
		s.maybeSendUpdate(update)
		return
	}

	//
	// In DEFICIENT state,
	//   Two possibilities
	//   1. Available headroom is enough to accommodate track that needs change.
	//      Note that the track could be muted, hence stopping.
	//   2. Have to steal bits from other tracks currently streaming.
	//
	//   For both cases, do
	//     a. Find cooperative transition from track that needs allocation.
	//     b. If track is giving back bits, apply the transition and use bits given
	//        back to boost any deficient track(s).
	//
	//   If track needs more bits, i.e. upward transition (may need resume or higher layer subscription),
	//     a. Try to allocate using existing headroom. This can be tried to get the best
	//        possible fit for the available headroom.
	//     b. If there is not enough headroom to allocate anything, ask for best offer from
	//        other tracks that are currently streaming and try to use it. This is done only if the
	//        track needing change is not currently streaming, i. e. it has to be resumed.
	//
	track.ProvisionalAllocatePrepare()
	transition := track.ProvisionalAllocateGetCooperativeTransition(cFlagAllowOvershootWhileDeficient)

	// downgrade, giving back bits
	if transition.From.GreaterThan(transition.To) {
		allocation := track.ProvisionalAllocateCommit()

		update := NewStreamStateUpdate()
		updateStreamStateChange(track, allocation, update)
		s.maybeSendUpdate(update)

		s.adjustState()

		// Use the bits given back to boost deficient track(s).
		// Note layer downgrade may actually have positive delta (i.e. consume more bits)
		// because of when the measurement is done. But, only available headroom after
		// applying the transition will be used to boost deficient track(s).
		s.maybeBoostDeficientTracks()
		return
	}

	// a no-op transition
	if transition.From == transition.To {
		return
	}

	// this track is currently not streaming and needs bits to start OR streaming at some layer and wants more bits.
	// NOTE: With co-operative transition, tracks should not be asking for more if already streaming, but handle that case any way.
	// first try an allocation using available headroom, current consumption of this track is discounted to calculate headroom.
	availableChannelCapacity := s.getAvailableHeadroomWithoutTracks(false, []*Track{track})
	if availableChannelCapacity > 0 {
		track.ProvisionalAllocateReset() // to reset allocation from co-operative transition above and try fresh

		bestLayer := buffer.InvalidLayer

	alloc_loop:
		for spatial := int32(0); spatial <= buffer.DefaultMaxLayerSpatial; spatial++ {
			for temporal := int32(0); temporal <= buffer.DefaultMaxLayerTemporal; temporal++ {
				layer := buffer.VideoLayer{
					Spatial:  spatial,
					Temporal: temporal,
				}

				isCandidate, usedChannelCapacity := track.ProvisionalAllocate(
					availableChannelCapacity,
					layer,
					s.allowPause,
					cFlagAllowOvershootWhileDeficient,
				)
				if availableChannelCapacity < usedChannelCapacity {
					break alloc_loop
				}

				if isCandidate {
					bestLayer = layer
				}
			}
		}

		if bestLayer.IsValid() {
			if bestLayer.GreaterThan(transition.From) {
				// found layer that can fit in available headroom, take it if it is better than existing
				update := NewStreamStateUpdate()
				allocation := track.ProvisionalAllocateCommit()
				updateStreamStateChange(track, allocation, update)
				s.maybeSendUpdate(update)
			}

			s.adjustState()
			return
		}

		track.ProvisionalAllocateReset()
		transition = track.ProvisionalAllocateGetCooperativeTransition(cFlagAllowOvershootWhileDeficient) // get transition again to reset above allocation attempt using available headroom
	}

	// if there is not enough headroom, try to redistribute starting with tracks that are closest to their desired.
	bandwidthAcquired := int64(0)
	var contributingTracks []*Track

	minDistanceSorted := s.getMinDistanceSorted(track)
	for _, t := range minDistanceSorted {
		t.ProvisionalAllocatePrepare()
	}

	for _, t := range minDistanceSorted {
		tx := t.ProvisionalAllocateGetBestWeightedTransition()
		if tx.BandwidthDelta < 0 {
			contributingTracks = append(contributingTracks, t)

			bandwidthAcquired += -tx.BandwidthDelta
			if bandwidthAcquired >= transition.BandwidthDelta {
				break
			}
		}
	}

	update := NewStreamStateUpdate()
	if bandwidthAcquired >= transition.BandwidthDelta {
		// commit the tracks that contributed
		for _, t := range contributingTracks {
			allocation := t.ProvisionalAllocateCommit()
			updateStreamStateChange(t, allocation, update)
		}

		// STREAM-ALLOCATOR-TODO if got too much extra, can potentially give it to some deficient track
	}

	// commit the track that needs change if enough could be acquired or pause not allowed
	if !s.allowPause || bandwidthAcquired >= transition.BandwidthDelta {
		allocation := track.ProvisionalAllocateCommit()
		updateStreamStateChange(track, allocation, update)
	} else {
		// explicitly pause to ensure stream state update happens if a track coming out of mute cannot be allocated
		allocation := track.Pause()
		updateStreamStateChange(track, allocation, update)
	}

	s.maybeSendUpdate(update)

	s.adjustState()
}

func (s *StreamAllocator) maybeStopProbe() {
	if s.activeProbeClusterId == ccutils.ProbeClusterIdInvalid {
		return
	}

	pci := s.params.Pacer.EndProbeCluster(s.activeProbeClusterId)

	for _, t := range s.getTracks() {
		t.DownTrack().SwapProbeClusterId(pci.Id, ccutils.ProbeClusterIdInvalid)
	}

	s.params.BWE.ProbeClusterDone(pci)
	s.prober.Reset(pci)
}

func (s *StreamAllocator) maybeBoostDeficientTracks() {
	availableChannelCapacity := s.getAvailableHeadroom(false)
	if availableChannelCapacity <= 0 {
		s.params.Logger.Debugw(
			"stream allocator: no available headroom to boost deficient tracks",
			"committedChannelCapacity", s.committedChannelCapacity,
			"availableChannelCapacity", availableChannelCapacity,
			"expectedBandwidthUsage", s.getExpectedBandwidthUsage(),
		)
		return
	}

	update := NewStreamStateUpdate()

	sortedTracks := s.getMaxDistanceSortedDeficient()
boost_loop:
	for {
		for idx, track := range sortedTracks {
			allocation, boosted := track.AllocateNextHigher(availableChannelCapacity, cFlagAllowOvershootInCatchup)
			if !boosted {
				if idx == len(sortedTracks)-1 {
					// all tracks tried
					break boost_loop
				}
				continue
			}

			updateStreamStateChange(track, allocation, update)

			availableChannelCapacity -= allocation.BandwidthDelta
			if availableChannelCapacity <= 0 {
				break boost_loop
			}

			break // sort again below as the track that was just boosted could still be farthest from its desired
		}
		sortedTracks = s.getMaxDistanceSortedDeficient()
		if len(sortedTracks) == 0 {
			break // nothing available to boost
		}
	}

	s.maybeSendUpdate(update)

	s.adjustState()
}

func (s *StreamAllocator) allocateAllTracks() {
	if !s.enabled {
		// nothing else to do when disabled
		return
	}

	//
	// Goals:
	//   1. Stream as many tracks as possible, i.e. no pauses.
	//   2. Try to give fair allocation to all track.
	//
	// Start with the lowest layer and give each track a chance at that layer and keep going up.
	// As long as there is enough bandwidth for tracks to stream at the lowest layer, the first goal is achieved.
	//
	// Tracks that have higher subscribed layer can use any additional available bandwidth. This tried to achieve the second goal.
	//
	// If there is not enough bandwidth even for the lowest layer, tracks at lower priorities will be paused.
	//
	update := NewStreamStateUpdate()

	availableChannelCapacity := s.getAvailableChannelCapacity(true)

	//
	// This pass is to find out if there is any leftover channel capacity after allocating exempt tracks.
	// Exempt tracks are given optimal allocation (i. e. no bandwidth constraint) so that they do not fail allocation.
	//
	videoTracks := s.getTracks()
	for _, track := range videoTracks {
		if track.IsManaged() {
			continue
		}

		allocation := track.AllocateOptimal(cFlagAllowOvershootExemptTrackWhileDeficient, false)
		updateStreamStateChange(track, allocation, update)

		// STREAM-ALLOCATOR-TODO: optimistic allocation before bitrate is available will return 0. How to account for that?
		if !s.params.Config.DisableEstimationUnmanagedTracks {
			availableChannelCapacity -= allocation.BandwidthRequested
		}
	}

	if availableChannelCapacity < 0 {
		availableChannelCapacity = 0
	}
	if availableChannelCapacity == 0 && s.allowPause {
		// nothing left for managed tracks, pause them all
		for _, track := range videoTracks {
			if !track.IsManaged() {
				continue
			}

			allocation := track.Pause()
			updateStreamStateChange(track, allocation, update)
		}
	} else {
		sorted := s.getSorted()
		for _, track := range sorted {
			track.ProvisionalAllocatePrepare()
		}

		for spatial := int32(0); spatial <= buffer.DefaultMaxLayerSpatial; spatial++ {
			for temporal := int32(0); temporal <= buffer.DefaultMaxLayerTemporal; temporal++ {
				layer := buffer.VideoLayer{
					Spatial:  spatial,
					Temporal: temporal,
				}

				for _, track := range sorted {
					_, usedChannelCapacity := track.ProvisionalAllocate(availableChannelCapacity, layer, s.allowPause, cFlagAllowOvershootWhileDeficient)
					availableChannelCapacity -= usedChannelCapacity
					if availableChannelCapacity < 0 {
						availableChannelCapacity = 0
					}
				}
			}
		}

		for _, track := range sorted {
			allocation := track.ProvisionalAllocateCommit()
			updateStreamStateChange(track, allocation, update)
		}
	}

	s.maybeSendUpdate(update)

	s.adjustState()
}

func (s *StreamAllocator) maybeSendUpdate(update *StreamStateUpdate) {
	if update.Empty() {
		return
	}

	// logging individual changes to make it easier for logging systems
	for _, streamState := range update.StreamStates {
		s.params.Logger.Debugw("streamed tracks changed",
			"trackID", streamState.TrackID,
			"state", streamState.State,
		)
	}
	if s.onStreamStateChange != nil {
		err := s.onStreamStateChange(update)
		if err != nil {
			s.params.Logger.Errorw("could not send streamed tracks update", err)
		}
	}
}

func (s *StreamAllocator) getAvailableChannelCapacity(allowOverride bool) int64 {
	availableChannelCapacity := s.committedChannelCapacity
	if s.params.Config.MinChannelCapacity > availableChannelCapacity {
		availableChannelCapacity = s.params.Config.MinChannelCapacity
		s.params.Logger.Debugw(
			"stream allocator: overriding channel capacity with min channel capacity",
			"actual", s.committedChannelCapacity,
			"override", availableChannelCapacity,
		)
	}
	if allowOverride && s.overriddenChannelCapacity > 0 {
		availableChannelCapacity = s.overriddenChannelCapacity
		s.params.Logger.Debugw(
			"stream allocator: overriding channel capacity",
			"actual", s.committedChannelCapacity,
			"override", availableChannelCapacity,
		)
	}

	return availableChannelCapacity
}

func (s *StreamAllocator) getExpectedBandwidthUsage() int64 {
	expected := int64(0)
	for _, track := range s.getTracks() {
		expected += track.BandwidthRequested()
	}

	return expected
}

func (s *StreamAllocator) getExpectedBandwidthUsageWithoutTracks(filteredTracks []*Track) int64 {
	expected := int64(0)
	for _, track := range s.getTracks() {
		filtered := false
		for _, ft := range filteredTracks {
			if ft == track {
				filtered = true
				break
			}
		}
		if !filtered {
			expected += track.BandwidthRequested()
		}
	}

	return expected
}

func (s *StreamAllocator) getAvailableHeadroom(allowOverride bool) int64 {
	return s.getAvailableChannelCapacity(allowOverride) - s.getExpectedBandwidthUsage()
}

func (s *StreamAllocator) getAvailableHeadroomWithoutTracks(allowOverride bool, filteredTracks []*Track) int64 {
	return s.getAvailableChannelCapacity(allowOverride) - s.getExpectedBandwidthUsageWithoutTracks(filteredTracks)
}

func (s *StreamAllocator) getNackDelta() (uint32, uint32) {
	aggPacketDelta := uint32(0)
	aggRepeatedNackDelta := uint32(0)
	for _, track := range s.getTracks() {
		packetDelta, nackDelta := track.GetNackDelta()
		aggPacketDelta += packetDelta
		aggRepeatedNackDelta += nackDelta
	}

	return aggPacketDelta, aggRepeatedNackDelta
}

func (s *StreamAllocator) maybeProbe() {
	if s.overriddenChannelCapacity > 0 {
		// do not probe if channel capacity is overridden
		return
	}

	if !s.params.BWE.CanProbe() {
		return
	}

	switch s.params.Config.ProbeMode {
	case ProbeModeMedia:
		s.maybeProbeWithMedia()
		s.adjustState()
	case ProbeModePadding:
		s.maybeProbeWithPadding()
	}
}

func (s *StreamAllocator) maybeProbeWithMedia() {
	// boost deficient track farthest from desired layer
	for _, track := range s.getMaxDistanceSortedDeficient() {
		allocation, boosted := track.AllocateNextHigher(cChannelCapacityInfinity, cFlagAllowOvershootInBoost)
		if !boosted {
			continue
		}

		update := NewStreamStateUpdate()
		updateStreamStateChange(track, allocation, update)
		s.maybeSendUpdate(update)

		s.params.BWE.Reset()
		break
	}
}

func (s *StreamAllocator) maybeProbeWithPadding() {
	// use deficient track farthest from desired layer to find how much to probe
	for _, track := range s.getMaxDistanceSortedDeficient() {
		transition, available := track.GetNextHigherTransition(cFlagAllowOvershootInProbe)
		if !available || transition.BandwidthDelta < 0 {
			continue
		}

		// overshoot a bit to account for noise (in measurement/estimate etc)
		desiredIncreaseBps := (transition.BandwidthDelta * s.params.Config.ProbeOveragePct) / 100
		if desiredIncreaseBps < s.params.Config.ProbeMinBps {
			desiredIncreaseBps = s.params.Config.ProbeMinBps
		}
		expectedBandwidthUsage := s.getExpectedBandwidthUsage()
		pci := s.prober.AddCluster(
			ccutils.ProbeClusterModeUniform,
			ccutils.ProbeClusterGoal{
				AvailableBandwidthBps: int(s.committedChannelCapacity),
				ExpectedUsageBps:      int(expectedBandwidthUsage),
				DesiredBps:            int(expectedBandwidthUsage + desiredIncreaseBps),
				Duration:              s.params.BWE.ProbeDuration(),
			},
		)
		s.params.Logger.Debugw(
			"stream allocator: adding probe",
			"probeClusterInfo", pci,
		)
		break
	}
}

func (s *StreamAllocator) getTracks() []*Track {
	s.videoTracksMu.RLock()
	tracks := make([]*Track, 0, len(s.videoTracks))
	for _, track := range s.videoTracks {
		tracks = append(tracks, track)
	}
	s.videoTracksMu.RUnlock()

	return tracks
}

func (s *StreamAllocator) getSorted() TrackSorter {
	s.videoTracksMu.RLock()
	var trackSorter TrackSorter
	for _, track := range s.videoTracks {
		if !track.IsManaged() {
			continue
		}

		trackSorter = append(trackSorter, track)
	}
	s.videoTracksMu.RUnlock()

	sort.Sort(trackSorter)

	return trackSorter
}

func (s *StreamAllocator) getMinDistanceSorted(exclude *Track) MinDistanceSorter {
	s.videoTracksMu.RLock()
	var minDistanceSorter MinDistanceSorter
	for _, track := range s.videoTracks {
		if !track.IsManaged() || track == exclude {
			continue
		}

		minDistanceSorter = append(minDistanceSorter, track)
	}
	s.videoTracksMu.RUnlock()

	sort.Sort(minDistanceSorter)

	return minDistanceSorter
}

func (s *StreamAllocator) getMaxDistanceSortedDeficient() MaxDistanceSorter {
	s.videoTracksMu.RLock()
	var maxDistanceSorter MaxDistanceSorter
	for _, track := range s.videoTracks {
		if !track.IsManaged() || !track.IsDeficient() {
			continue
		}

		maxDistanceSorter = append(maxDistanceSorter, track)
	}
	s.videoTracksMu.RUnlock()

	sort.Sort(maxDistanceSorter)

	return maxDistanceSorter
}

// ------------------------------------------------

func updateStreamStateChange(track *Track, allocation sfu.VideoAllocation, update *StreamStateUpdate) {
	updated := false
	streamState := StreamStateInactive
	switch allocation.PauseReason {
	case sfu.VideoPauseReasonMuted:
		fallthrough

	case sfu.VideoPauseReasonPubMuted:
		streamState = StreamStateInactive
		updated = track.SetStreamState(streamState)

	case sfu.VideoPauseReasonBandwidth:
		streamState = StreamStatePaused
		updated = track.SetStreamState(streamState)
	}

	if updated {
		update.HandleStreamingChange(track, streamState)
	}
}

func isHoldableCongestionState(bweCongestionState bwe.CongestionState) bool {
	return bweCongestionState == bwe.CongestionStateEarlyWarning
}

func isDeficientCongestionState(bweCongestionState bwe.CongestionState) bool {
	return bweCongestionState == bwe.CongestionStateCongested
}

// ------------------------------------------------

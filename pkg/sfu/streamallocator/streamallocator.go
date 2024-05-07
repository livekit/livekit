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
	"github.com/pion/webrtc/v3"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/utils"
)

const (
	ChannelCapacityInfinity = 100 * 1000 * 1000 // 100 Mbps

	PriorityMin                = uint8(1)
	PriorityMax                = uint8(255)
	PriorityDefaultScreenshare = PriorityMax
	PriorityDefaultVideo       = PriorityMin

	FlagAllowOvershootWhileOptimal              = true
	FlagAllowOvershootWhileDeficient            = false
	FlagAllowOvershootExemptTrackWhileDeficient = true
	FlagAllowOvershootInProbe                   = true
	FlagAllowOvershootInCatchup                 = false
	FlagAllowOvershootInBoost                   = true
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
	streamAllocatorSignalPeriodicPing
	streamAllocatorSignalSendProbe
	streamAllocatorSignalProbeClusterDone
	streamAllocatorSignalResume
	streamAllocatorSignalSetAllowPause
	streamAllocatorSignalSetChannelCapacity
	// STREAM-ALLOCATOR-DATA streamAllocatorSignalNACK
	// STREAM-ALLOCATOR-DATA streamAllocatorSignalRTCPReceiverReport
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
	case streamAllocatorSignalPeriodicPing:
		return "PERIODIC_PING"
	case streamAllocatorSignalSendProbe:
		return "SEND_PROBE"
	case streamAllocatorSignalProbeClusterDone:
		return "PROBE_CLUSTER_DONE"
	case streamAllocatorSignalResume:
		return "RESUME"
	case streamAllocatorSignalSetAllowPause:
		return "SET_ALLOW_PAUSE"
	case streamAllocatorSignalSetChannelCapacity:
		return "SET_CHANNEL_CAPACITY"
		/* STREAM-ALLOCATOR-DATA
		case streamAllocatorSignalNACK:
			return "NACK"
		case streamAllocatorSignalRTCPReceiverReport:
			return "RTCP_RECEIVER_REPORT"
		*/
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

type StreamAllocatorParams struct {
	Config config.CongestionControlConfig
	Logger logger.Logger
}

type StreamAllocator struct {
	params StreamAllocatorParams

	onStreamStateChange func(update *StreamStateUpdate) error

	bwe cc.BandwidthEstimator

	allowPause bool

	lastReceivedEstimate      int64
	committedChannelCapacity  int64
	overriddenChannelCapacity int64

	probeController *ProbeController

	prober *Prober

	channelObserver *ChannelObserver
	// STREAM-ALLOCATOR-DATA rateMonitor     *RateMonitor

	videoTracksMu        sync.RWMutex
	videoTracks          map[livekit.TrackID]*Track
	isAllocateAllPending bool
	rembTrackingSSRC     uint32

	state streamAllocatorState

	eventsQueue *utils.TypedOpsQueue[Event]

	isStopped atomic.Bool
}

func NewStreamAllocator(params StreamAllocatorParams) *StreamAllocator {
	s := &StreamAllocator{
		params:     params,
		allowPause: params.Config.AllowPause,
		prober: NewProber(ProberParams{
			Logger: params.Logger,
		}),
		// STREAM-ALLOCATOR-DATA rateMonitor: NewRateMonitor(),
		videoTracks: make(map[livekit.TrackID]*Track),
		eventsQueue: utils.NewTypedOpsQueue[Event](utils.OpsQueueParams{
			Name:    "stream-allocator",
			MinSize: 64,
			Logger:  params.Logger,
		}),
	}

	s.probeController = NewProbeController(ProbeControllerParams{
		Config: s.params.Config.ProbeConfig,
		Prober: s.prober,
		Logger: params.Logger,
	})

	s.resetState()

	s.prober.SetProberListener(s)

	return s
}

func (s *StreamAllocator) Start() {
	s.eventsQueue.Start()
	go s.ping()
}

func (s *StreamAllocator) Stop() {
	if s.isStopped.Swap(true) {
		return
	}

	// wait for eventsQueue to be done
	<-s.eventsQueue.Stop()
	s.probeController.StopProbe()
}

func (s *StreamAllocator) OnStreamStateChange(f func(update *StreamStateUpdate) error) {
	s.onStreamStateChange = f
}

func (s *StreamAllocator) SetBandwidthEstimator(bwe cc.BandwidthEstimator) {
	if bwe != nil {
		bwe.OnTargetBitrateChange(s.onTargetBitrateChange)
	}
	s.bwe = bwe
}

type AddTrackParams struct {
	Source      livekit.TrackSource
	Priority    uint8
	IsSimulcast bool
	PublisherID livekit.ParticipantID
}

func (s *StreamAllocator) AddTrack(downTrack *sfu.DownTrack, params AddTrackParams) {
	if downTrack.Kind() != webrtc.RTPCodecTypeVideo {
		return
	}

	track := NewTrack(downTrack, params.Source, params.IsSimulcast, params.PublisherID, s.params.Logger)
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
	if s.prober.IsRunning() {
		// STREAM-ALLOCATOR-TODO: this can be changed to adapt to probe rate
		downTrack.SetStreamAllocatorReportInterval(50 * time.Millisecond)
	}

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

func (s *StreamAllocator) resetState() {
	s.channelObserver = s.newChannelObserverNonProbe()
	s.probeController.Reset()

	s.state = streamAllocatorStateStable
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

	track := s.videoTracks[livekit.TrackID(downTrack.ID())]
	downTrackSSRC := uint32(0)
	if track != nil {
		downTrackSSRC = track.DownTrack().SSRC()
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
		if downTrackSSRC != 0 {
			for _, ssrc := range remb.SSRCs {
				if ssrc == downTrackSSRC {
					s.rembTrackingSSRC = downTrackSSRC
					found = true
					break
				}
			}
		}

		if !found {
			s.rembTrackingSSRC = remb.SSRCs[0]
		}
	}

	if s.rembTrackingSSRC == 0 || s.rembTrackingSSRC != downTrackSSRC {
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
	if s.bwe != nil {
		s.bwe.WriteRTCP([]rtcp.Packet{fb}, nil)
	}
}

// called when target bitrate changes (send side bandwidth estimation)
func (s *StreamAllocator) onTargetBitrateChange(bitrate int) {
	s.postEvent(Event{
		Signal: streamAllocatorSignalEstimate,
		Data:   int64(bitrate),
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

// called by a video DownTrack to report packet send
func (s *StreamAllocator) OnPacketsSent(downTrack *sfu.DownTrack, size int) {
	s.prober.PacketsSent(size)
}

/* STREAM-ALLOCATOR-DATA
// called by a video DownTrack when it processes NACKs
func (s *StreamAllocator) OnNACK(downTrack *sfu.DownTrack, nackInfos []sfu.NackInfo) {
	s.postEvent(Event{
		Signal:  streamAllocatorSignalNACK,
		TrackID: livekit.TrackID(downTrack.ID()),
		Data:    nackInfos,
	})
}

// called by a video DownTrack when it receives an RTCP Receiver Report
// STREAM-ALLOCATOR-TODO: this should probably be done for audio tracks also
func (s *StreamAllocator) OnRTCPReceiverReport(downTrack *sfu.DownTrack, rr rtcp.ReceptionReport) {
	s.postEvent(Event{
		Signal:  streamAllocatorSignalRTCPReceiverReport,
		TrackID: livekit.TrackID(downTrack.ID()),
		Data:    rr,
	})
}
*/

// called when prober wants to send packet(s)
func (s *StreamAllocator) OnSendProbe(bytesToSend int) {
	s.postEvent(Event{
		Signal: streamAllocatorSignalSendProbe,
		Data:   bytesToSend,
	})
}

// called when prober finishes a probe cluster, could be called when prober is reset which stops an active cluster
func (s *StreamAllocator) OnProbeClusterDone(info ProbeClusterInfo) {
	s.postEvent(Event{
		Signal: streamAllocatorSignalProbeClusterDone,
		Data:   info,
	})
}

// called when prober active state changes
func (s *StreamAllocator) OnActiveChanged(isActive bool) {
	for _, t := range s.getTracks() {
		if isActive {
			// STREAM-ALLOCATOR-TODO: this can be changed to adapt to probe rate
			t.DownTrack().SetStreamAllocatorReportInterval(50 * time.Millisecond)
		} else {
			t.DownTrack().ClearStreamAllocatorReportInterval()
		}
	}
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
		if track.SetDirty(true) {
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

func (s *StreamAllocator) ping() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		<-ticker.C
		if s.isStopped.Load() {
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
		case streamAllocatorSignalPeriodicPing:
			event.handleSignalPeriodicPing(event)
		case streamAllocatorSignalSendProbe:
			event.handleSignalSendProbe(event)
		case streamAllocatorSignalProbeClusterDone:
			event.handleSignalProbeClusterDone(event)
		case streamAllocatorSignalResume:
			event.handleSignalResume(event)
		case streamAllocatorSignalSetAllowPause:
			event.handleSignalSetAllowPause(event)
		case streamAllocatorSignalSetChannelCapacity:
			event.handleSignalSetChannelCapacity(event)
			/* STREAM-ALLOCATOR-DATA
			case streamAllocatorSignalNACK:
				event.s.handleSignalNACK(event)
			case streamAllocatorSignalRTCPReceiverReport:
				event.s.handleSignalRTCPReceiverReport(event)
			*/
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
	receivedEstimate, _ := event.Data.(int64)
	s.lastReceivedEstimate = receivedEstimate
	// s.monitorRate(receivedEstimate)

	// while probing, maintain estimate separately to enable keeping current committed estimate if probe fails
	if s.probeController.IsInProbe() {
		s.handleNewEstimateInProbe()
	} else {
		s.handleNewEstimateInNonProbe()
	}
}

func (s *StreamAllocator) handleSignalPeriodicPing(Event) {
	// finalize probe if necessary
	trend, _ := s.channelObserver.GetTrend()
	isHandled, isNotFailing, isGoalReached := s.probeController.MaybeFinalizeProbe(
		s.channelObserver.HasEnoughEstimateSamples(),
		trend,
		s.channelObserver.GetLowestEstimate(),
	)
	if isHandled {
		s.onProbeDone(isNotFailing, isGoalReached)
	}

	// probe if necessary and timing is right
	if s.state == streamAllocatorStateDeficient {
		s.maybeProbe()
	}

	// s.updateTracksHistory()
}

func (s *StreamAllocator) handleSignalSendProbe(event Event) {
	bytesToSend := event.Data.(int)
	if bytesToSend <= 0 {
		return
	}

	bytesSent := 0
	for _, track := range s.getTracks() {
		sent := track.WritePaddingRTP(bytesToSend)
		bytesSent += sent
		bytesToSend -= sent
		if bytesToSend <= 0 {
			break
		}
	}

	if bytesSent != 0 {
		s.prober.ProbeSent(bytesSent)
	}
}

func (s *StreamAllocator) handleSignalProbeClusterDone(event Event) {
	info, _ := event.Data.(ProbeClusterInfo)
	s.probeController.ProbeClusterDone(info)
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

/* STREAM-ALLOCATOR-DATA
func (s *StreamAllocator) handleSignalNACK(event Event) {
	nackInfos := event.Data.([]sfu.NackInfo)

	s.videoTracksMu.Lock()
	track := s.videoTracks[event.TrackID]
	s.videoTracksMu.Unlock()

	if track != nil {
		track.UpdateNack(nackInfos)
	}
}

func (s *StreamAllocator) handleSignalRTCPReceiverReport(event Event) {
	rr := event.Data.(rtcp.ReceptionReport)

	s.videoTracksMu.Lock()
	track := s.videoTracks[event.TrackID]
	s.videoTracksMu.Unlock()

	if track != nil {
		track.ProcessRTCPReceiverReport(rr)
	}
}
*/

func (s *StreamAllocator) setState(state streamAllocatorState) {
	if s.state == state {
		return
	}

	s.params.Logger.Infow("stream allocator: state change", "from", s.state, "to", state)
	s.state = state

	// reset probe to enforce a delay after state change before probing
	s.probeController.Reset()
	// a fresh channel observer after state transition to get clean data
	s.channelObserver = s.newChannelObserverNonProbe()
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

func (s *StreamAllocator) handleNewEstimateInProbe() {
	// always update NACKs, even if aborted
	packetDelta, repeatedNackDelta := s.getNackDelta()

	if s.probeController.DoesProbeNeedFinalize() {
		// waiting for aborted probe to finalize
		return
	}

	s.channelObserver.AddEstimate(s.lastReceivedEstimate)
	s.channelObserver.AddNack(packetDelta, repeatedNackDelta)

	trend, _ := s.channelObserver.GetTrend()
	s.probeController.CheckProbe(trend, s.channelObserver.GetHighestEstimate())
}

func (s *StreamAllocator) handleNewEstimateInNonProbe() {
	s.channelObserver.AddEstimate(s.lastReceivedEstimate)

	packetDelta, repeatedNackDelta := s.getNackDelta()
	s.channelObserver.AddNack(packetDelta, repeatedNackDelta)

	trend, reason := s.channelObserver.GetTrend()
	if trend != ChannelTrendCongesting {
		return
	}

	var estimateToCommit int64
	expectedBandwidthUsage := s.getExpectedBandwidthUsage()
	switch reason {
	case ChannelCongestionReasonLoss:
		estimateToCommit = int64(float64(expectedBandwidthUsage) * (1.0 - s.params.Config.NackRatioAttenuator*s.channelObserver.GetNackRatio()))
	default:
		estimateToCommit = s.lastReceivedEstimate
	}
	if estimateToCommit > s.lastReceivedEstimate {
		estimateToCommit = s.lastReceivedEstimate
	}

	commitThreshold := int64(s.params.Config.ExpectedUsageThreshold * float64(expectedBandwidthUsage))
	action := "applying"
	if estimateToCommit > commitThreshold {
		action = "skipping"
	}

	s.params.Logger.Infow(
		fmt.Sprintf("stream allocator: channel congestion detected, %s channel capacity update", action),
		"reason", reason,
		"old(bps)", s.committedChannelCapacity,
		"new(bps)", estimateToCommit,
		"lastReceived(bps)", s.lastReceivedEstimate,
		"expectedUsage(bps)", expectedBandwidthUsage,
		"commitThreshold(bps)", commitThreshold,
		"channel", s.channelObserver.ToString(),
	)
	/* STREAM-ALLOCATOR-DATA
	s.params.Logger.Debugw(
		fmt.Sprintf("stream allocator: channel congestion detected, %s channel capacity: experimental", action),
		"rateHistory", s.rateMonitor.GetHistory(),
		"expectedQueuing", s.rateMonitor.GetQueuingGuess(),
		"nackHistory", s.channelObserver.GetNackHistory(),
		"trackHistory", s.getTracksHistory(),
	)
	*/
	if estimateToCommit > commitThreshold {
		// estimate to commit is either higher or within tolerance of expected uage, skip committing and re-allocating
		return
	}

	s.committedChannelCapacity = estimateToCommit

	// reset to get new set of samples for next trend
	s.channelObserver = s.newChannelObserverNonProbe()

	// reset probe to ensure it does not start too soon after a downward trend
	s.probeController.Reset()

	s.allocateAllTracks()
}

func (s *StreamAllocator) allocateTrack(track *Track) {
	// abort any probe that may be running when a track specific change needs allocation
	s.probeController.AbortProbe()

	// if not deficient, free pass allocate track
	if !s.params.Config.Enabled || s.state == streamAllocatorStateStable || !track.IsManaged() {
		update := NewStreamStateUpdate()
		allocation := track.AllocateOptimal(FlagAllowOvershootWhileOptimal)
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
	transition := track.ProvisionalAllocateGetCooperativeTransition(FlagAllowOvershootWhileDeficient)

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
					FlagAllowOvershootWhileDeficient,
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
		transition = track.ProvisionalAllocateGetCooperativeTransition(FlagAllowOvershootWhileDeficient) // get transition again to reset above allocation attempt using available headroom
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

func (s *StreamAllocator) onProbeDone(isNotFailing bool, isGoalReached bool) {
	highestEstimateInProbe := s.channelObserver.GetHighestEstimate()

	//
	// Reset estimator at the end of a probe irrespective of probe result to get fresh readings.
	// With a failed probe, the latest estimate could be lower than committed estimate.
	// As bandwidth estimator (remote in REMB case, local in TWCC case) holds state,
	// subsequent estimates could start from the lower point. That should not trigger a
	// downward trend and get latched to committed estimate as that would trigger a re-allocation.
	// With fresh readings, as long as the trend is not going downward, it will not get latched.
	//
	// NOTE: With TWCC, it is possible to reset bandwidth estimation to clean state as
	// the send side is in full control of bandwidth estimation.
	//
	channelObserverString := s.channelObserver.ToString()
	s.channelObserver = s.newChannelObserverNonProbe()
	s.params.Logger.Debugw(
		"probe done",
		"isNotFailing", isNotFailing,
		"isGoalReached", isGoalReached,
		"committedEstimate", s.committedChannelCapacity,
		"highestEstimate", highestEstimateInProbe,
		"channel", channelObserverString,
	)
	if !isNotFailing {
		return
	}

	if highestEstimateInProbe > s.committedChannelCapacity {
		s.committedChannelCapacity = highestEstimateInProbe
	}

	s.maybeBoostDeficientTracks()
}

func (s *StreamAllocator) maybeBoostDeficientTracks() {
	availableChannelCapacity := s.getAvailableHeadroom(false)
	if availableChannelCapacity <= 0 {
		return
	}

	update := NewStreamStateUpdate()

	sortedTracks := s.getMaxDistanceSortedDeficient()
boost_loop:
	for {
		for idx, track := range sortedTracks {
			allocation, boosted := track.AllocateNextHigher(availableChannelCapacity, FlagAllowOvershootInCatchup)
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
	if !s.params.Config.Enabled {
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

		allocation := track.AllocateOptimal(FlagAllowOvershootExemptTrackWhileDeficient)
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
					_, usedChannelCapacity := track.ProvisionalAllocate(availableChannelCapacity, layer, s.allowPause, FlagAllowOvershootWhileDeficient)
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

func (s *StreamAllocator) newChannelObserverProbe() *ChannelObserver {
	return NewChannelObserver(
		ChannelObserverParams{
			Name:   "probe",
			Config: s.params.Config.ChannelObserverProbeConfig,
		},
		s.params.Logger,
	)
}

func (s *StreamAllocator) newChannelObserverNonProbe() *ChannelObserver {
	return NewChannelObserver(
		ChannelObserverParams{
			Name:   "non-probe",
			Config: s.params.Config.ChannelObserverNonProbeConfig,
		},
		s.params.Logger,
	)
}

func (s *StreamAllocator) initProbe(probeGoalDeltaBps int64) {
	expectedBandwidthUsage := s.getExpectedBandwidthUsage()
	probeClusterId, probeGoalBps := s.probeController.InitProbe(probeGoalDeltaBps, expectedBandwidthUsage)

	channelState := ""
	if s.channelObserver != nil {
		channelState = s.channelObserver.ToString()
	}
	s.channelObserver = s.newChannelObserverProbe()
	s.channelObserver.SeedEstimate(s.lastReceivedEstimate)

	s.params.Logger.Debugw(
		"stream allocator: starting probe",
		"probeClusterId", probeClusterId,
		"current usage", expectedBandwidthUsage,
		"committed", s.committedChannelCapacity,
		"lastReceived", s.lastReceivedEstimate,
		"channel", channelState,
		"probeGoalDeltaBps", probeGoalDeltaBps,
		"goalBps", probeGoalBps,
	)
}

func (s *StreamAllocator) maybeProbe() {
	if s.overriddenChannelCapacity > 0 {
		// do not probe if channel capacity is overridden
		return
	}
	if !s.probeController.CanProbe() {
		return
	}

	switch s.params.Config.ProbeMode {
	case config.CongestionControlProbeModeMedia:
		s.maybeProbeWithMedia()
		s.adjustState()
	case config.CongestionControlProbeModePadding:
		s.maybeProbeWithPadding()
	}
}

func (s *StreamAllocator) maybeProbeWithMedia() {
	// boost deficient track farthest from desired layer
	for _, track := range s.getMaxDistanceSortedDeficient() {
		allocation, boosted := track.AllocateNextHigher(ChannelCapacityInfinity, FlagAllowOvershootInBoost)
		if !boosted {
			continue
		}

		update := NewStreamStateUpdate()
		updateStreamStateChange(track, allocation, update)
		s.maybeSendUpdate(update)

		s.probeController.Reset()
		break
	}
}

func (s *StreamAllocator) maybeProbeWithPadding() {
	// use deficient track farthest from desired layer to find how much to probe
	for _, track := range s.getMaxDistanceSortedDeficient() {
		transition, available := track.GetNextHigherTransition(FlagAllowOvershootInProbe)
		if !available || transition.BandwidthDelta < 0 {
			continue
		}

		s.initProbe(transition.BandwidthDelta)
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

/* STREAM-ALLOCATOR-DATA
// STREAM-ALLOCATOR-EXPERIMENTAL-TODO
// Monitor sent rate vs estimate to figure out queuing on congestion.
// Idea here is to pause all managed tracks on congestion detection immediately till queue drains.
// That will allow channel to clear up without more traffic added and a re-allocation can start afresh.
// Some bits to work out
//   - how good is queuing estimate?
//   - should we pause unmanaged tracks also? But, they will restart at highest layer and request a key frame.
//   - what should be the channel capacity to use when resume re-allocation happens?
func (s *StreamAllocator) monitorRate(estimate int64) {
	managedBytesSent := uint32(0)
	managedBytesRetransmitted := uint32(0)
	unmanagedBytesSent := uint32(0)
	unmanagedBytesRetransmitted := uint32(0)
	for _, track := range s.getTracks() {
		b, r := track.GetAndResetBytesSent()
		if track.IsManaged() {
			managedBytesSent += b
			managedBytesRetransmitted += r
		} else {
			unmanagedBytesSent += b
			unmanagedBytesRetransmitted += r
		}
	}

	s.rateMonitor.Update(estimate, managedBytesSent, managedBytesRetransmitted, unmanagedBytesSent, unmanagedBytesRetransmitted)
}

func (s *StreamAllocator) updateTracksHistory() {
	for _, track := range s.getTracks() {
		track.UpdateHistory()
	}
}

func (s *StreamAllocator) getTracksHistory() map[livekit.TrackID]string {
	tracks := s.getTracks()
	history := make(map[livekit.TrackID]string, len(tracks))
	for _, track := range tracks {
		history[track.ID()] = track.GetHistory()
	}

	return history
}
*/

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

// ------------------------------------------------

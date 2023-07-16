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
)

const (
	ChannelCapacityInfinity = 100 * 1000 * 1000 // 100 Mbps

	NackRatioAttenuator = 0.4 // how much to attenuate NACK ratio while calculating loss adjusted estimate

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

var (
	ChannelObserverParamsProbe = ChannelObserverParams{
		Name:                           "probe",
		EstimateRequiredSamples:        3,
		EstimateDownwardTrendThreshold: 0.0,
		EstimateCollapseThreshold:      0,
		EstimateValidityWindow:         10 * time.Second,
		NackWindowMinDuration:          500 * time.Millisecond,
		NackWindowMaxDuration:          1 * time.Second,
		NackRatioThreshold:             0.04,
	}

	ChannelObserverParamsNonProbe = ChannelObserverParams{
		Name:                           "non-probe",
		EstimateRequiredSamples:        8,
		EstimateDownwardTrendThreshold: -0.5,
		EstimateCollapseThreshold:      500 * time.Millisecond,
		EstimateValidityWindow:         10 * time.Second,
		NackWindowMinDuration:          1 * time.Second,
		NackWindowMaxDuration:          2 * time.Second,
		NackRatioThreshold:             0.08,
	}
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
		return fmt.Sprintf("%d", int(s))
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
	streamAllocatorSignalNACK
	streamAllocatorSignalRTCPReceiverReport
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
	case streamAllocatorSignalNACK:
		return "NACK"
	case streamAllocatorSignalRTCPReceiverReport:
		return "RTCP_RECEIVER_REPORT"
	default:
		return fmt.Sprintf("%d", int(s))
	}
}

// ---------------------------------------------------------------------------

type Event struct {
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
	rateMonitor     *RateMonitor

	videoTracksMu        sync.RWMutex
	videoTracks          map[livekit.TrackID]*Track
	isAllocateAllPending bool
	rembTrackingSSRC     uint32

	state streamAllocatorState

	eventChMu sync.RWMutex
	eventCh   chan Event

	isStopped atomic.Bool
}

func NewStreamAllocator(params StreamAllocatorParams) *StreamAllocator {
	s := &StreamAllocator{
		params:     params,
		allowPause: params.Config.AllowPause,
		prober: NewProber(ProberParams{
			Logger: params.Logger,
		}),
		rateMonitor: NewRateMonitor(),
		videoTracks: make(map[livekit.TrackID]*Track),
		eventCh:     make(chan Event, 1000),
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
	go s.processEvents()
	go s.ping()
}

func (s *StreamAllocator) Stop() {
	s.eventChMu.Lock()
	if s.isStopped.Swap(true) {
		s.eventChMu.Unlock()
		return
	}

	close(s.eventCh)
	s.eventChMu.Unlock()
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

	s.videoTracksMu.Lock()
	s.videoTracks[livekit.TrackID(downTrack.ID())] = track
	s.videoTracksMu.Unlock()

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

func (s *StreamAllocator) postEvent(event Event) {
	s.eventChMu.RLock()
	if s.isStopped.Load() {
		s.eventChMu.RUnlock()
		return
	}

	select {
	case s.eventCh <- event:
	default:
		s.params.Logger.Warnw("stream allocator: event queue full", nil)
	}
	s.eventChMu.RUnlock()
}

func (s *StreamAllocator) processEvents() {
	for event := range s.eventCh {
		if s.isStopped.Load() {
			break
		}

		s.handleEvent(&event)
	}

	s.probeController.StopProbe()
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

func (s *StreamAllocator) handleEvent(event *Event) {
	switch event.Signal {
	case streamAllocatorSignalAllocateTrack:
		s.handleSignalAllocateTrack(event)
	case streamAllocatorSignalAllocateAllTracks:
		s.handleSignalAllocateAllTracks(event)
	case streamAllocatorSignalAdjustState:
		s.handleSignalAdjustState(event)
	case streamAllocatorSignalEstimate:
		s.handleSignalEstimate(event)
	case streamAllocatorSignalPeriodicPing:
		s.handleSignalPeriodicPing(event)
	case streamAllocatorSignalSendProbe:
		s.handleSignalSendProbe(event)
	case streamAllocatorSignalProbeClusterDone:
		s.handleSignalProbeClusterDone(event)
	case streamAllocatorSignalResume:
		s.handleSignalResume(event)
	case streamAllocatorSignalSetAllowPause:
		s.handleSignalSetAllowPause(event)
	case streamAllocatorSignalSetChannelCapacity:
		s.handleSignalSetChannelCapacity(event)
	case streamAllocatorSignalNACK:
		s.handleSignalNACK(event)
	case streamAllocatorSignalRTCPReceiverReport:
		s.handleSignalRTCPReceiverReport(event)
	}
}

func (s *StreamAllocator) handleSignalAllocateTrack(event *Event) {
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

func (s *StreamAllocator) handleSignalAllocateAllTracks(event *Event) {
	s.videoTracksMu.Lock()
	s.isAllocateAllPending = false
	s.videoTracksMu.Unlock()

	if s.state == streamAllocatorStateDeficient {
		s.allocateAllTracks()
	}
}

func (s *StreamAllocator) handleSignalAdjustState(event *Event) {
	s.adjustState()
}

func (s *StreamAllocator) handleSignalEstimate(event *Event) {
	receivedEstimate, _ := event.Data.(int64)
	s.lastReceivedEstimate = receivedEstimate
	s.monitorRate(receivedEstimate)

	// while probing, maintain estimate separately to enable keeping current committed estimate if probe fails
	if s.probeController.IsInProbe() {
		s.handleNewEstimateInProbe()
	} else {
		s.handleNewEstimateInNonProbe()
	}
}

func (s *StreamAllocator) handleSignalPeriodicPing(event *Event) {
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

	s.updateTracksHistory()
}

func (s *StreamAllocator) handleSignalSendProbe(event *Event) {
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

func (s *StreamAllocator) handleSignalProbeClusterDone(event *Event) {
	info, _ := event.Data.(ProbeClusterInfo)
	s.probeController.ProbeClusterDone(info)
}

func (s *StreamAllocator) handleSignalResume(event *Event) {
	s.videoTracksMu.Lock()
	track := s.videoTracks[event.TrackID]
	s.videoTracksMu.Unlock()

	if track != nil {
		update := NewStreamStateUpdate()
		if track.SetPaused(false) {
			update.HandleStreamingChange(false, track)
		}
		s.maybeSendUpdate(update)
	}
}

func (s *StreamAllocator) handleSignalSetAllowPause(event *Event) {
	s.allowPause = event.Data.(bool)
}

func (s *StreamAllocator) handleSignalSetChannelCapacity(event *Event) {
	s.overriddenChannelCapacity = event.Data.(int64)
	if s.overriddenChannelCapacity > 0 {
		s.params.Logger.Infow("allocating on override channel capacity", "override", s.overriddenChannelCapacity)
		s.allocateAllTracks()
	} else {
		s.params.Logger.Infow("clearing override channel capacity")
	}
}

func (s *StreamAllocator) handleSignalNACK(event *Event) {
	nackInfos := event.Data.([]sfu.NackInfo)

	s.videoTracksMu.Lock()
	track := s.videoTracks[event.TrackID]
	s.videoTracksMu.Unlock()

	if track != nil {
		track.UpdateNack(nackInfos)
	}
}

func (s *StreamAllocator) handleSignalRTCPReceiverReport(event *Event) {
	rr := event.Data.(rtcp.ReceptionReport)

	s.videoTracksMu.Lock()
	track := s.videoTracks[event.TrackID]
	s.videoTracksMu.Unlock()

	if track != nil {
		track.ProcessRTCPReceiverReport(rr)
	}
}

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
		estimateToCommit = int64(float64(expectedBandwidthUsage) * (1.0 - NackRatioAttenuator*s.channelObserver.GetNackRatio()))
		if estimateToCommit > s.lastReceivedEstimate {
			estimateToCommit = s.lastReceivedEstimate
		}
	default:
		estimateToCommit = s.lastReceivedEstimate
	}

	s.params.Logger.Infow(
		"stream allocator: channel congestion detected, updating channel capacity",
		"reason", reason,
		"old(bps)", s.committedChannelCapacity,
		"new(bps)", estimateToCommit,
		"lastReceived(bps)", s.lastReceivedEstimate,
		"expectedUsage(bps)", expectedBandwidthUsage,
		"channel", s.channelObserver.ToString(),
	)
	s.params.Logger.Infow(
		"stream allocator: channel congestion detected, updating channel capacity: experimental",
		"rateHistory", s.rateMonitor.GetHistory(),
		"expectedQueuing", s.rateMonitor.GetQueuingGuess(),
		"nackHistory", s.channelObserver.GetNackHistory(),
		"trackHistory", s.getTracksHistory(),
	)
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
		if allocation.PauseReason == sfu.VideoPauseReasonBandwidth && track.SetPaused(true) {
			update.HandleStreamingChange(true, track)
		}
		s.maybeSendUpdate(update)
		return
	}

	//
	// In DEFICIENT state,
	//   1. Find cooperative transition from track that needs allocation.
	//   2. If track is currently streaming at minimum, do not do anything.
	//   3. If that track is giving back bits, apply the transition.
	//   4. If this track needs more, ask for best offer from others and try to use it.
	//
	track.ProvisionalAllocatePrepare()
	transition := track.ProvisionalAllocateGetCooperativeTransition(FlagAllowOvershootWhileDeficient)

	// track is currently streaming at minimum
	if transition.BandwidthDelta == 0 {
		return
	}

	// downgrade, giving back bits
	if transition.From.GreaterThan(transition.To) {
		allocation := track.ProvisionalAllocateCommit()

		update := NewStreamStateUpdate()
		if allocation.PauseReason == sfu.VideoPauseReasonBandwidth && track.SetPaused(true) {
			update.HandleStreamingChange(true, track)
		}
		s.maybeSendUpdate(update)

		s.adjustState()
		return
		// STREAM-ALLOCATOR-TODO-START
		// Should use the bits given back to start any paused track.
		// Note layer downgrade may actually have positive delta (i.e. consume more bits)
		// because of when the measurement is done. Watch for that.
		// STREAM_ALLOCATOR-TODO-END
	}

	//
	// This track is currently not streaming and needs bits to start.
	// Try to redistribute starting with tracks that are closest to their desired.
	//
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
			if allocation.PauseReason == sfu.VideoPauseReasonBandwidth && track.SetPaused(true) {
				update.HandleStreamingChange(true, t)
			}
		}

		// STREAM-ALLOCATOR-TODO if got too much extra, can potentially give it to some deficient track
	}

	// commit the track that needs change if enough could be acquired or pause not allowed
	if !s.allowPause || bandwidthAcquired >= transition.BandwidthDelta {
		allocation := track.ProvisionalAllocateCommit()
		if allocation.PauseReason == sfu.VideoPauseReasonBandwidth && track.SetPaused(true) {
			update.HandleStreamingChange(true, track)
		}
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
	s.params.Logger.Infow(
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
	committedChannelCapacity := s.committedChannelCapacity
	if s.params.Config.MinChannelCapacity > committedChannelCapacity {
		committedChannelCapacity = s.params.Config.MinChannelCapacity
		s.params.Logger.Debugw(
			"stream allocator: overriding channel capacity",
			"actual", s.committedChannelCapacity,
			"override", committedChannelCapacity,
		)
	}
	availableChannelCapacity := committedChannelCapacity - s.getExpectedBandwidthUsage()
	if availableChannelCapacity <= 0 {
		return
	}

	update := NewStreamStateUpdate()

	for _, track := range s.getMaxDistanceSortedDeficient() {
		allocation, boosted := track.AllocateNextHigher(availableChannelCapacity, FlagAllowOvershootInCatchup)
		if !boosted {
			continue
		}

		if allocation.PauseReason == sfu.VideoPauseReasonBandwidth && track.SetPaused(true) {
			update.HandleStreamingChange(true, track)
		}

		availableChannelCapacity -= allocation.BandwidthDelta
		if availableChannelCapacity <= 0 {
			break
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

	availableChannelCapacity := s.committedChannelCapacity
	if s.params.Config.MinChannelCapacity > availableChannelCapacity {
		availableChannelCapacity = s.params.Config.MinChannelCapacity
		s.params.Logger.Debugw(
			"stream allocator: overriding channel capacity with min channel capacity",
			"actual", s.committedChannelCapacity,
			"override", availableChannelCapacity,
		)
	}
	if s.overriddenChannelCapacity > 0 {
		availableChannelCapacity = s.overriddenChannelCapacity
		s.params.Logger.Debugw(
			"stream allocator: overriding channel capacity",
			"actual", s.committedChannelCapacity,
			"override", availableChannelCapacity,
		)
	}

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
		if allocation.PauseReason == sfu.VideoPauseReasonBandwidth && track.SetPaused(true) {
			update.HandleStreamingChange(true, track)
		}

		// STREAM-ALLOCATOR-TODO: optimistic allocation before bitrate is available will return 0. How to account for that?
		availableChannelCapacity -= allocation.BandwidthRequested
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
			if allocation.PauseReason == sfu.VideoPauseReasonBandwidth && track.SetPaused(true) {
				update.HandleStreamingChange(true, track)
			}
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
					usedChannelCapacity := track.ProvisionalAllocate(availableChannelCapacity, layer, s.allowPause, FlagAllowOvershootWhileDeficient)
					availableChannelCapacity -= usedChannelCapacity
					if availableChannelCapacity < 0 {
						availableChannelCapacity = 0
					}
				}
			}
		}

		for _, track := range sorted {
			allocation := track.ProvisionalAllocateCommit()
			if allocation.PauseReason == sfu.VideoPauseReasonBandwidth && track.SetPaused(true) {
				update.HandleStreamingChange(true, track)
			}
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

func (s *StreamAllocator) getExpectedBandwidthUsage() int64 {
	expected := int64(0)
	for _, track := range s.getTracks() {
		expected += track.BandwidthRequested()
	}

	return expected
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
	return NewChannelObserver(ChannelObserverParamsProbe, s.params.Logger)
}

func (s *StreamAllocator) newChannelObserverNonProbe() *ChannelObserver {
	return NewChannelObserver(ChannelObserverParamsNonProbe, s.params.Logger)
}

func (s *StreamAllocator) initProbe(probeGoalDeltaBps int64) {
	expectedBandwidthUsage := s.getExpectedBandwidthUsage()
	if float64(expectedBandwidthUsage) > 1.5*float64(s.committedChannelCapacity) {
		// STREAM-ALLOCATOR-TODO-START
		// Should probably skip probing if the expected usage is much higher than committed channel capacity.
		// But, give that bandwidth estimate is volatile at times and can drop down to small values,
		// not probing means streaming stuck in a well for long.
		// Observe this and figure out if there is a threshold from practical use cases that can be used to
		// skip probing safely
		// STREAM-ALLOCATOR-TODO-END
		s.params.Logger.Warnw(
			"stream allocator: starting probe alarm",
			fmt.Errorf("expected too high, expected: %d, committed: %d", expectedBandwidthUsage, s.committedChannelCapacity),
		)
	}

	probeClusterId, probeGoalBps := s.probeController.InitProbe(probeGoalDeltaBps, expectedBandwidthUsage)

	channelState := ""
	if s.channelObserver != nil {
		channelState = s.channelObserver.ToString()
	}
	s.channelObserver = s.newChannelObserverProbe()
	s.channelObserver.SeedEstimate(s.lastReceivedEstimate)

	s.params.Logger.Infow(
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
		if allocation.PauseReason == sfu.VideoPauseReasonBandwidth && track.SetPaused(true) {
			update.HandleStreamingChange(true, track)
		}
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

// ------------------------------------------------

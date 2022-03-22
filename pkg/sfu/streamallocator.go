package sfu

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"go.uber.org/atomic"

	"github.com/livekit/livekit-server/pkg/config"
)

const (
	ChannelCapacityInfinity = 100 * 1000 * 1000 // 100 Mbps

	NumRequiredEstimatesNonProbe = 8
	NumRequiredEstimatesProbe    = 3

	NackRatioThresholdNonProbe = 0.06
	NackRatioThresholdProbe    = 0.04

	NackRatioAttenuator = 0.4 // how much to attenuate NACK ratio while calculating loss adjusted estimate

	ProbeWaitBase      = 5 * time.Second
	ProbeBackoffFactor = 1.5
	ProbeWaitMax       = 30 * time.Second
	ProbeSettleWait    = 250
	ProbeTrendWait     = 2 * time.Second

	ProbePct         = 120
	ProbeMinBps      = 200 * 1000 // 200 kbps
	ProbeMinDuration = 20 * time.Second
	ProbeMaxDuration = 21 * time.Second

	PriorityMin                = uint8(1)
	PriorityMax                = uint8(255)
	PriorityDefaultScreenshare = PriorityMax
	PriorityDefaultVideo       = PriorityMin
)

type State int

const (
	StateStable State = iota
	StateDeficient
)

func (s State) String() string {
	switch s {
	case StateStable:
		return "STABLE"
	case StateDeficient:
		return "DEFICIENT"
	default:
		return fmt.Sprintf("%d", int(s))
	}
}

type Signal int

const (
	SignalAddTrack Signal = iota
	SignalRemoveTrack
	SignalSetTrackPriority
	SignalEstimate
	SignalTargetBitrate
	SignalAvailableLayersChange
	SignalBitrateAvailabilityChange
	SignalSubscriptionChange
	SignalSubscribedLayersChange
	SignalPeriodicPing
	SignalSendProbe
	SignalProbeClusterDone
)

func (s Signal) String() string {
	switch s {
	case SignalAddTrack:
		return "ADD_TRACK"
	case SignalRemoveTrack:
		return "REMOVE_TRACK"
	case SignalSetTrackPriority:
		return "SET_TRACK_PRIORITY"
	case SignalEstimate:
		return "ESTIMATE"
	case SignalTargetBitrate:
		return "TARGET_BITRATE"
	case SignalAvailableLayersChange:
		return "AVAILABLE_LAYERS_CHANGE"
	case SignalBitrateAvailabilityChange:
		return "BITRATE_AVAILABILITY_CHANGE"
	case SignalSubscriptionChange:
		return "SUBSCRIPTION_CHANGE"
	case SignalSubscribedLayersChange:
		return "SUBSCRIBED_LAYERS_CHANGE"
	case SignalPeriodicPing:
		return "PERIODIC_PING"
	case SignalSendProbe:
		return "SEND_PROBE"
	case SignalProbeClusterDone:
		return "PROBE_CLUSTER_DONE"
	default:
		return fmt.Sprintf("%d", int(s))
	}
}

type Event struct {
	Signal    Signal
	DownTrack *DownTrack
	Data      interface{}
}

func (e Event) String() string {
	return fmt.Sprintf("StreamAllocator:Event{signal: %s, data: %s}", e.Signal, e.Data)
}

type StreamAllocatorParams struct {
	Config config.CongestionControlConfig
	Logger logger.Logger
}

type StreamAllocator struct {
	params StreamAllocatorParams

	onStreamStateChange func(update *StreamStateUpdate) error

	rembTrackingSSRC uint32

	bwe cc.BandwidthEstimator

	lastReceivedEstimate     int64
	committedChannelCapacity int64

	probeInterval         time.Duration
	lastProbeStartTime    time.Time
	probeGoalBps          int64
	probeClusterId        ProbeClusterId
	abortedProbeClusterId ProbeClusterId
	probeTrendObserved    bool
	probeEndTime          time.Time
	probeChannelObserver  *ChannelObserver

	prober *Prober

	channelObserver *ChannelObserver

	videoTracks map[livekit.TrackID]*Track

	state State

	eventChMu sync.RWMutex
	eventCh   chan Event

	isStopped atomic.Bool
}

func NewStreamAllocator(params StreamAllocatorParams) *StreamAllocator {
	s := &StreamAllocator{
		params: params,
		prober: NewProber(ProberParams{
			Logger: params.Logger,
		}),
		channelObserver: NewChannelObserver("non-probe", params.Logger, NumRequiredEstimatesNonProbe, NackRatioThresholdNonProbe),
		videoTracks:     make(map[livekit.TrackID]*Track),
		eventCh:         make(chan Event, 20),
	}

	s.resetState()

	s.prober.OnSendProbe(s.onSendProbe)
	s.prober.OnProbeClusterDone(s.onProbeClusterDone)

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

func (s *StreamAllocator) AddTrack(downTrack *DownTrack, params AddTrackParams) {
	s.postEvent(Event{
		Signal:    SignalAddTrack,
		DownTrack: downTrack,
		Data:      params,
	})

	if downTrack.Kind() == webrtc.RTPCodecTypeVideo {
		downTrack.OnREMB(s.onREMB)
		downTrack.OnTransportCCFeedback(s.onTransportCCFeedback)
		downTrack.OnAvailableLayersChanged(s.onAvailableLayersChanged)
		downTrack.OnBitrateAvailabilityChanged(s.onBitrateAvailabilityChanged)
		downTrack.OnSubscriptionChanged(s.onSubscriptionChanged)
		downTrack.OnSubscribedLayersChanged(s.onSubscribedLayersChanged)
		downTrack.OnPacketSentUnsafe(s.onPacketSent)
	}
}

func (s *StreamAllocator) RemoveTrack(downTrack *DownTrack) {
	s.postEvent(Event{
		Signal:    SignalRemoveTrack,
		DownTrack: downTrack,
	})
}

func (s *StreamAllocator) SetTrackPriority(downTrack *DownTrack, priority uint8) {
	s.postEvent(Event{
		Signal:    SignalSetTrackPriority,
		DownTrack: downTrack,
		Data:      priority,
	})
}

func (s *StreamAllocator) resetState() {
	s.channelObserver.Reset()
	s.resetProbe()

	s.state = StateStable
}

// called when a new REMB is received
func (s *StreamAllocator) onREMB(downTrack *DownTrack, remb *rtcp.ReceiverEstimatedMaximumBitrate) {
	s.postEvent(Event{
		Signal:    SignalEstimate,
		DownTrack: downTrack,
		Data:      remb,
	})
}

// called when a new transport-cc feedback is received
func (s *StreamAllocator) onTransportCCFeedback(downTrack *DownTrack, fb *rtcp.TransportLayerCC) {
	if s.bwe != nil {
		s.bwe.WriteRTCP([]rtcp.Packet{fb}, nil)
	}
}

// called when target bitrate changes
func (s *StreamAllocator) onTargetBitrateChange(bitrate int) {
	s.postEvent(Event{
		Signal: SignalTargetBitrate,
		Data:   bitrate,
	})
}

// called when feeding track's layer availability changes
func (s *StreamAllocator) onAvailableLayersChanged(downTrack *DownTrack) {
	s.postEvent(Event{
		Signal:    SignalAvailableLayersChange,
		DownTrack: downTrack,
	})
}

// called when feeding track's bitrate measurement of any layer is available
func (s *StreamAllocator) onBitrateAvailabilityChanged(downTrack *DownTrack) {
	s.postEvent(Event{
		Signal:    SignalBitrateAvailabilityChange,
		DownTrack: downTrack,
	})
}

// called when subscription settings changes (muting/unmuting of track)
func (s *StreamAllocator) onSubscriptionChanged(downTrack *DownTrack) {
	s.postEvent(Event{
		Signal:    SignalSubscriptionChange,
		DownTrack: downTrack,
	})
}

// called when subscribed layers changes (limiting max layers)
func (s *StreamAllocator) onSubscribedLayersChanged(downTrack *DownTrack, layers VideoLayers) {
	s.postEvent(Event{
		Signal:    SignalSubscribedLayersChange,
		DownTrack: downTrack,
		Data:      layers,
	})
}

// called when a video DownTrack sends a packet
func (s *StreamAllocator) onPacketSent(downTrack *DownTrack, size int) {
	s.prober.PacketSent(size)
}

// called when prober wants to send packet(s)
func (s *StreamAllocator) onSendProbe(bytesToSend int) {
	s.postEvent(Event{
		Signal: SignalSendProbe,
		Data:   bytesToSend,
	})
}

// called when prober wants to send packet(s)
func (s *StreamAllocator) onProbeClusterDone(info ProbeClusterInfo) {
	s.postEvent(Event{
		Signal: SignalProbeClusterDone,
		Data:   info,
	})
}

func (s *StreamAllocator) postEvent(event Event) {
	s.eventChMu.RLock()
	if s.isStopped.Load() {
		s.eventChMu.RUnlock()
		return
	}

	s.eventCh <- event
	s.eventChMu.RUnlock()
}

func (s *StreamAllocator) processEvents() {
	for event := range s.eventCh {
		s.handleEvent(&event)
	}
}

func (s *StreamAllocator) ping() {
	ticker := time.NewTicker(time.Second)

	for {
		<-ticker.C
		if s.isStopped.Load() {
			return
		}

		s.postEvent(Event{
			Signal: SignalPeriodicPing,
		})
	}
}

func (s *StreamAllocator) handleEvent(event *Event) {
	switch event.Signal {
	case SignalAddTrack:
		s.handleSignalAddTrack(event)
	case SignalRemoveTrack:
		s.handleSignalRemoveTrack(event)
	case SignalSetTrackPriority:
		s.handleSignalSetTrackPriority(event)
	case SignalEstimate:
		s.handleSignalEstimate(event)
	case SignalTargetBitrate:
		s.handleSignalTargetBitrate(event)
	case SignalAvailableLayersChange:
		s.handleSignalAvailableLayersChange(event)
	case SignalBitrateAvailabilityChange:
		s.handleSignalBitrateAvailabilityChange(event)
	case SignalSubscriptionChange:
		s.handleSignalSubscriptionChange(event)
	case SignalSubscribedLayersChange:
		s.handleSignalSubscribedLayersChange(event)
	case SignalPeriodicPing:
		s.handleSignalPeriodicPing(event)
	case SignalSendProbe:
		s.handleSignalSendProbe(event)
	case SignalProbeClusterDone:
		s.handleSignalProbeClusterDone(event)
	}
}

func (s *StreamAllocator) handleSignalAddTrack(event *Event) {
	if event.DownTrack.Kind() != webrtc.RTPCodecTypeVideo {
		return
	}

	params, _ := event.Data.(AddTrackParams)
	track := newTrack(event.DownTrack, params.Source, params.IsSimulcast, params.PublisherID, s.params.Logger)
	track.SetPriority(params.Priority)

	trackID := livekit.TrackID(event.DownTrack.ID())
	s.videoTracks[trackID] = track

	s.allocateTrack(track)
}

func (s *StreamAllocator) handleSignalRemoveTrack(event *Event) {
	if event.DownTrack.Kind() != webrtc.RTPCodecTypeVideo {
		return
	}

	trackID := livekit.TrackID(event.DownTrack.ID())
	delete(s.videoTracks, trackID)

	// re-initialize estimate if all managed tracks are removed, let it get a fresh start
	if len(s.getSorted()) == 0 {
		s.resetState()
		return
	}

	// LK-TODO: use any saved bandwidth to re-distribute
	s.adjustState()
}

func (s *StreamAllocator) handleSignalSetTrackPriority(event *Event) {
	trackID := livekit.TrackID(event.DownTrack.ID())
	track, ok := s.videoTracks[trackID]
	if !ok {
		return
	}

	priority, _ := event.Data.(uint8)
	changed := track.SetPriority(priority)
	if changed && s.state == StateDeficient {
		// do a full allocation on a track priority change to keep it simple
		// LK-TODO-START
		// When in a large room, subscriber could be adjusting priority of
		// a lot of tracks in quick succession. That could trigger allocation burst.
		// Find ways to avoid it.
		// LK-TODO-END
		s.allocateAllTracks()
	}

}

func (s *StreamAllocator) handleSignalEstimate(event *Event) {
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
	// LK-TODO-START
	// Need to check if the same SSRC reports can somehow race, i.e. does pion send
	// RTCP dispatch for same SSRC on different threads? If not, the tracking SSRC
	// should prevent racing
	// LK-TODO-END
	//

	// if there are no video tracks, ignore any straggler REMB
	if len(s.videoTracks) == 0 {
		return
	}

	remb, _ := event.Data.(*rtcp.ReceiverEstimatedMaximumBitrate)

	found := false
	for _, ssrc := range remb.SSRCs {
		if ssrc == s.rembTrackingSSRC {
			found = true
			break
		}
	}
	if !found {
		if len(remb.SSRCs) == 0 {
			s.params.Logger.Warnw("no SSRC to track REMB", nil)
			return
		}

		// try to lock to track which is sending this update
		for _, ssrc := range remb.SSRCs {
			if ssrc == event.DownTrack.SSRC() {
				s.rembTrackingSSRC = event.DownTrack.SSRC()
				found = true
				break
			}
		}

		if !found {
			s.rembTrackingSSRC = remb.SSRCs[0]
		}
	}

	if s.rembTrackingSSRC != event.DownTrack.SSRC() {
		return
	}

	s.handleNewEstimate(int64(remb.Bitrate))
}

func (s *StreamAllocator) handleSignalTargetBitrate(event *Event) {
	receivedEstimate, _ := event.Data.(int)
	s.handleNewEstimate(int64(receivedEstimate))
}

func (s *StreamAllocator) handleSignalAvailableLayersChange(event *Event) {
	track, ok := s.videoTracks[livekit.TrackID(event.DownTrack.ID())]
	if !ok {
		return
	}

	s.allocateTrack(track)
}

func (s *StreamAllocator) handleSignalBitrateAvailabilityChange(event *Event) {
	track, ok := s.videoTracks[livekit.TrackID(event.DownTrack.ID())]
	if !ok {
		return
	}

	s.allocateTrack(track)
}

func (s *StreamAllocator) handleSignalSubscriptionChange(event *Event) {
	track, ok := s.videoTracks[livekit.TrackID(event.DownTrack.ID())]
	if !ok {
		return
	}

	s.allocateTrack(track)
}

func (s *StreamAllocator) handleSignalSubscribedLayersChange(event *Event) {
	track, ok := s.videoTracks[livekit.TrackID(event.DownTrack.ID())]
	if !ok {
		return
	}

	layers := event.Data.(VideoLayers)
	track.UpdateMaxLayers(layers)

	s.allocateTrack(track)
}

func (s *StreamAllocator) handleSignalPeriodicPing(event *Event) {
	// finalize probe if necessary
	if s.isInProbe() && !s.probeEndTime.IsZero() && time.Now().After(s.probeEndTime) {
		s.finalizeProbe()
	}

	// probe if necessary and timing is right
	if s.state == StateDeficient {
		s.maybeProbe()
	}
}

func (s *StreamAllocator) handleSignalSendProbe(event *Event) {
	bytesToSend := event.Data.(int)
	if bytesToSend <= 0 {
		return
	}

	bytesSent := 0
	for _, track := range s.videoTracks {
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
	if s.probeClusterId != info.Id {
		return
	}

	if s.abortedProbeClusterId == ProbeClusterIdInvalid {
		// successful probe, finalize
		s.finalizeProbe()
		return
	}

	// ensure probe queue is flushed
	// LK-TODO: ProbeSettleWait should actually be a certain number of RTTs.
	lowestEstimate := int64(math.Min(float64(s.committedChannelCapacity), float64(s.probeChannelObserver.GetLowestEstimate())))
	expectedDuration := float64(info.BytesSent*8*1000) / float64(lowestEstimate)
	queueTime := expectedDuration - float64(info.Duration.Milliseconds())
	if queueTime < 0.0 {
		queueTime = 0.0
	}
	queueWait := time.Duration(queueTime+float64(ProbeSettleWait)) * time.Millisecond
	s.probeEndTime = s.lastProbeStartTime.Add(queueWait)
}

func (s *StreamAllocator) setState(state State) {
	if s.state == state {
		return
	}

	s.params.Logger.Debugw("state change", "from", s.state, "to", state)
	s.state = state

	// reset probe to enforce a delay after state change before probing
	s.lastProbeStartTime = time.Now()
}

func (s *StreamAllocator) adjustState() {
	for _, track := range s.videoTracks {
		if track.IsDeficient() {
			s.setState(StateDeficient)
			return
		}
	}

	s.setState(StateStable)
}

func (s *StreamAllocator) handleNewEstimate(receivedEstimate int64) {
	s.lastReceivedEstimate = receivedEstimate

	// while probing, maintain estimate separately to enable keeping current committed estimate if probe fails
	if s.isInProbe() {
		s.handleNewEstimateInProbe()
	} else {
		s.handleNewEstimateInNonProbe()
	}
}

func (s *StreamAllocator) handleNewEstimateInProbe() {
	s.probeChannelObserver.AddEstimate(s.lastReceivedEstimate)

	packetDelta, repeatedNackDelta := s.getNackDelta()
	s.probeChannelObserver.AddNack(packetDelta, repeatedNackDelta)

	trend := s.probeChannelObserver.GetTrend()
	if trend != ChannelTrendNeutral {
		s.probeTrendObserved = true
	}

	switch {
	case s.abortedProbeClusterId != ProbeClusterIdInvalid:
		return
	case !s.probeTrendObserved && time.Since(s.lastProbeStartTime) > ProbeTrendWait:
		//
		// More of a safety net.
		// In rare cases, the estimate gets stuck. Prevent from probe running amok
		// LK-TODO: Need more testing here to ensure that probe does not cause a lot of damage
		//
		s.params.Logger.Debugw("probe: aborting, no trend", "cluster", s.probeClusterId)
		s.abortProbe()
	case trend == ChannelTrendCongesting:
		// stop immediately if the probe is congesting channel more
		s.params.Logger.Debugw("probe: aborting, channel is congesting", "cluster", s.probeClusterId)
		s.abortProbe()
	case s.probeChannelObserver.GetHighestEstimate() > s.probeGoalBps:
		// reached goal, stop probing
		s.params.Logger.Debugw(
			"probe: stopping, goal reached",
			"cluster", s.probeClusterId,
			"goal", s.probeGoalBps,
			"highest", s.probeChannelObserver.GetHighestEstimate(),
		)
		s.stopProbe()
	}
}

func (s *StreamAllocator) handleNewEstimateInNonProbe() {
	s.channelObserver.AddEstimate(s.lastReceivedEstimate)

	packetDelta, repeatedNackDelta := s.getNackDelta()
	s.channelObserver.AddNack(packetDelta, repeatedNackDelta)

	trend := s.channelObserver.GetTrend()
	if trend != ChannelTrendCongesting {
		return
	}

	nackRatio := s.channelObserver.GetNackRatio()
	lossAdjustedEstimate := s.lastReceivedEstimate
	if nackRatio > NackRatioThresholdNonProbe {
		lossAdjustedEstimate = int64(float64(lossAdjustedEstimate) * (1.0 - NackRatioAttenuator*nackRatio))
	}

	s.params.Logger.Infow(
		"channel congestion detected, updating channel capacity",
		"old(bps)", s.committedChannelCapacity,
		"new(bps)", lossAdjustedEstimate,
		"lastReceived(bps)", s.lastReceivedEstimate,
		"nackRatio", nackRatio,
	)
	s.committedChannelCapacity = lossAdjustedEstimate

	// reset to get new set of samples for next trend
	s.channelObserver.Reset()

	// reset probe to ensure it does not start too soon after a downward trend
	s.resetProbe()

	s.allocateAllTracks()
}

func (s *StreamAllocator) allocateTrack(track *Track) {
	// abort any probe that may be running when a track specific change needs allocation
	s.abortProbe()

	// if not deficient, free pass allocate track
	if !s.params.Config.Enabled || s.state == StateStable || !track.IsManaged() {
		update := NewStreamStateUpdate()
		allocation := track.AllocateOptimal()
		update.HandleStreamingChange(allocation.change, track)
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
	transition := track.ProvisionalAllocateGetCooperativeTransition()

	// track is currently streaming at minimum
	if transition.bandwidthDelta == 0 {
		return
	}

	// downgrade, giving back bits
	if transition.from.GreaterThan(transition.to) {
		allocation := track.ProvisionalAllocateCommit()

		update := NewStreamStateUpdate()
		update.HandleStreamingChange(allocation.change, track)
		s.maybeSendUpdate(update)

		s.adjustState()
		return
		// LK-TODO-START
		// Should use the bits given back to start any paused track.
		// Note layer downgrade may actually have positive delta (i.e. consume more bits)
		// because of when the measurement is done. Watch for that.
		// LK-TODO-END
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
		if tx.bandwidthDelta < 0 {
			contributingTracks = append(contributingTracks, t)

			bandwidthAcquired += -tx.bandwidthDelta
			if bandwidthAcquired >= transition.bandwidthDelta {
				break
			}
		}
	}

	update := NewStreamStateUpdate()
	if bandwidthAcquired >= transition.bandwidthDelta {
		// commit the tracks that contributed
		for _, t := range contributingTracks {
			allocation := t.ProvisionalAllocateCommit()
			update.HandleStreamingChange(allocation.change, t)
		}

		// LK-TODO if got too much extra, can potentially give it to some deficient track
	}

	// commit the track that needs change if enough could be acquired or pause not allowed
	if !s.params.Config.AllowPause || bandwidthAcquired >= transition.bandwidthDelta {
		allocation := track.ProvisionalAllocateCommit()
		update.HandleStreamingChange(allocation.change, track)
	}

	s.maybeSendUpdate(update)

	s.adjustState()
}

func (s *StreamAllocator) finalizeProbe() {
	aborted := s.probeClusterId == s.abortedProbeClusterId
	highestEstimateInProbe := s.probeChannelObserver.GetHighestEstimate()

	s.clearProbe()

	//
	// Reset estimator at the end of a probe irrespective of probe result to get fresh readings.
	// With a failed probe, the latest estimate would be lower than committed estimate.
	// As bandwidth estimator (remote in REMB case, local in TWCC case) holds state,
	// subsequent estimates could start from the lower point. That should not trigger a
	// downward trend and get latched to committed estimate as that would trigger a re-allocation.
	// With fresh readings, as long as the trend is not going downward, it will not get latched.
	//
	// NOTE: With TWCC, it is possible to reset bandwidth estimation to clean state as
	// the send side is in full control of bandwidth estimation.
	//
	s.channelObserver.Reset()

	if aborted {
		// failed probe, backoff
		s.backoffProbeInterval()
		return
	}

	// reset probe interval on a successful probe
	s.resetProbeInterval()

	// probe estimate is same or higher, commit it and try allocate deficient tracks
	s.params.Logger.Infow(
		"successful probe, updating channel capacity",
		"old(bps)", s.committedChannelCapacity,
		"new(bps)", highestEstimateInProbe,
	)
	s.committedChannelCapacity = highestEstimateInProbe

	s.maybeBoostDeficientTracks()
}

func (s *StreamAllocator) maybeBoostDeficientTracks() {
	committedChannelCapacity := s.committedChannelCapacity
	if s.params.Config.MinChannelCapacity > committedChannelCapacity {
		committedChannelCapacity = s.params.Config.MinChannelCapacity
		s.params.Logger.Debugw(
			"overriding channel capacity",
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
		allocation, boosted := track.AllocateNextHigher(availableChannelCapacity)
		if !boosted {
			continue
		}

		update.HandleStreamingChange(allocation.change, track)

		availableChannelCapacity -= allocation.bandwidthDelta
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
	// Start with the lowest layers and give each track a chance at that layer and keep going up.
	// As long as there is enough bandwidth for tracks to stream at the lowest layers, the first goal is achieved.
	//
	// Tracks that have higher subscribed layers can use any additional available bandwidth. This tried to achieve the second goal.
	//
	// If there is not enough bandwidth even for the lowest layers, tracks at lower priorities will be paused.
	//
	update := NewStreamStateUpdate()

	availableChannelCapacity := s.committedChannelCapacity
	if s.params.Config.MinChannelCapacity > availableChannelCapacity {
		availableChannelCapacity = s.params.Config.MinChannelCapacity
		s.params.Logger.Debugw(
			"overriding channel capacity",
			"actual", s.committedChannelCapacity,
			"override", availableChannelCapacity,
		)
	}

	//
	// This pass is find out if there is any leftover channel capacity after allocating exempt tracks.
	// Infinite channel capacity is given so that exempt tracks do not stall
	//
	for _, track := range s.videoTracks {
		if track.IsManaged() {
			continue
		}

		allocation := track.AllocateOptimal()
		update.HandleStreamingChange(allocation.change, track)

		// LK-TODO: optimistic allocation before bitrate is available will return 0. How to account for that?
		availableChannelCapacity -= allocation.bandwidthRequested
	}

	if availableChannelCapacity < 0 {
		availableChannelCapacity = 0
	}
	if availableChannelCapacity == 0 && s.params.Config.AllowPause {
		// nothing left for managed tracks, pause them all
		for _, track := range s.videoTracks {
			if !track.IsManaged() {
				continue
			}

			allocation := track.Pause()
			update.HandleStreamingChange(allocation.change, track)
		}
	} else {
		sorted := s.getSorted()
		for _, track := range sorted {
			track.ProvisionalAllocatePrepare()
		}

		for spatial := int32(0); spatial <= DefaultMaxLayerSpatial; spatial++ {
			for temporal := int32(0); temporal <= DefaultMaxLayerTemporal; temporal++ {
				layers := VideoLayers{
					spatial:  spatial,
					temporal: temporal,
				}

				for _, track := range sorted {
					usedChannelCapacity := track.ProvisionalAllocate(availableChannelCapacity, layers, s.params.Config.AllowPause)
					availableChannelCapacity -= usedChannelCapacity
					if availableChannelCapacity < 0 {
						availableChannelCapacity = 0
					}
				}
			}
		}

		for _, track := range sorted {
			allocation := track.ProvisionalAllocateCommit()
			update.HandleStreamingChange(allocation.change, track)
		}
	}

	s.maybeSendUpdate(update)

	s.adjustState()
}

func (s *StreamAllocator) maybeSendUpdate(update *StreamStateUpdate) {
	if update.Empty() {
		return
	}

	s.params.Logger.Debugw("streamed tracks changed", "update", update)
	if s.onStreamStateChange != nil {
		err := s.onStreamStateChange(update)
		if err != nil {
			s.params.Logger.Errorw("could not send streamed tracks update", err)
		}
	}
}

func (s *StreamAllocator) getExpectedBandwidthUsage() int64 {
	expected := int64(0)
	for _, track := range s.videoTracks {
		expected += track.BandwidthRequested()
	}

	return expected
}

func (s *StreamAllocator) getNackDelta() (uint32, uint32) {
	aggPacketDelta := uint32(0)
	aggRepeatedNackDelta := uint32(0)
	for _, track := range s.videoTracks {
		packetDelta, nackDelta := track.GetNackDelta()
		aggPacketDelta += packetDelta
		aggRepeatedNackDelta += nackDelta
	}

	return aggPacketDelta, aggRepeatedNackDelta
}

func (s *StreamAllocator) initProbe(probeRateBps int64) {
	s.lastProbeStartTime = time.Now()

	expectedBandwidthUsage := s.getExpectedBandwidthUsage()
	s.probeGoalBps = expectedBandwidthUsage + probeRateBps

	s.abortedProbeClusterId = ProbeClusterIdInvalid

	s.probeTrendObserved = false

	s.probeEndTime = time.Time{}

	s.probeChannelObserver = NewChannelObserver("probe", s.params.Logger, NumRequiredEstimatesProbe, NackRatioThresholdProbe)
	s.probeChannelObserver.SeedEstimate(s.lastReceivedEstimate)

	desiredRateBps := int(probeRateBps) + int(math.Max(float64(s.committedChannelCapacity), float64(expectedBandwidthUsage)))
	s.probeClusterId = s.prober.AddCluster(
		desiredRateBps,
		int(expectedBandwidthUsage),
		ProbeMinDuration,
		ProbeMaxDuration,
	)
	s.params.Logger.Debugw(
		"starting probe",
		"probeClusterId", s.probeClusterId,
		"current usage", expectedBandwidthUsage,
		"committed", s.committedChannelCapacity,
		"lastReceived", s.lastReceivedEstimate,
		"probeRateBps", probeRateBps,
		"goalBps", expectedBandwidthUsage+probeRateBps,
	)
}

func (s *StreamAllocator) resetProbe() {
	s.lastProbeStartTime = time.Now()

	s.resetProbeInterval()

	s.clearProbe()
}

func (s *StreamAllocator) clearProbe() {
	s.probeClusterId = ProbeClusterIdInvalid
	s.abortedProbeClusterId = ProbeClusterIdInvalid

	s.probeTrendObserved = false

	s.probeEndTime = time.Time{}

	s.probeChannelObserver = nil
}

func (s *StreamAllocator) backoffProbeInterval() {
	s.probeInterval = time.Duration(s.probeInterval.Seconds()*ProbeBackoffFactor) * time.Second
	if s.probeInterval > ProbeWaitMax {
		s.probeInterval = ProbeWaitMax
	}
}

func (s *StreamAllocator) resetProbeInterval() {
	s.probeInterval = ProbeWaitBase
}

func (s *StreamAllocator) stopProbe() {
	s.prober.Reset()
}

func (s *StreamAllocator) abortProbe() {
	s.abortedProbeClusterId = s.probeClusterId
	s.stopProbe()
}

func (s *StreamAllocator) isInProbe() bool {
	return s.probeClusterId != ProbeClusterIdInvalid
}

func (s *StreamAllocator) maybeProbe() {
	if time.Since(s.lastProbeStartTime) < s.probeInterval || s.probeClusterId != ProbeClusterIdInvalid {
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
	// boost deficient track farthest from desired layers
	for _, track := range s.getMaxDistanceSortedDeficient() {
		allocation, boosted := track.AllocateNextHigher(ChannelCapacityInfinity)
		if !boosted {
			continue
		}

		update := NewStreamStateUpdate()
		update.HandleStreamingChange(allocation.change, track)
		s.maybeSendUpdate(update)

		s.lastProbeStartTime = time.Now()
		break
	}
}

func (s *StreamAllocator) maybeProbeWithPadding() {
	// use deficient track farthest from desired layers to find how much to probe
	for _, track := range s.getMaxDistanceSortedDeficient() {
		transition, available := track.GetNextHigherTransition()
		if !available || transition.bandwidthDelta < 0 {
			continue
		}

		probeRateBps := (transition.bandwidthDelta * ProbePct) / 100
		if probeRateBps < ProbeMinBps {
			probeRateBps = ProbeMinBps
		}

		s.initProbe(probeRateBps)
		break
	}
}

func (s *StreamAllocator) getSorted() TrackSorter {
	var trackSorter TrackSorter
	for _, track := range s.videoTracks {
		if !track.IsManaged() {
			continue
		}

		trackSorter = append(trackSorter, track)
	}

	sort.Sort(trackSorter)

	return trackSorter
}

func (s *StreamAllocator) getMinDistanceSorted(exclude *Track) MinDistanceSorter {
	var minDistanceSorter MinDistanceSorter
	for _, track := range s.videoTracks {
		if !track.IsManaged() || track == exclude {
			continue
		}

		minDistanceSorter = append(minDistanceSorter, track)
	}

	sort.Sort(minDistanceSorter)

	return minDistanceSorter
}

func (s *StreamAllocator) getMaxDistanceSortedDeficient() MaxDistanceSorter {
	var maxDistanceSorter MaxDistanceSorter
	for _, track := range s.videoTracks {
		if !track.IsManaged() || !track.IsDeficient() {
			continue
		}

		maxDistanceSorter = append(maxDistanceSorter, track)
	}

	sort.Sort(maxDistanceSorter)

	return maxDistanceSorter
}

// ------------------------------------------------

type StreamState int

const (
	StreamStateActive StreamState = iota
	StreamStatePaused
)

type StreamStateInfo struct {
	ParticipantID livekit.ParticipantID
	TrackID       livekit.TrackID
	State         StreamState
}

type StreamStateUpdate struct {
	StreamStates []*StreamStateInfo
}

func NewStreamStateUpdate() *StreamStateUpdate {
	return &StreamStateUpdate{}
}

func (s *StreamStateUpdate) HandleStreamingChange(change VideoStreamingChange, track *Track) {
	switch change {
	case VideoStreamingChangePausing:
		s.StreamStates = append(s.StreamStates, &StreamStateInfo{
			ParticipantID: track.PublisherID(),
			TrackID:       track.ID(),
			State:         StreamStatePaused,
		})
	case VideoStreamingChangeResuming:
		s.StreamStates = append(s.StreamStates, &StreamStateInfo{
			ParticipantID: track.PublisherID(),
			TrackID:       track.ID(),
			State:         StreamStateActive,
		})
	}
}

func (s *StreamStateUpdate) Empty() bool {
	return len(s.StreamStates) == 0
}

// ------------------------------------------------

type Track struct {
	downTrack   *DownTrack
	source      livekit.TrackSource
	isSimulcast bool
	priority    uint8
	publisherID livekit.ParticipantID
	logger      logger.Logger

	maxLayers VideoLayers

	totalPackets       uint32
	totalRepeatedNacks uint32
}

func newTrack(
	downTrack *DownTrack,
	source livekit.TrackSource,
	isSimulcast bool,
	publisherID livekit.ParticipantID,
	logger logger.Logger,
) *Track {
	t := &Track{
		downTrack:   downTrack,
		source:      source,
		isSimulcast: isSimulcast,
		publisherID: publisherID,
		logger:      logger,
	}
	t.SetPriority(0)
	t.UpdateMaxLayers(downTrack.MaxLayers())

	return t
}

func (t *Track) SetPriority(priority uint8) bool {
	if priority == 0 {
		switch t.source {
		case livekit.TrackSource_SCREEN_SHARE:
			priority = PriorityDefaultScreenshare
		default:
			priority = PriorityDefaultVideo
		}
	}

	if t.priority == priority {
		return false
	}

	t.priority = priority
	return true
}

func (t *Track) Priority() uint8 {
	return t.priority
}

func (t *Track) DownTrack() *DownTrack {
	return t.downTrack
}

func (t *Track) IsManaged() bool {
	return t.source != livekit.TrackSource_SCREEN_SHARE || t.isSimulcast
}

func (t *Track) ID() livekit.TrackID {
	return livekit.TrackID(t.downTrack.ID())
}

func (t *Track) PublisherID() livekit.ParticipantID {
	return t.publisherID
}

func (t *Track) UpdateMaxLayers(layers VideoLayers) {
	t.maxLayers = layers
}

func (t *Track) WritePaddingRTP(bytesToSend int) int {
	return t.downTrack.WritePaddingRTP(bytesToSend)
}

func (t *Track) AllocateOptimal() VideoAllocation {
	return t.downTrack.AllocateOptimal()
}

func (t *Track) ProvisionalAllocatePrepare() {
	t.downTrack.ProvisionalAllocatePrepare()
}

func (t *Track) ProvisionalAllocate(availableChannelCapacity int64, layers VideoLayers, allowPause bool) int64 {
	return t.downTrack.ProvisionalAllocate(availableChannelCapacity, layers, allowPause)
}

func (t *Track) ProvisionalAllocateGetCooperativeTransition() VideoTransition {
	return t.downTrack.ProvisionalAllocateGetCooperativeTransition()
}

func (t *Track) ProvisionalAllocateGetBestWeightedTransition() VideoTransition {
	return t.downTrack.ProvisionalAllocateGetBestWeightedTransition()
}

func (t *Track) ProvisionalAllocateCommit() VideoAllocation {
	return t.downTrack.ProvisionalAllocateCommit()
}

func (t *Track) AllocateNextHigher(availableChannelCapacity int64) (VideoAllocation, bool) {
	return t.downTrack.AllocateNextHigher(availableChannelCapacity)
}

func (t *Track) GetNextHigherTransition() (VideoTransition, bool) {
	return t.downTrack.GetNextHigherTransition()
}

func (t *Track) Pause() VideoAllocation {
	return t.downTrack.Pause()
}

func (t *Track) IsDeficient() bool {
	return t.downTrack.IsDeficient()
}

func (t *Track) BandwidthRequested() int64 {
	return t.downTrack.BandwidthRequested()
}

func (t *Track) DistanceToDesired() int32 {
	return t.downTrack.DistanceToDesired()
}

func (t *Track) GetNackDelta() (uint32, uint32) {
	totalPackets, totalRepeatedNacks := t.downTrack.GetNackStats()

	packetDelta := totalPackets - t.totalPackets
	t.totalPackets = totalPackets

	nackDelta := totalRepeatedNacks - t.totalRepeatedNacks
	t.totalRepeatedNacks = totalRepeatedNacks

	return packetDelta, nackDelta
}

// ------------------------------------------------

type TrackSorter []*Track

func (t TrackSorter) Len() int {
	return len(t)
}

func (t TrackSorter) Swap(i, j int) {
	t[i], t[j] = t[j], t[i]
}

func (t TrackSorter) Less(i, j int) bool {
	//
	// TrackSorter is used to allocate layer-by-layer.
	// So, higher priority track should come earlier so that it gets an earlier shot at each layer
	//
	if t[i].priority != t[j].priority {
		return t[i].priority > t[j].priority
	}

	if t[i].maxLayers.spatial != t[j].maxLayers.spatial {
		return t[i].maxLayers.spatial > t[j].maxLayers.spatial
	}

	return t[i].maxLayers.temporal > t[j].maxLayers.temporal
}

// ------------------------------------------------

type MaxDistanceSorter []*Track

func (m MaxDistanceSorter) Len() int {
	return len(m)
}

func (m MaxDistanceSorter) Swap(i, j int) {
	m[i], m[j] = m[j], m[i]
}

func (m MaxDistanceSorter) Less(i, j int) bool {
	//
	// MaxDistanceSorter is used to find a deficient track to use for probing during recovery from congestion.
	// So, higher priority track should come earlier so that they have a chance to recover sooner.
	//
	if m[i].priority != m[j].priority {
		return m[i].priority > m[j].priority
	}

	return m[i].DistanceToDesired() > m[j].DistanceToDesired()
}

// ------------------------------------------------

type MinDistanceSorter []*Track

func (m MinDistanceSorter) Len() int {
	return len(m)
}

func (m MinDistanceSorter) Swap(i, j int) {
	m[i], m[j] = m[j], m[i]
}

func (m MinDistanceSorter) Less(i, j int) bool {
	//
	// MinDistanceSorter is used to find excess bandwidth in cooperative allocation.
	// So, lower priority track should come earlier so that they contribute bandwidth to higher priority tracks.
	//
	if m[i].priority != m[j].priority {
		return m[i].priority < m[j].priority
	}

	return m[i].DistanceToDesired() < m[j].DistanceToDesired()
}

// ------------------------------------------------

type ChannelTrend int

const (
	ChannelTrendNeutral ChannelTrend = iota
	ChannelTrendClearing
	ChannelTrendCongesting
)

func (c ChannelTrend) String() string {
	switch c {
	case ChannelTrendNeutral:
		return "NEUTRAL"
	case ChannelTrendClearing:
		return "CLEARING"
	case ChannelTrendCongesting:
		return "CONGESTING"
	default:
		return fmt.Sprintf("%d", int(c))
	}
}

type ChannelObserver struct {
	name   string
	logger logger.Logger

	estimateTrend *TrendDetector

	nackRatioThreshold float64
	packets            uint32
	repeatedNacks      uint32
}

func NewChannelObserver(
	name string,
	logger logger.Logger,
	estimateRequiredSamples int,
	nackRatioThreshold float64,
) *ChannelObserver {
	return &ChannelObserver{
		name:               name,
		logger:             logger,
		estimateTrend:      NewTrendDetector(name+"-estimate", logger, estimateRequiredSamples),
		nackRatioThreshold: nackRatioThreshold,
	}
}

func (c *ChannelObserver) Reset() {
	c.estimateTrend.Reset()

	c.packets = 0
	c.repeatedNacks = 0
}

func (c *ChannelObserver) SeedEstimate(estimate int64) {
	c.estimateTrend.Seed(estimate)
}

func (c *ChannelObserver) SeedNack(packets uint32, repeatedNacks uint32) {
	c.packets = packets
	c.repeatedNacks = repeatedNacks
}

func (c *ChannelObserver) AddEstimate(estimate int64) {
	c.estimateTrend.AddValue(estimate)
}

func (c *ChannelObserver) AddNack(packets uint32, repeatedNacks uint32) {
	c.packets += packets
	c.repeatedNacks += repeatedNacks
}

func (c *ChannelObserver) GetLowestEstimate() int64 {
	return c.estimateTrend.GetLowest()
}

func (c *ChannelObserver) GetHighestEstimate() int64 {
	return c.estimateTrend.GetHighest()
}

func (c *ChannelObserver) GetNackRatio() float64 {
	ratio := float64(0.0)
	if c.packets != 0 {
		ratio = float64(c.repeatedNacks) / float64(c.packets)
		if ratio > 1.0 {
			ratio = 1.0
		}
	}

	return ratio
}

func (c *ChannelObserver) GetTrend() ChannelTrend {
	estimateDirection := c.estimateTrend.GetDirection()
	nackRatio := c.GetNackRatio()

	switch {
	case estimateDirection == TrendDirectionDownward:
		c.logger.Debugw(
			"channel observer: estimate is trending downward",
			"estimate", c.estimateTrend.ToString(),
		)
		return ChannelTrendCongesting
	case nackRatio > c.nackRatioThreshold:
		c.logger.Debugw("channel observer: high rate of repeated NACKs", "ratio", nackRatio)
		return ChannelTrendCongesting
	case estimateDirection == TrendDirectionUpward:
		return ChannelTrendClearing
	}

	return ChannelTrendNeutral
}

// ------------------------------------------------

type TrendDirection int

const (
	TrendDirectionNeutral TrendDirection = iota
	TrendDirectionUpward
	TrendDirectionDownward
)

func (t TrendDirection) String() string {
	switch t {
	case TrendDirectionNeutral:
		return "NEUTRAL"
	case TrendDirectionUpward:
		return "UPWARD"
	case TrendDirectionDownward:
		return "DOWNWARD"
	default:
		return fmt.Sprintf("%d", int(t))
	}
}

type TrendDetector struct {
	name            string
	logger          logger.Logger
	requiredSamples int

	startTime    time.Time
	numSamples   int
	values       []int64
	lowestValue  int64
	highestValue int64

	direction TrendDirection
}

func NewTrendDetector(name string, logger logger.Logger, requiredSamples int) *TrendDetector {
	return &TrendDetector{
		name:            name,
		logger:          logger,
		requiredSamples: requiredSamples,
		direction:       TrendDirectionNeutral,
	}
}

func (t *TrendDetector) Reset() {
	t.values = nil
	t.lowestValue = int64(0)
	t.highestValue = int64(0)
}

func (t *TrendDetector) Seed(value int64) {
	if len(t.values) != 0 {
		return
	}

	t.values = append(t.values, value)
}

func (t *TrendDetector) AddValue(value int64) {
	t.numSamples++
	if t.lowestValue == 0 || value < t.lowestValue {
		t.lowestValue = value
	}
	if value > t.highestValue {
		t.highestValue = value
	}

	if len(t.values) == t.requiredSamples {
		t.values = t.values[1:]
	}
	t.values = append(t.values, value)

	t.updateDirection()
}

func (t *TrendDetector) GetLowest() int64 {
	return t.lowestValue
}

func (t *TrendDetector) GetHighest() int64 {
	return t.highestValue
}

func (t *TrendDetector) GetValues() []int64 {
	return t.values
}

func (t *TrendDetector) GetDirection() TrendDirection {
	return t.direction
}

func (t *TrendDetector) ToString() string {
	now := time.Now()
	elapsed := now.Sub(t.startTime).Seconds()
	str := fmt.Sprintf("t: %+v|%+v|%.2fs", t.startTime.Format(time.UnixDate), now.Format(time.UnixDate), elapsed)
	str += fmt.Sprintf(", v: %d|%d|%d|%+v", t.numSamples, t.lowestValue, t.highestValue, t.values)
	return str
}

func (t *TrendDetector) updateDirection() {
	if len(t.values) < t.requiredSamples {
		t.direction = TrendDirectionNeutral
		return
	}

	// using Kendall's Tau to find trend
	concordantPairs := 0
	discordantPairs := 0

	for i := 0; i < len(t.values)-1; i++ {
		for j := i + 1; j < len(t.values); j++ {
			if t.values[i] < t.values[j] {
				concordantPairs++
			} else if t.values[i] > t.values[j] {
				discordantPairs++
			}
		}
	}

	if (concordantPairs + discordantPairs) == 0 {
		t.direction = TrendDirectionNeutral
		return
	}

	t.direction = TrendDirectionNeutral
	kt := (float64(concordantPairs) - float64(discordantPairs)) / (float64(concordantPairs) + float64(discordantPairs))
	switch {
	case kt > 0:
		t.direction = TrendDirectionUpward
	case kt < 0:
		t.direction = TrendDirectionDownward
	}
}

// ------------------------------------------------

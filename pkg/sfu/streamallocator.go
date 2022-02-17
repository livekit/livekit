package sfu

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/config"
)

const (
	ChannelCapacityInfinity = 100 * 1000 * 1000 // 100 Mbps

	NumRequiredEstimatesNonProbe = 8
	NumRequiredEstimatesProbe    = 3

	NumRequiredNacksNonProbe = 10
	NumRequiredNacksProbe    = 5

	ProbeWaitBase      = 5 * time.Second
	ProbeBackoffFactor = 1.5
	ProbeWaitMax       = 30 * time.Second
	ProbeSettleWait    = 250
	ProbeTrendWait     = 2 * time.Second

	ProbePct         = 120
	ProbeMinBps      = 200 * 1000 // 200 kbps
	ProbeMinDuration = 20 * time.Second
	ProbeMaxDuration = 21 * time.Second

	GratuitousProbeHeadroomBps = 1 * 1000 * 1000 // if headroom > 1 Mbps, don't probe
	GratuitousProbePct         = 10
	GratuitousProbeMinBps      = 100 * 1000 // 100 kbps
	GratuitousProbeMaxBps      = 300 * 1000 // 300 kbps
	GratuitousProbeMinDuration = 500 * time.Millisecond
	GratuitousProbeMaxDuration = 600 * time.Millisecond

	AudioLossWeight = 0.75
	VideoLossWeight = 0.25
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
	SignalEstimate
	SignalTargetBitrate
	SignalReceiverReport
	SignalAvailableLayersChange
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
	case SignalEstimate:
		return "ESTIMATE"
	case SignalTargetBitrate:
		return "TARGET_BITRATE"
	case SignalReceiverReport:
		return "RECEIVER_REPORT"
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

	lastNackTime time.Time

	audioTracks              map[livekit.TrackID]*Track
	videoTracks              map[livekit.TrackID]*Track
	exemptVideoTracksSorted  TrackSorter
	managedVideoTracksSorted TrackSorter

	state State

	eventChMu sync.RWMutex
	eventCh   chan Event

	isStopped utils.AtomicFlag
}

func NewStreamAllocator(params StreamAllocatorParams) *StreamAllocator {
	s := &StreamAllocator{
		params: params,
		prober: NewProber(ProberParams{
			Logger: params.Logger,
		}),
		channelObserver: NewChannelObserver("non-probe", params.Logger, NumRequiredEstimatesNonProbe, NumRequiredNacksNonProbe),
		audioTracks:     make(map[livekit.TrackID]*Track),
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
	if !s.isStopped.TrySet(true) {
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
		downTrack.OnSubscriptionChanged(s.onSubscriptionChanged)
		downTrack.OnSubscribedLayersChanged(s.onSubscribedLayersChanged)
		downTrack.OnPacketSent(s.onPacketSent)
	}
	downTrack.AddReceiverReportListener(s.onReceiverReport)
}

func (s *StreamAllocator) RemoveTrack(downTrack *DownTrack) {
	s.postEvent(Event{
		Signal:    SignalRemoveTrack,
		DownTrack: downTrack,
	})
}

func (s *StreamAllocator) resetState() {
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

// called when a new RTCP Receiver Report is received
func (s *StreamAllocator) onReceiverReport(downTrack *DownTrack, rr *rtcp.ReceiverReport) {
	s.postEvent(Event{
		Signal:    SignalReceiverReport,
		DownTrack: downTrack,
		Data:      rr,
	})
}

// called when feeding track's layer availability changes
func (s *StreamAllocator) onAvailableLayersChanged(downTrack *DownTrack) {
	s.postEvent(Event{
		Signal:    SignalAvailableLayersChange,
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
	if s.isStopped.Get() {
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
		if s.isStopped.Get() {
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
	case SignalEstimate:
		s.handleSignalEstimate(event)
	case SignalTargetBitrate:
		s.handleSignalTargetBitrate(event)
	case SignalReceiverReport:
		s.handleSignalReceiverReport(event)
	case SignalAvailableLayersChange:
		s.handleSignalAvailableLayersChange(event)
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
	params, _ := event.Data.(AddTrackParams)
	isManaged := (params.Source != livekit.TrackSource_SCREEN_SHARE && params.Source != livekit.TrackSource_SCREEN_SHARE_AUDIO) || params.IsSimulcast
	track := newTrack(event.DownTrack, isManaged, params.PublisherID, s.params.Logger)

	trackID := livekit.TrackID(event.DownTrack.ID())
	switch event.DownTrack.Kind() {
	case webrtc.RTPCodecTypeAudio:
		s.audioTracks[trackID] = track
	case webrtc.RTPCodecTypeVideo:
		s.videoTracks[trackID] = track

		if isManaged {
			s.managedVideoTracksSorted = append(s.managedVideoTracksSorted, track)
			sort.Sort(s.managedVideoTracksSorted)
		} else {
			s.exemptVideoTracksSorted = append(s.exemptVideoTracksSorted, track)
			sort.Sort(s.exemptVideoTracksSorted)
		}

		s.allocateTrack(track)
	}
}

func (s *StreamAllocator) handleSignalRemoveTrack(event *Event) {
	trackID := livekit.TrackID(event.DownTrack.ID())
	switch event.DownTrack.Kind() {
	case webrtc.RTPCodecTypeAudio:
		if _, ok := s.audioTracks[trackID]; !ok {
			return
		}

		delete(s.audioTracks, trackID)
	case webrtc.RTPCodecTypeVideo:
		track, ok := s.videoTracks[trackID]
		if !ok {
			return
		}

		delete(s.videoTracks, trackID)

		if track.IsManaged() {
			n := len(s.managedVideoTracksSorted)
			for idx, videoTrack := range s.managedVideoTracksSorted {
				if videoTrack.DownTrack() == event.DownTrack {
					s.managedVideoTracksSorted[idx] = s.managedVideoTracksSorted[n-1]
					s.managedVideoTracksSorted = s.managedVideoTracksSorted[:n-1]
					break
				}
			}
			sort.Sort(s.managedVideoTracksSorted)
		} else {
			n := len(s.exemptVideoTracksSorted)
			for idx, videoTrack := range s.exemptVideoTracksSorted {
				if videoTrack.DownTrack() == event.DownTrack {
					s.exemptVideoTracksSorted[idx] = s.exemptVideoTracksSorted[n-1]
					s.exemptVideoTracksSorted = s.exemptVideoTracksSorted[:n-1]
					break
				}
			}
			sort.Sort(s.exemptVideoTracksSorted)
		}

		// re-initialize estimate if all managed tracks are removed, let it get a fresh start
		if len(s.managedVideoTracksSorted) == 0 {
			s.resetState()
			return
		}

		// LK-TODO: use any saved bandwidth to re-distribute
		s.adjustState()
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
	if len(s.managedVideoTracksSorted) == 0 {
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

// LK-TODO-START
// Receiver report stats are not used in the current implementation.
//
// The idea is to use a loss/rtt based estimator and compare against REMB like outlined here
// https://datatracker.ietf.org/doc/html/draft-ietf-rmcat-gcc-02#section-6
//
// But the implementation could get quite tricky. So, a separate PR dedicated effort for that
// is required. Something like from Chrome, but hopefully much less complicated :-)
// https://source.chromium.org/chromium/chromium/src/+/main:third_party/webrtc/modules/congestion_controller/goog_cc/loss_based_bandwidth_estimation.cc;bpv=0;bpt=1
// LK-TODO-END
func (s *StreamAllocator) handleSignalReceiverReport(event *Event) {
	var track *Track
	ok := false

	trackID := livekit.TrackID(event.DownTrack.ID())
	switch event.DownTrack.Kind() {
	case webrtc.RTPCodecTypeAudio:
		track, ok = s.audioTracks[trackID]
	case webrtc.RTPCodecTypeVideo:
		track, ok = s.videoTracks[trackID]
	}
	if !ok {
		return
	}

	rr, _ := event.Data.(*rtcp.ReceiverReport)
	track.UpdatePacketStats(rr)
}

func (s *StreamAllocator) handleSignalAvailableLayersChange(event *Event) {
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
	if track.IsManaged() {
		sort.Sort(s.managedVideoTracksSorted)
	} else {
		sort.Sort(s.exemptVideoTracksSorted)
	}

	s.allocateTrack(track)
}

func (s *StreamAllocator) handleSignalPeriodicPing(event *Event) {
	// finalize probe if necessary
	if s.isInProbe() && !s.probeEndTime.IsZero() && time.Now().After(s.probeEndTime) {
		s.finalizeProbe()
	}

	// catch up on all optimistically streamed tracks
	s.finalizeTracks()

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
	for _, videoTrack := range s.managedVideoTracksSorted {
		if videoTrack.IsDeficient() {
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
		s.params.Logger.Debugw("SA_DEBUG, estimate in probe", "estimate", receivedEstimate) // REMOVE
		s.handleNewEstimateInProbe()
	} else {
		s.params.Logger.Debugw("SA_DEBUG, estimate in non-probe", "estimate", receivedEstimate) // REMOVE
		s.handleNewEstimateInNonProbe()
	}
}

func (s *StreamAllocator) handleNewEstimateInProbe() {
	s.probeChannelObserver.AddEstimate(s.lastReceivedEstimate)
	s.probeChannelObserver.AddNack(s.getNackRate())
	trend := s.probeChannelObserver.GetTrend()
	if trend != ChannelObserverTrendNeutral {
		s.probeTrendObserved = true
	}
	switch {
	case s.abortedProbeClusterId != ProbeClusterIdInvalid:
		return
	case !s.probeTrendObserved && time.Since(s.lastProbeStartTime) > ProbeTrendWait:
		//
		// More of a safety net.
		// In rare cases, the estimate gets stuck. Prevent from probe running amok
		// LK-TODO: Need more testing this here and ensure that probe does not cause a lot of damage
		//
		s.params.Logger.Debugw("probe: aborting, no trend")
		s.abortProbe()
	case trend == ChannelObserverTrendDownward:
		// stop immediately if estimate falls below the previously committed estimate, the probe is congesting channel more
		s.params.Logger.Debugw("probe: aborting, estimate is trending downward")
		s.abortProbe()
	case s.probeChannelObserver.GetHighestEstimate() > s.probeGoalBps:
		// reached goal, stop probing
		s.params.Logger.Debugw("probe: stopping, goal reached")
		s.stopProbe()
	}
}

func (s *StreamAllocator) handleNewEstimateInNonProbe() {
	s.channelObserver.AddEstimate(s.lastReceivedEstimate)
	s.channelObserver.AddNack(s.getNackRate())
	trend := s.channelObserver.GetTrend()
	if trend != ChannelObserverTrendDownward {
		return
	}

	s.params.Logger.Infow(
		"estimate trending down, updating channel capacity",
		"old(bps)", s.committedChannelCapacity,
		"new(bps)", s.lastReceivedEstimate,
	)
	s.committedChannelCapacity = s.lastReceivedEstimate

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
		allocation := track.Allocate(ChannelCapacityInfinity, s.params.Config.AllowPause)
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
	var minDistanceSorted MinDistanceSorter
	for _, t := range s.managedVideoTracksSorted {
		if t != track {
			minDistanceSorted = append(minDistanceSorted, t)
		}
	}
	sort.Sort(minDistanceSorted)

	bandwidthAcquired := int64(0)
	var contributingTracks []*Track

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

	if s.committedChannelCapacity == highestEstimateInProbe {
		// no increase for whatever reason, don't backoff though
		return
	}

	// probe estimate is higher, commit it and try allocate deficient tracks
	s.params.Logger.Infow(
		"successful probe, updating channel capacity",
		"old(bps)", s.committedChannelCapacity,
		"new(bps)", highestEstimateInProbe,
	)
	s.committedChannelCapacity = highestEstimateInProbe

	availableChannelCapacity := s.committedChannelCapacity - s.getExpectedBandwidthUsage()
	if availableChannelCapacity <= 0 {
		return
	}

	var maxDistanceSorted MaxDistanceSorter
	for _, track := range s.managedVideoTracksSorted {
		maxDistanceSorted = append(maxDistanceSorted, track)
	}
	sort.Sort(maxDistanceSorted)

	update := NewStreamStateUpdate()

	for _, track := range maxDistanceSorted {
		if !track.IsDeficient() {
			continue
		}

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

	//
	// This pass is just to find out if there is any leftover channel capacity.
	// Infinite channel capacity is given so that exempt tracks do not stall
	//
	for _, track := range s.exemptVideoTracksSorted {
		allocation := track.Allocate(ChannelCapacityInfinity, s.params.Config.AllowPause)
		update.HandleStreamingChange(allocation.change, track)

		// LK-TODO: optimistic allocation before bitrate is available will return 0. How to account for that?
		availableChannelCapacity -= allocation.bandwidthRequested
	}

	if availableChannelCapacity < 0 {
		availableChannelCapacity = 0
	}
	if availableChannelCapacity == 0 && s.params.Config.AllowPause {
		// nothing left for managed tracks, pause them all
		for _, track := range s.managedVideoTracksSorted {
			allocation := track.Pause()
			update.HandleStreamingChange(allocation.change, track)
		}
	} else {
		for _, track := range s.managedVideoTracksSorted {
			track.ProvisionalAllocatePrepare()
		}

		for spatial := int32(0); spatial <= DefaultMaxLayerSpatial; spatial++ {
			for temporal := int32(0); temporal <= DefaultMaxLayerTemporal; temporal++ {
				layers := VideoLayers{
					spatial:  spatial,
					temporal: temporal,
				}

				for _, track := range s.managedVideoTracksSorted {
					usedChannelCapacity := track.ProvisionalAllocate(availableChannelCapacity, layers, s.params.Config.AllowPause)
					availableChannelCapacity -= usedChannelCapacity
					if availableChannelCapacity < 0 {
						availableChannelCapacity = 0
					}
				}
			}
		}

		for _, track := range s.managedVideoTracksSorted {
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

func (s *StreamAllocator) finalizeTracks() {
	for _, t := range s.exemptVideoTracksSorted {
		t.FinalizeAllocate()
	}

	for _, t := range s.managedVideoTracksSorted {
		t.FinalizeAllocate()
	}

	s.adjustState()
}

func (s *StreamAllocator) getExpectedBandwidthUsage() int64 {
	expected := int64(0)
	for _, track := range s.videoTracks {
		expected += track.BandwidthRequested()
	}

	return expected
}

func (s *StreamAllocator) getNackRate() int64 {
	nacks := uint32(0)
	for _, track := range s.videoTracks {
		nacks += track.GetNackDelta()
	}

	now := time.Now()
	var elapsed time.Duration
	if !s.lastNackTime.IsZero() {
		elapsed = now.Sub(s.lastNackTime)
	}
	s.lastNackTime = now

	if elapsed == 0 {
		return 0
	}

	return int64(nacks) * 1000 / elapsed.Milliseconds()
}

// LK-TODO: unused till loss based estimation is done, but just a sample impl of weighting audio higher
func (s *StreamAllocator) calculateLoss() float32 {
	packetsAudio := uint32(0)
	packetsLostAudio := uint32(0)
	for _, track := range s.audioTracks {
		packets, packetsLost := track.GetPacketStats()

		packetsAudio += packets
		packetsLostAudio += packetsLost
	}

	audioLossPct := float32(0.0)
	if packetsAudio != 0 {
		audioLossPct = (float32(packetsLostAudio) * 100.0) / float32(packetsAudio)
	}

	packetsVideo := uint32(0)
	packetsLostVideo := uint32(0)
	for _, track := range s.videoTracks {
		packets, packetsLost := track.GetPacketStats()

		packetsVideo += packets
		packetsLostVideo += packetsLost
	}

	videoLossPct := float32(0.0)
	if packetsVideo != 0 {
		videoLossPct = (float32(packetsLostVideo) * 100.0) / float32(packetsVideo)
	}

	return AudioLossWeight*audioLossPct + VideoLossWeight*videoLossPct
}

func (s *StreamAllocator) initProbe(goalBps int64) {
	s.lastProbeStartTime = time.Now()

	s.probeGoalBps = goalBps

	s.abortedProbeClusterId = ProbeClusterIdInvalid

	s.probeTrendObserved = false

	s.probeEndTime = time.Time{}

	s.probeChannelObserver = NewChannelObserver("probe", s.params.Logger, NumRequiredEstimatesProbe, NumRequiredNacksProbe)
	s.probeChannelObserver.SeedEstimate(s.lastReceivedEstimate)
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
	var maxDistanceSorted MaxDistanceSorter
	for _, track := range s.managedVideoTracksSorted {
		maxDistanceSorted = append(maxDistanceSorted, track)
	}
	sort.Sort(maxDistanceSorted)

	// boost deficient track farthest from desired layers
	for _, track := range maxDistanceSorted {
		if !track.IsDeficient() {
			continue
		}

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
	var maxDistanceSorted MaxDistanceSorter
	for _, track := range s.managedVideoTracksSorted {
		maxDistanceSorted = append(maxDistanceSorted, track)
	}
	sort.Sort(maxDistanceSorted)

	// use deficient track farthest from desired layers to find how much to probe
	for _, track := range maxDistanceSorted {
		if !track.IsDeficient() {
			continue
		}

		transition, available := track.GetNextHigherTransition()
		if !available || transition.bandwidthDelta < 0 {
			continue
		}

		probeRateBps := (transition.bandwidthDelta * ProbePct) / 100
		if probeRateBps < ProbeMinBps {
			probeRateBps = ProbeMinBps
		}

		s.initProbe(s.getExpectedBandwidthUsage() + probeRateBps)
		s.probeClusterId = s.prober.AddCluster(
			int(s.committedChannelCapacity+probeRateBps),
			int(s.getExpectedBandwidthUsage()),
			ProbeMinDuration,
			ProbeMaxDuration,
		)
		break
	}
}

func (s *StreamAllocator) maybeGratuitousProbe() bool {
	// don't gratuitously probe too often
	if time.Since(s.lastProbeStartTime) < s.probeInterval || len(s.managedVideoTracksSorted) == 0 || s.probeClusterId != ProbeClusterIdInvalid {
		return false
	}

	// use last received estimate for gratuitous probing base as
	// more updates may have been received since the last commit
	expectedRateBps := s.getExpectedBandwidthUsage()
	headroomBps := s.lastReceivedEstimate - expectedRateBps
	if headroomBps > GratuitousProbeHeadroomBps {
		return false
	}

	probeRateBps := (s.lastReceivedEstimate * GratuitousProbePct) / 100
	if probeRateBps < GratuitousProbeMinBps {
		probeRateBps = GratuitousProbeMinBps
	}
	if probeRateBps > GratuitousProbeMaxBps {
		probeRateBps = GratuitousProbeMaxBps
	}

	s.initProbe(expectedRateBps + probeRateBps)
	s.probeClusterId = s.prober.AddCluster(
		int(s.lastReceivedEstimate+probeRateBps),
		int(expectedRateBps),
		GratuitousProbeMinDuration,
		GratuitousProbeMaxDuration,
	)

	return true
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
	isManaged   bool
	publisherID livekit.ParticipantID
	logger      logger.Logger

	highestSN       uint32
	packetsLost     uint32
	lastHighestSN   uint32
	lastPacketsLost uint32

	maxLayers VideoLayers

	totalRepeatedNACKs uint32
}

func newTrack(downTrack *DownTrack, isManaged bool, publisherID livekit.ParticipantID, logger logger.Logger) *Track {
	t := &Track{
		downTrack:   downTrack,
		isManaged:   isManaged,
		publisherID: publisherID,
		logger:      logger,
	}
	t.UpdateMaxLayers(downTrack.MaxLayers())

	return t
}

func (t *Track) DownTrack() *DownTrack {
	return t.downTrack
}

func (t *Track) IsManaged() bool {
	return t.isManaged
}

func (t *Track) ID() livekit.TrackID {
	return livekit.TrackID(t.downTrack.ID())
}

func (t *Track) PublisherID() livekit.ParticipantID {
	return t.publisherID
}

// LK-TODO this should probably be maintained in downTrack and this module can query what it needs
func (t *Track) UpdatePacketStats(rr *rtcp.ReceiverReport) {
	t.lastHighestSN = t.highestSN
	t.lastPacketsLost = t.packetsLost

	for _, report := range rr.Reports {
		if report.LastSequenceNumber > t.highestSN {
			t.highestSN = report.LastSequenceNumber
		}
		if report.TotalLost > t.packetsLost {
			t.packetsLost = report.TotalLost
		}
	}
}

func (t *Track) UpdateMaxLayers(layers VideoLayers) {
	t.maxLayers = layers
}

func (t *Track) GetPacketStats() (uint32, uint32) {
	return t.highestSN - t.lastHighestSN, t.packetsLost - t.lastPacketsLost
}

func (t *Track) WritePaddingRTP(bytesToSend int) int {
	return t.downTrack.WritePaddingRTP(bytesToSend)
}

func (t *Track) Allocate(availableChannelCapacity int64, allowPause bool) VideoAllocation {
	return t.downTrack.Allocate(availableChannelCapacity, allowPause)
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

func (t *Track) FinalizeAllocate() {
	t.downTrack.FinalizeAllocate()
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

func (t *Track) GetNackDelta() uint32 {
	totalPackets, totalRepeatedNACKs := t.downTrack.GetNackStats()
	t.logger.Debugw("SA_DEBUG, nack stats", "track", t.ID(), "packets", totalPackets, "repeatedNACKs", totalRepeatedNACKs) // REMOVE

	delta := totalRepeatedNACKs - t.totalRepeatedNACKs
	t.totalRepeatedNACKs = totalRepeatedNACKs

	return delta
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
	return m[i].DistanceToDesired() < m[j].DistanceToDesired()
}

// ------------------------------------------------

type ChannelObserverTrend int

const (
	ChannelObserverTrendNeutral ChannelObserverTrend = iota
	ChannelObserverTrendUpward
	ChannelObserverTrendDownward
)

func (c ChannelObserverTrend) String() string {
	switch c {
	case ChannelObserverTrendNeutral:
		return "NEUTRAL"
	case ChannelObserverTrendUpward:
		return "UPWARD"
	case ChannelObserverTrendDownward:
		return "DOWNWARD"
	default:
		return fmt.Sprintf("%d", int(c))
	}
}

type ChannelObserver struct {
	name   string
	logger logger.Logger

	estimateTrend *TrendDetector
	nackTrend     *TrendDetector
}

func NewChannelObserver(
	name string,
	logger logger.Logger,
	estimateRequiredSamples int,
	nackRequiredSamples int,
) *ChannelObserver {
	return &ChannelObserver{
		name:          name,
		logger:        logger,
		estimateTrend: NewTrendDetector(name+"-estimate", logger, estimateRequiredSamples),
		nackTrend:     NewTrendDetector(name+"-nack", logger, nackRequiredSamples),
	}
}

func (c *ChannelObserver) Reset() {
	c.estimateTrend.Reset()
	c.nackTrend.Reset()
}

func (c *ChannelObserver) SeedEstimate(estimate int64) {
	c.estimateTrend.Seed(estimate)
}

func (c *ChannelObserver) SeeNack(nack int64) {
	c.nackTrend.Seed(nack)
}

func (c *ChannelObserver) AddEstimate(estimate int64) {
	c.estimateTrend.AddValue(estimate)
}

func (c *ChannelObserver) AddNack(nack int64) {
	c.nackTrend.AddValue(nack)
}

func (c *ChannelObserver) GetLowestEstimate() int64 {
	return c.estimateTrend.GetLowest()
}

func (c *ChannelObserver) GetHighestEstimate() int64 {
	return c.estimateTrend.GetHighest()
}

func (c *ChannelObserver) GetLowestNack() int64 {
	return c.nackTrend.GetLowest()
}

func (c *ChannelObserver) GetHighestNack() int64 {
	return c.nackTrend.GetHighest()
}

func (c *ChannelObserver) GetTrend() ChannelObserverTrend {
	estimateDirection := c.estimateTrend.GetDirection()
	nackDirection := c.nackTrend.GetDirection()

	switch {
	case estimateDirection == TrendDirectionDownward || nackDirection == TrendDirectionUpward:
		if estimateDirection == TrendDirectionDownward {
			c.logger.Debugw("channel observer: estimate is trending downward")
		}
		if nackDirection == TrendDirectionUpward {
			c.logger.Debugw("channel observer: nack is trending upward")
		}
		return ChannelObserverTrendDownward
	case estimateDirection == TrendDirectionUpward:
		return ChannelObserverTrendUpward
	}

	return ChannelObserverTrendNeutral
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

	values       []int64
	lowestvalue  int64
	highestvalue int64

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
	t.lowestvalue = int64(0)
	t.highestvalue = int64(0)
}

func (t *TrendDetector) Seed(value int64) {
	if len(t.values) != 0 {
		return
	}

	t.values = append(t.values, value)
}

func (t *TrendDetector) AddValue(value int64) {
	if t.lowestvalue == 0 || value < t.lowestvalue {
		t.lowestvalue = value
	}
	if value > t.highestvalue {
		t.highestvalue = value
	}

	if len(t.values) == t.requiredSamples {
		t.values = t.values[1:]
	}
	t.values = append(t.values, value)

	t.updateDirection()
}

func (t *TrendDetector) GetLowest() int64 {
	return t.lowestvalue
}

func (t *TrendDetector) GetHighest() int64 {
	return t.highestvalue
}

func (t *TrendDetector) GetDirection() TrendDirection {
	return t.direction
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

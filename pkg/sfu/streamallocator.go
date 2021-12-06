//
// Design of StreamAllocator
//
// Each participant uses one peer connection for all downstream
// traffic. It is possible that the downstream peer connection
// gets congested. In such an event, the SFU (sender on that
// peer connection) should take measures to mitigate the
// media loss and latency that would result from such a congestion.
//
// This module is supposed to aggregate down stream tracks and
// drive bandwidth allocation with the goals of
//   - Try and send highest quality media
//   - React as quickly as possible to mitigate congestion
//
// Setup:
// ------
// The following should be done to set up a stream allocator
//   - There will be one of these per subscriber peer connection.
//     Created in livekit-sever/transport.go for subscriber type
//     peer connections.
//   - In `AddSubscribedTrack` of livekit-server/participant.go, the created
//     downTrack is added to the stream allocator.
//   - In `RemoveSubscribedTrack` of livekit-server/participant.go,
//     the downTrack is removed from the stream allocator.
//   - Both video and audio tracks are added to this module. Although the
//     stream allocator does not act on audio track forwarding, audio track
//     information like loss rate may be used to adjust available bandwidth.
//
// Callbacks:
// ----------
// StreamAllocator registers the following callbacks on all registered down tracks
//   - OnREMB: called when down track receives RTCP REMB. Note that REMB is a
//     peer connection level aggregate metric. But, it contains all the SSRCs
//     used in the calculation of that REMB. So, there could be multiple
//     callbacks per RTCP REMB received (one each from down track pertaining
//     to the contained SSRCs) with the same estimated channel capacity.
//   - AddReceiverReportListener: called when down track received RTCP RR (Receiver Report).
//   - OnAvailableLayersChanged: called when the feeding track changes its layers.
//     This could happen due to publisher throttling layers due to upstream congestion
//     in its path.
//   - OnSubscriptionChanged: called when a down track settings are changed resulting
//     from client side requests (muting/unmuting)
//   - OnSubscribedLayersChanged: called when a down track settings are changed resulting
//     from client side requests (limiting maximum layer).
//   - OnPacketSent: called when a media packet is forwarded by the down track. As
//     this happens once per forwarded packet, processing in this callback should be
//     kept to a minimum.
//
// The following may be needed depending on the StreamAllocator algorithm
//    - OnBitrateUpdate: called periodically to update the bit rate at which a down track
//      is forwarding. This can be used to measure any overshoot and adjust allocations
//      accordingly. This may have granular information like primary bitrate, retransmitted
//      bitrate and padding bitrate.
//
// State machine:
// --------------
// The most critical component. It should monitor current state of channel and
// take actions to provide the best user experience by striving to achieve the
// goals outlined earlier
//
//  States:
//  ------
//  - StateStable: When all streams are forwarded at their optimal requested layers.
//
//                 Before the first estimate is committed, estimated channel capacity
//                 is initialized to some arbitrarily high value to start streaming
//                 immediately. Serves two purposes
//                   1. Gives the bandwidth estimation algorithms data
//                   2. Start streaming as soon as a user joins. Imagine
//                      a user joining a room with 10 participants already
//                      in it. That user should start receiving streams
//                      from everybody as soon as possible.
//
//                 In this state, it is also possible to probe for extra capacity
//                 to be prepared for cases like new participant joining and streaming OR
//                 an existing participant starting a new stream like enabling camera or
//                 screen share.
//  - StateDeficient: When at least one stream is not able to forward optimal requested layers.
//
//  Signals:
//  -------
//  Each state should take action based on these signals and advance the state machine based
//  on the result of the action.
//  - SignalAddTrack: A new track has been added.
//  - SignalRemoveTrack: An existing track has been removed.
//  - SignalEstimate: A new channel capacity estimate has been received.
//                    Note that when channel gets congested, it is possible to
//                    get several of these in a very short time window.
//  - SignalReceiverReport: An RTCP Receiver Report received from some down track.
//  - SignalAvailableLayersChange: Available layers of publisher changed.
//  - SignalSubscriptionChange: Subscription changed (mute/unmute)
//  - SignalSubscribedLayersChange: Subscribed layers changed (requested layers changed).
//  - SignalPeriodicPing: Periodic ping.
//  - SignalSendProbe: Request from Prober to send padding probes.
//
// There are several interesting challenges which are documented in relevant code below.
//
package sfu

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/livekit/protocol/logger"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
)

const (
	ChannelCapacityInfinity = 100 * 1000 * 1000 // 100 Mbps

	EstimateEpsilon = 2000 // 2 kbps

	GratuitousProbeHeadroomBps   = 1 * 1000 * 1000 // if headroom > 1 Mbps, don't probe
	GratuitousProbePct           = 10
	GratuitousProbeMinBps        = 100 * 1000 // 100 kbps
	GratuitousProbeMaxBps        = 300 * 1000 // 300 kbps
	GratuitousProbeMinDurationMs = 500 * time.Millisecond
	GratuitousProbeMaxDurationMs = 600 * time.Millisecond

	AudioLossWeight = 0.75
	VideoLossWeight = 0.25

	// LK-TODO-START
	// These constants will definitely require more tweaking.
	// In fact, simple time tresholded rules most proably will not be enough.
	// LK-TODO-END
	EstimateCommitMs          = 2 * 1000 * time.Millisecond // 2 seconds
	ProbeWaitMs               = 8 * 1000 * time.Millisecond // 8 seconds
	BoostWaitMs               = 5 * 1000 * time.Millisecond // 5 seconds
	GratuitousProbeWaitMs     = 8 * 1000 * time.Millisecond // 8 seconds
	GratuitousProbeMoreWaitMs = 5 * 1000 * time.Millisecond // 5 seconds
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
	SignalReceiverReport
	SignalAvailableLayersChange
	SignalSubscriptionChange
	SignalSubscribedLayersChange
	SignalPeriodicPing
	SignalSendProbe
)

func (s Signal) String() string {
	switch s {
	case SignalAddTrack:
		return "ADD_TRACK"
	case SignalRemoveTrack:
		return "REMOVE_TRACK"
	case SignalEstimate:
		return "ESTIMATE"
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
	default:
		return fmt.Sprintf("%d", int(s))
	}
}

type StreamAllocatorParams struct {
	ParticipantID string
	Logger        logger.Logger
}

type StreamAllocator struct {
	participantID string
	logger        logger.Logger

	onStreamStateChange func(update *StreamStateUpdate) error

	trackingSSRC             uint32
	committedChannelCapacity int64
	lastCommitTime           time.Time
	prevReceivedEstimate     int64
	receivedEstimate         int64
	lastEstimateDecreaseTime time.Time

	lastBoostTime time.Time

	lastGratuitousProbeTime time.Time

	audioTracks       map[string]*Track
	videoTracks       map[string]*Track
	videoTracksSorted TrackSorter

	prober *Prober

	state State

	chMu      sync.RWMutex
	eventCh   chan Event
	runningCh chan struct{}
}

type Event struct {
	Signal    Signal
	DownTrack *DownTrack
	Data      interface{}
}

func NewStreamAllocator(params StreamAllocatorParams) *StreamAllocator {
	s := &StreamAllocator{
		participantID: params.ParticipantID,
		logger:        params.Logger,
		audioTracks:   make(map[string]*Track),
		videoTracks:   make(map[string]*Track),
		prober: NewProber(ProberParams{
			ParticipantID: params.ParticipantID,
			Logger:        params.Logger,
		}),
		eventCh:   make(chan Event, 20),
		runningCh: make(chan struct{}),
	}

	s.initializeEstimate()

	s.prober.OnSendProbe(s.onSendProbe)

	return s
}

func (s *StreamAllocator) Start() {
	go s.processEvents()
	go s.ping()
}

func (s *StreamAllocator) Stop() {
	s.chMu.Lock()
	defer s.chMu.Unlock()

	close(s.runningCh)
	close(s.eventCh)
}

func (s *StreamAllocator) OnStreamStateChange(f func(update *StreamStateUpdate) error) {
	s.onStreamStateChange = f
}

func (s *StreamAllocator) AddTrack(downTrack *DownTrack) {
	s.postEvent(Event{
		Signal:    SignalAddTrack,
		DownTrack: downTrack,
	})

	if downTrack.Kind() == webrtc.RTPCodecTypeVideo {
		downTrack.OnREMB(s.onREMB)
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

func (s *StreamAllocator) initializeEstimate() {
	s.committedChannelCapacity = ChannelCapacityInfinity
	s.lastCommitTime = time.Now().Add(-EstimateCommitMs)
	s.receivedEstimate = ChannelCapacityInfinity
	s.lastEstimateDecreaseTime = time.Now()

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

func (s *StreamAllocator) postEvent(event Event) {
	s.chMu.RLock()
	defer s.chMu.RUnlock()

	if !s.isRunning() {
		return
	}

	s.eventCh <- event
}

func (s *StreamAllocator) processEvents() {
	for event := range s.eventCh {
		s.handleEvent(&event)
	}
}

func (s *StreamAllocator) isRunning() bool {
	select {
	case <-s.runningCh:
		return false
	default:
		return true
	}
}

func (s *StreamAllocator) ping() {
	ticker := time.NewTicker(time.Second)

	for s.isRunning() {
		<-ticker.C
		if !s.isRunning() {
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
	}
}

func (s *StreamAllocator) handleSignalAddTrack(event *Event) {
	track := newTrack(event.DownTrack)
	switch event.DownTrack.Kind() {
	case webrtc.RTPCodecTypeAudio:
		s.audioTracks[event.DownTrack.ID()] = track
	case webrtc.RTPCodecTypeVideo:
		s.videoTracks[event.DownTrack.ID()] = track

		s.videoTracksSorted = append(s.videoTracksSorted, track)
		sort.Sort(s.videoTracksSorted)

		s.allocateTrack(track)
	}
}

func (s *StreamAllocator) handleSignalRemoveTrack(event *Event) {
	switch event.DownTrack.Kind() {
	case webrtc.RTPCodecTypeAudio:
		if _, ok := s.audioTracks[event.DownTrack.ID()]; !ok {
			return
		}

		delete(s.audioTracks, event.DownTrack.ID())
	case webrtc.RTPCodecTypeVideo:
		track, ok := s.videoTracks[event.DownTrack.ID()]
		if !ok {
			return
		}

		delete(s.videoTracks, event.DownTrack.ID())

		n := len(s.videoTracksSorted)
		for idx, videoTrack := range s.videoTracksSorted {
			if videoTrack.DownTrack() == event.DownTrack {
				s.videoTracksSorted[idx] = s.videoTracksSorted[n-1]
				s.videoTracksSorted = s.videoTracksSorted[:n-1]
				break
			}
		}
		sort.Sort(s.videoTracksSorted)

		// re-initialize estimate if all tracks are removed, let it get a fresh start
		if len(s.videoTracksSorted) == 0 {
			s.initializeEstimate()
			return
		}

		s.unallocateTrack(track)
	}
}

func (s *StreamAllocator) handleSignalEstimate(event *Event) {
	// the channel capacity is estimated at a peer connection level. All down tracks
	// in the peer connection will end up calling this for a REMB report with
	// the same estimated channel capacity. Use a tracking SSRC to lock onto to
	// one report. As SSRCs can be dropped over time, update tracking SSRC as needed
	//
	// A couple of things to keep in mind
	//   - REMB reports could be sent gratuitously as a way of providing
	//     periodic feedback, i. e. even if the estimated capacity does not
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

	// if there are no video tracks, ignore any straggler REMB
	if len(s.videoTracksSorted) == 0 {
		return
	}

	remb, _ := event.Data.(*rtcp.ReceiverEstimatedMaximumBitrate)

	found := false
	for _, ssrc := range remb.SSRCs {
		if ssrc == s.trackingSSRC {
			found = true
			break
		}
	}
	if !found {
		if len(remb.SSRCs) == 0 {
			s.logger.Warnw("no SSRC to track REMB", nil, "participant", s.participantID)
			return
		}

		// try to lock to track which is sending this update
		for _, ssrc := range remb.SSRCs {
			if ssrc == event.DownTrack.SSRC() {
				s.trackingSSRC = event.DownTrack.SSRC()
				found = true
				break
			}
		}

		if !found {
			s.trackingSSRC = remb.SSRCs[0]
		}
	}

	if s.trackingSSRC != event.DownTrack.SSRC() {
		return
	}

	s.prevReceivedEstimate = s.receivedEstimate
	s.receivedEstimate = int64(remb.Bitrate)
	if s.prevReceivedEstimate != s.receivedEstimate {
		s.logger.Debugw("received new estimate", "participant", s.participantID, "old(bps)", s.prevReceivedEstimate, "new(bps)", s.receivedEstimate)
	}

	if s.maybeCommitEstimate() {
		s.allocateAllTracks()
	}
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
	switch event.DownTrack.Kind() {
	case webrtc.RTPCodecTypeAudio:
		track, ok = s.audioTracks[event.DownTrack.ID()]
	case webrtc.RTPCodecTypeVideo:
		track, ok = s.videoTracks[event.DownTrack.ID()]
	}
	if !ok {
		return
	}

	rr, _ := event.Data.(*rtcp.ReceiverReport)
	track.UpdatePacketStats(rr)
}

func (s *StreamAllocator) handleSignalAvailableLayersChange(event *Event) {
	track, ok := s.videoTracks[event.DownTrack.ID()]
	if !ok {
		return
	}

	s.allocateTrack(track)
}

func (s *StreamAllocator) handleSignalSubscriptionChange(event *Event) {
	track, ok := s.videoTracks[event.DownTrack.ID()]
	if !ok {
		return
	}

	s.allocateTrack(track)
}

func (s *StreamAllocator) handleSignalSubscribedLayersChange(event *Event) {
	track, ok := s.videoTracks[event.DownTrack.ID()]
	if !ok {
		return
	}

	layers := event.Data.(VideoLayers)
	track.UpdateMaxLayers(layers)
	sort.Sort(s.videoTracksSorted)

	s.allocateTrack(track)
}

func (s *StreamAllocator) handleSignalPeriodicPing(event *Event) {
	if s.maybeCommitEstimate() {
		s.allocateAllTracks()
	}

	// catch up on all optimistically streamed tracks
	s.finalizeTracks()

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

func (s *StreamAllocator) setState(state State) {
	if s.state != state {
		s.logger.Infow("state change", "participant", s.participantID, "from", s.state.String(), "to", state.String())
	}

	s.state = state
}

func (s *StreamAllocator) adjustState() {
	for _, videoTrack := range s.videoTracksSorted {
		if videoTrack.IsDeficient() {
			s.setState(StateDeficient)
			return
		}
	}

	s.setState(StateStable)
}

func (s *StreamAllocator) maybeCommitEstimate() (isDecreasing bool) {
	// commit channel capacity estimate under following rules
	//   1. Abs(receivedEstimate - prevReceivedEstimate) < EstimateEpsilon => estimate stable
	//   2. time.Since(lastCommitTime) > EstimateCommitMs => to catch long oscillating estimate
	if math.Abs(float64(s.receivedEstimate)-float64(s.prevReceivedEstimate)) > EstimateEpsilon {
		// too large a change, wait for estimate to settle.
		// Unless estimate has been oscillating for too long.
		if time.Since(s.lastCommitTime) < EstimateCommitMs {
			return
		}
	}

	// don't commit too often even if the change is small.
	// Small changes will also get picked up during periodic check.
	if time.Since(s.lastCommitTime) < EstimateCommitMs {
		return
	}

	if s.receivedEstimate == s.committedChannelCapacity {
		// no change in estimate, no need to commit
		return
	}

	if s.committedChannelCapacity > s.receivedEstimate && s.committedChannelCapacity != ChannelCapacityInfinity {
		// this prevents declaring a decrease when coming out of init state.
		// But, this bypasses the case where streaming starts on a bunch of
		// tracks simultaneously (imagine a participant joining a large room
		// with a lot of video tracks). In that case, it is possible that the
		// channel is hitting congestion. It will caught on the next estimate
		// decrease.
		s.lastEstimateDecreaseTime = time.Now()
		isDecreasing = true
	}
	s.committedChannelCapacity = s.receivedEstimate
	s.lastCommitTime = time.Now()

	s.logger.Debugw("committing channel capacity", "participant", s.participantID, "capacity(bps)", s.committedChannelCapacity)
	return
}

func (s *StreamAllocator) allocateTrack(track *Track) {
	// if not deficient, free pass allocate track
	if s.state == StateStable {
		update := NewStreamStateUpdate()
		result := track.Allocate(ChannelCapacityInfinity)
		update.HandleStreamingChange(result.change, track)
		s.maybeSendUpdate(update)
		return
	}

	// slice into higher priority tracks and lower priority tracks
	var hpTracks []*Track
	var lpTracks []*Track
	for idx, t := range s.videoTracksSorted {
		if t == track {
			hpTracks = s.videoTracksSorted[:idx]
			lpTracks = s.videoTracksSorted[idx+1:]
		}
	}

	// check how much can be stolen from lower priority tracks
	lpExpectedBps := int64(0)
	for _, t := range lpTracks {
		lpExpectedBps += t.BandwidthRequested()
	}

	//
	// Note that there might be no lower priority tracks and nothing to steal.
	// But, a TryAllocate is done irrespective of any stolen bits as the
	// track may be downgrading due to mute or reduction in subscribed layers
	// and actually giving back some bits.
	//
	update := NewStreamStateUpdate()

	result := track.TryAllocate(lpExpectedBps)

	update.HandleStreamingChange(result.change, track)

	delta := lpExpectedBps - result.bandwidthDelta
	if delta > 0 {
		// gotten some bits back, check if any deficient higher priority track can make use of it
		delta = s.tryAllocateTracks(hpTracks, delta, update)
	}

	// allocate all lower priority tracks with left over capacity
	if delta < 0 {
		// stolen too much
		delta = 0
	}

	for _, t := range lpTracks {
		result := t.Allocate(delta)
		update.HandleStreamingChange(result.change, t)

		delta -= result.bandwidthRequested
		if delta < 0 {
			delta = 0
		}
	}

	s.maybeSendUpdate(update)

	s.adjustState()
}

func (s *StreamAllocator) unallocateTrack(track *Track) {
	if s.state == StateStable {
		return
	}

	update := NewStreamStateUpdate()

	unallocatedBps := track.BandwidthRequested()
	if unallocatedBps > 0 {
		s.tryAllocateTracks(s.videoTracksSorted, unallocatedBps, update)
	}

	s.maybeSendUpdate(update)

	s.adjustState()
}

func (s *StreamAllocator) tryAllocateTracks(tracks []*Track, additionalBps int64, update *StreamStateUpdate) int64 {
	for _, t := range tracks {
		if !t.IsDeficient() {
			continue
		}

		result := t.TryAllocate(additionalBps)
		update.HandleStreamingChange(result.change, t)

		additionalBps -= result.bandwidthDelta
		if additionalBps <= 0 {
			// used up all the extra bits
			break
		}
	}

	return additionalBps
}

func (s *StreamAllocator) allocateAllTracks() {
	s.resetBoost()

	//
	// LK-TODO-START
	// Calculate the aggregate loss. This may or may not
	// be necessary depending on the algorithm we choose. In this
	// pass, we could also calculate audio & video track loss
	// separately and use different rules.
	//
	// The loss calculation should be for the window between last
	// allocation and now. The `lastPackets*` field in
	// `Track` structure is used to cache the packet stats
	// at the last allocation. Potentially need to think about
	// giving higher weight to recent losses. So, might have
	// to update the `lastPackets*` periodically even when
	// there is no allocation for a long time to ensure loss calculation
	// remains fresh.
	// LK-TODO-END
	//

	//
	// Ask down tracks adjust their forwarded layers.
	//
	update := NewStreamStateUpdate()

	availableChannelCapacity := s.committedChannelCapacity
	for _, track := range s.videoTracksSorted {
		//
		// `video` tracks could do one of the following
		//    - no change, i. e. currently forwarding optimal available
		//      layer and there is enough bandwidth for that.
		//    - adjust layers up or down
		//    - pause if there is not enough capacity for any layer
		//
		result := track.Allocate(availableChannelCapacity)

		update.HandleStreamingChange(result.change, track)

		availableChannelCapacity -= result.bandwidthRequested
		if availableChannelCapacity < 0 || result.state == VideoAllocationStateDeficient {
			//
			// This is walking down tracks in priortized order.
			// Once one of those streams do not fit, set
			// the availableChannelCapacity to 0 so that no
			// other lower priority stream gets forwarded.
			// Note that a lower priority stream may have
			// a layer which might fit in the left over
			// capacity. This is one type of policy
			// implementation. There may be other policies
			// which might allow lower priority to go through too.
			// So, we need some sort of policy framework here
			// to decide which streams get priority
			//
			availableChannelCapacity = 0
		}
	}

	s.maybeSendUpdate(update)

	s.adjustState()
}

func (s *StreamAllocator) maybeSendUpdate(update *StreamStateUpdate) {
	if update.Empty() {
		return
	}

	s.logger.Debugw("streamed tracks changed", "participant", s.participantID, "update", update)
	if s.onStreamStateChange != nil {
		err := s.onStreamStateChange(update)
		if err != nil {
			s.logger.Errorw("could not send streamed tracks update", err, "participant", s.participantID)
		}
	}
}

func (s *StreamAllocator) finalizeTracks() {
	for _, t := range s.videoTracksSorted {
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

func (s *StreamAllocator) maybeProbe() {
	if !s.isTimeToBoost() {
		return
	}

	s.maybeBoostLayer()
	s.adjustState()
}

func (s *StreamAllocator) maybeBoostLayer() {
	// boost first deficient track in priority order
	for _, track := range s.videoTracksSorted {
		if !track.IsDeficient() {
			continue
		}

		result := track.AllocateNextHigher()
		if result.layersChanged {
			s.lastBoostTime = time.Now()

			update := NewStreamStateUpdate()
			update.HandleStreamingChange(result.change, track)
			s.maybeSendUpdate(update)

			break
		}
	}
}

func (s *StreamAllocator) isTimeToBoost() bool {
	// if enough time has passed since last esitmate drop or last estimate boost,
	// artificially boost estimate before allocating.
	// Checking against last estimate boost prevents multiple artificial boosts
	// in situations where multiple tracks become available in a short span.
	if !s.lastBoostTime.IsZero() {
		return time.Since(s.lastBoostTime) > BoostWaitMs
	} else {
		return time.Since(s.lastEstimateDecreaseTime) > ProbeWaitMs
	}
}

func (s *StreamAllocator) resetBoost() {
	s.lastBoostTime = time.Now()
}

func (s *StreamAllocator) maybeGratuitousProbe() bool {
	if time.Since(s.lastEstimateDecreaseTime) < GratuitousProbeWaitMs || len(s.videoTracksSorted) == 0 {
		return false
	}

	// don't gratuitously probe too often
	if time.Since(s.lastGratuitousProbeTime) < GratuitousProbeMoreWaitMs {
		return false
	}

	// use last received estimate for gratuitous probing base as
	// more updates may have been received since the last commit
	expectedRateBps := s.getExpectedBandwidthUsage()
	headroomBps := s.receivedEstimate - expectedRateBps
	if headroomBps > GratuitousProbeHeadroomBps {
		return false
	}

	probeRateBps := (s.receivedEstimate * GratuitousProbePct) / 100
	if probeRateBps < GratuitousProbeMinBps {
		probeRateBps = GratuitousProbeMinBps
	}
	if probeRateBps > GratuitousProbeMaxBps {
		probeRateBps = GratuitousProbeMaxBps
	}

	s.prober.AddCluster(
		int(s.receivedEstimate+probeRateBps),
		int(expectedRateBps),
		GratuitousProbeMinDurationMs,
		GratuitousProbeMaxDurationMs,
	)

	s.lastGratuitousProbeTime = time.Now()
	return true
}

func (s *StreamAllocator) resetGratuitousProbe() {
	s.prober.Reset()
	s.lastGratuitousProbeTime = time.Now()
}

//------------------------------------------------

type StreamState int

const (
	StreamStateActive StreamState = iota
	StreamStatePaused
)

type StreamStateInfo struct {
	ParticipantSid string
	TrackSid       string
	State          StreamState
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
			ParticipantSid: track.PeerID(),
			TrackSid:       track.ID(),
			State:          StreamStatePaused,
		})
	case VideoStreamingChangeResuming:
		s.StreamStates = append(s.StreamStates, &StreamStateInfo{
			ParticipantSid: track.PeerID(),
			TrackSid:       track.ID(),
			State:          StreamStateActive,
		})
	}
}

func (s *StreamStateUpdate) Empty() bool {
	return len(s.StreamStates) == 0
}

//------------------------------------------------

type ForwardingState int

const (
	ForwardingStateOptimistic ForwardingState = iota
	ForwardingStateDryFeed
	ForwardingStateDeficient
	ForwardingStateOptimal
)

func (f ForwardingState) String() string {
	switch f {
	case ForwardingStateOptimistic:
		return "OPTIMISTIC"
	case ForwardingStateDryFeed:
		return "DRY_FEED"
	case ForwardingStateDeficient:
		return "DEFICIENT"
	case ForwardingStateOptimal:
		return "OPTIMAL"
	default:
		return fmt.Sprintf("%d", int(f))
	}
}

type Track struct {
	downTrack *DownTrack

	highestSN       uint32
	packetsLost     uint32
	lastHighestSN   uint32
	lastPacketsLost uint32

	maxLayers VideoLayers
}

func newTrack(downTrack *DownTrack) *Track {
	return &Track{
		downTrack: downTrack,
		maxLayers: downTrack.MaxLayers(),
	}
}

func (t *Track) DownTrack() *DownTrack {
	return t.downTrack
}

func (t *Track) ID() string {
	return t.downTrack.ID()
}

func (t *Track) PeerID() string {
	return t.downTrack.PeerID()
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

func (t *Track) Allocate(availableChannelCapacity int64) VideoAllocationResult {
	return t.downTrack.Allocate(availableChannelCapacity)
}

func (t *Track) TryAllocate(additionalChannelCapacity int64) VideoAllocationResult {
	return t.downTrack.TryAllocate(additionalChannelCapacity)
}

func (t *Track) FinalizeAllocate() {
	t.downTrack.FinalizeAllocate()
}

func (t *Track) AllocateNextHigher() VideoAllocationResult {
	return t.downTrack.AllocateNextHigher()
}

func (t *Track) IsDeficient() bool {
	return t.downTrack.AllocationState() == VideoAllocationStateDeficient
}

func (t *Track) BandwidthRequested() int64 {
	return t.downTrack.AllocationBandwidth()
}

//------------------------------------------------

// LK-TODO-START
// Typically, in a system like this, there are track priorities.
// It is either implemented as policy
//   Examples:
//     1. active speaker gets hi-res, all else lo-res
//     2. screen share streams get hi-res, all else lo-res
// OR
// It is left up to the clients to subscribe explicitly to the quality they want.
//
// This sorter is prioritizing tracks by max layer subscribed. But, with simple
// tracks, there is only one layer. But, it is possible they should be higher
// priority, for e.g. screen share track.
// LK-TODO-END
type TrackSorter []*Track

func (t TrackSorter) Len() int {
	return len(t)
}

func (t TrackSorter) Swap(i, j int) {
	t[i], t[j] = t[j], t[i]
}

func (t TrackSorter) Less(i, j int) bool {
	// highest spatial layers have higher priority
	if t[i].maxLayers.spatial != t[j].maxLayers.spatial {
		return t[i].maxLayers.spatial > t[j].maxLayers.spatial
	}

	// highest temporal layers have priority if max spatial layers match
	if t[i].maxLayers.temporal != t[j].maxLayers.temporal {
		return t[i].maxLayers.temporal > t[j].maxLayers.temporal
	}

	// use track id to keep ordering if nothing else changes
	// LK-TODO: ideally should be sorting, compare and then re-allocate only if order changed
	return t[i].ID() < t[j].ID()
}

//------------------------------------------------

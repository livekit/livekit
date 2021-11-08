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
//     from client side requests (muting/pausing a video or limiting maximum layer).
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
//  - State_PRE_COMMIT: Before the first estimate is committed.
//                        Estimated channel capacity is initialized to some
//                        arbitrarily high value to start streaming immediately.
//                        Serves two purposes
//                        1. Gives the bandwidth estimation algorithms data
//                        2. Start streaming as soon as a user joins. Imagine
//                           a user joining a room with 10 participants already
//                           in it. That user should start receiving streams
//                           from everybody as soon as possible.
//  - State_STABLE: When all streams are forwarded at their optimal requested layers.
//  - State_DEFICIENT: When at least one stream is not able to forward optimal requested layers.
//  - State_GRATUITOUS_PROBING: When all streams are forwarded at their optimal requested layers,
//                              but probing for extra capacity to be prepared for cases like
//                              new participant joining and streaming OR an existing participant
//                              starting a new stream like enabling camera or screen share.
//
//  Signals:
//  -------
//  Each state should take action based on these signals and advance the state machine based
//  on the result of the action.
//  - Signal_ADD_TRACK: A new track has been added.
//  - Signal_REMOVE_TRACK: An existing track has been removed.
//  - Signal_ESTIMATE_INCREASE: Estimated channel capacity is increasing.
//  - Signal_ESTIMATE_DECREASE: Estimated channel capacity is decreasing. Note that when
//                              channel gets congested, it is possible to get several of these
//                              in a very short time window.
//  - Signal_RECEIVER_REPORT: An RTCP Receiver Report received from some down track.
//  - Signal_AVAILABLE_LAYERS_ADD: Available layers of publisher changed, new layer(s) available.
//  - Signal_AVAILABLE_LAYERS_REMOVE: Available layers of publisher changed, some previously
//                                    available layer(s) not available anymore.
//  - Signal_SUBSCRIPTION_CHANGE: Subscription changed (mute/requested layers changed).
//  - Signal_PERIODIC_PING: Periodic ping
//
// There are several interesting challenges which are documented in relevant code below.
//
package sfu

import (
	"math"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
)

const (
	InitialChannelCapacity = 100 * 1000 * 1000 // 100 Mbps

	EstimateEpsilon = 2000 // 2 kbps

	BoostPct    = 8
	BoostMinBps = 20 * 1000 // 20 kbps
	BoostMaxBps = 60 * 1000 // 60 kbps

	GratuitousProbeHeadroomBps   = 1 * 1000 * 1000 // if headroom is more than 1 Mbps, don't probe
	GratuitousProbePct           = 10
	GratuitousProbeMaxBps        = 300 * 1000 // 300 kbps
	GratuitousProbeMinDurationMs = 500
	GratuitousProbeMaxDurationMs = 600

	AudioLossWeight = 0.75
	VideoLossWeight = 0.25
)

type State int

const (
	State_PRE_COMMIT State = iota
	State_STABLE
	State_DEFICIENT
	State_GRATUITOUS_PROBING
)

type Signal int

const (
	Signal_NONE Signal = iota
	Signal_ADD_TRACK
	Signal_REMOVE_TRACK
	Signal_ESTIMATE_INCREASE
	Signal_ESTIMATE_DECREASE
	Signal_RECEIVER_REPORT
	Signal_AVAILABLE_LAYERS_ADD
	Signal_AVAILABLE_LAYERS_REMOVE
	Signal_SUBSCRIPTION_CHANGE
	Signal_PERIODIC_PING
)

type BoostMode int

const (
	BoostMode_LAYER BoostMode = iota
	BoostMode_BANDWIDTH
)

var (
	// LK-TODO-START
	// These constants will definitely require more tweaking.
	// In fact, simple time tresholded rules most proably will not be enough.
	// LK-TODO-END
	EstimateCommitMs      = 2 * 1000 * time.Millisecond // 2 seconds
	ProbeWaitMs           = 5 * 1000 * time.Millisecond // 5 seconds
	GratuitousProbeWaitMs = 8 * 1000 * time.Millisecond // 8 seconds
	BoostWaitMs           = 3 * 1000 * time.Millisecond // 3 seconds
)

// LK-TODO add logger and log interesting events
type StreamAllocator struct {
	estimateMu               sync.RWMutex
	trackingSSRC             uint32
	committedChannelCapacity uint64
	lastCommitTime           time.Time
	prevReceivedEstimate     uint64
	receivedEstimate         uint64
	lastEstimateDecreaseTime time.Time

	boostMode              BoostMode
	boostedChannelCapacity uint64
	lastBoostTime          time.Time

	tracksMu sync.RWMutex
	tracks   map[string]*Track

	prober *Prober

	state State

	chMu      sync.RWMutex
	eventCh   chan []Event
	runningCh chan struct{}
}

type Event struct {
	Signal    Signal
	DownTrack *DownTrack
}

func NewStreamAllocator() *StreamAllocator {
	s := &StreamAllocator{
		committedChannelCapacity: InitialChannelCapacity,
		lastCommitTime:           time.Now(),
		receivedEstimate:         InitialChannelCapacity,
		lastEstimateDecreaseTime: time.Now(),
		boostMode:                BoostMode_LAYER,
		tracks:                   make(map[string]*Track),
		prober:                   NewProber(),
		state:                    State_PRE_COMMIT,
		eventCh:                  make(chan []Event, 10),
		runningCh:                make(chan struct{}),
	}
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

func (s *StreamAllocator) AddTrack(downTrack *DownTrack) {
	downTrack.OnREMB(s.onREMB)
	downTrack.AddReceiverReportListener(s.onReceiverReport)
	downTrack.OnAvailableLayersChanged(s.onAvailableLayersChanged)
	downTrack.OnSubscriptionChanged(s.onSubscriptionChanged)
	downTrack.OnPacketSent(s.onPacketSent)

	s.tracksMu.Lock()
	s.tracks[downTrack.ID()] = NewTrack(downTrack)
	s.tracksMu.Unlock()

	s.postEvent(Signal_ADD_TRACK, downTrack)
}

func (s *StreamAllocator) RemoveTrack(downTrack *DownTrack) {
	s.tracksMu.Lock()

	if _, ok := s.tracks[downTrack.ID()]; !ok {
		s.tracksMu.Unlock()
		return
	}

	delete(s.tracks, downTrack.ID())
	s.tracksMu.Unlock()

	s.postEvent(Signal_REMOVE_TRACK, downTrack)
}

func (s *StreamAllocator) onREMB(downTrack *DownTrack, remb *rtcp.ReceiverEstimatedMaximumBitrate) {
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

	s.estimateMu.Lock()

	found := false
	for _, ssrc := range remb.SSRCs {
		if ssrc == s.trackingSSRC {
			found = true
			break
		}
	}
	if !found {
		if len(remb.SSRCs) == 0 {
			// LK-TODO - log about REMB wihtout SSRCs
			s.estimateMu.Unlock()
			return
		}

		// try to lock to track which is sending this update
		for _, ssrc := range remb.SSRCs {
			if ssrc == downTrack.SSRC() {
				s.trackingSSRC = downTrack.SSRC()
				found = true
				break
			}
		}

		if !found {
			s.trackingSSRC = remb.SSRCs[0]
		}
	}

	if s.trackingSSRC != downTrack.SSRC() {
		s.estimateMu.Unlock()
		return
	}

	s.prevReceivedEstimate = s.receivedEstimate
	s.receivedEstimate = uint64(remb.Bitrate)
	signal := s.maybeCommitEstimate()
	s.estimateMu.Unlock()

	if signal != Signal_NONE {
		s.postEvent(signal, nil)
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
func (s *StreamAllocator) onReceiverReport(downTrack *DownTrack, rr *rtcp.ReceiverReport) {
	s.tracksMu.RLock()
	defer s.tracksMu.RUnlock()

	if track, ok := s.tracks[downTrack.ID()]; ok {
		track.UpdatePacketStats(rr)
	}
}

// called when feeding track's simulcast layer availability changes
func (s *StreamAllocator) onAvailableLayersChanged(downTrack *DownTrack, layerAdded bool) {
	// LK-TODO: Look at processing specific downtrack
	if layerAdded {
		s.postEvent(Signal_AVAILABLE_LAYERS_ADD, downTrack)
	} else {
		s.postEvent(Signal_AVAILABLE_LAYERS_REMOVE, downTrack)
	}
}

// called when subscription settings changes
func (s *StreamAllocator) onSubscriptionChanged(downTrack *DownTrack) {
	// LK-TODO: Look at processing specific downtrack
	s.postEvent(Signal_SUBSCRIPTION_CHANGE, downTrack)
}

// called when DownTrack sends a packet
func (s *StreamAllocator) onPacketSent(downTrack *DownTrack, size int) {
	if downTrack.Kind() == webrtc.RTPCodecTypeAudio {
		return
	}

	s.prober.PacketSent(size)
}

// called when prober wants to send packets
func (s *StreamAllocator) onSendProbe(bytesToSend int) int {
	if bytesToSend <= 0 {
		return 0
	}

	s.tracksMu.RLock()
	defer s.tracksMu.RUnlock()

	bytesSent := 0
	for _, track := range s.tracks {
		sent := track.WritePaddingRTP(bytesToSend)
		bytesSent += sent
		bytesToSend -= sent
		if bytesToSend <= 0 {
			break
		}
	}

	return bytesSent
}

func (s *StreamAllocator) postEvent(signal Signal, downTrack *DownTrack) {
	s.chMu.RLock()
	defer s.chMu.RUnlock()

	if !s.isRunning() {
		return
	}

	s.eventCh <- []Event{Event{
		Signal:    signal,
		DownTrack: downTrack,
	}}
}

func (s *StreamAllocator) processEvents() {
	for events := range s.eventCh {
		if events == nil {
			return
		}

		for _, event := range events {
			s.runStateMachine(event)
		}
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

		s.estimateMu.Lock()
		signal := s.maybeCommitEstimate()
		s.estimateMu.Unlock()

		if signal != Signal_NONE {
			s.postEvent(signal, nil)
		}

		s.postEvent(Signal_PERIODIC_PING, nil)
	}
}

// LK-TODO-START
// Typically, in a system like this, there are track priorities.
// It is either implemented as policy
//   Examples:
//     1. active speaker gets hi-res, all else lo-res
//     2. screen share streams get hi-res, all else lo-res
// OR it is left up to the clients to subscribe explicitly to the quality they want.
//
// When such a policy is implemented, some of the state machine behaviour needs
// to be changed. For example, in State_DEFICIENT when Signal_ADD_TRACK, it does not
// allocate immediately (it allocates only if enough time has passed since last
// estimate decrease or since the last artificial estimate boost). But, if there is
// a policy and the added track falls in the high priority bucket, an allocation
// would be required even in State_DEFICIENT to ensure higher priority streams are
// forwarded without delay.
// LK-TODO-END

// LK-TODO-START
// A better implementation would be to handle each signal as for a lot of signals,
// all the states do the same thing. But, it is written the following way for
// better readability. Will do the refactor once this code is more stable and
// more people have a chance to get familiar with it.
// LK-TODO-END
func (s *StreamAllocator) runStateMachine(event Event) {
	switch s.state {
	case State_PRE_COMMIT:
		s.runStatePreCommit(event)
	case State_STABLE:
		s.runStateStable(event)
	case State_DEFICIENT:
		s.runStateDeficient(event)
	case State_GRATUITOUS_PROBING:
		s.runStateGratuitousProbing(event)
	}
}

// LK-TODO-START
// Signal_ADD_TRACK is not useful. Probably can get rid of it.
// AVAILABLE_LAYERS_ADD/REMOVE should be how track start should
// be getting an allocation.
// LK-TODO-END
func (s *StreamAllocator) runStatePreCommit(event Event) {
	switch event.Signal {
	case Signal_ADD_TRACK:
		s.allocate()
	case Signal_REMOVE_TRACK:
		s.allocate()
	case Signal_ESTIMATE_INCREASE:
		s.allocate()
	case Signal_ESTIMATE_DECREASE:
		s.allocate()
	case Signal_RECEIVER_REPORT:
	case Signal_AVAILABLE_LAYERS_ADD:
		s.allocate()
	case Signal_AVAILABLE_LAYERS_REMOVE:
		s.allocate()
	case Signal_SUBSCRIPTION_CHANGE:
		s.allocate()
	case Signal_PERIODIC_PING:
	}
}

func (s *StreamAllocator) runStateStable(event Event) {
	switch event.Signal {
	case Signal_ADD_TRACK:
		s.allocate()
	case Signal_REMOVE_TRACK:
		// LK-TODO - may want to re-calculate channel usage?
	case Signal_ESTIMATE_INCREASE:
		// streaming optimally, no need to do anything
	case Signal_ESTIMATE_DECREASE:
		s.allocate()
	case Signal_RECEIVER_REPORT:
	case Signal_AVAILABLE_LAYERS_ADD:
		s.allocate()
	case Signal_AVAILABLE_LAYERS_REMOVE:
		s.allocate()
	case Signal_SUBSCRIPTION_CHANGE:
		s.allocate()
	case Signal_PERIODIC_PING:
		// if bandwidth estimate has been stable for a while, maybe gratuitously probe
		s.maybeGratuitousProbe()
	}
}

// LK-TODO-START
// The current implementation tries to probe using media if the allocation is not optimal.
//
// But, another option to try is using padding only to probe even when deficient.
// In the current impl, starting a new stream or moving stream to a new layer might end up
// affecting more streams, i.e. in the sense that we started a new stream and user sees
// that affecting all the streams. Using padding only means a couple of things
//   1. If padding packets get lost, no issue
//   2. From a user perspective, it will appear like a network glitch and not due to
//      starting a new stream. This is not desirable (i. e. just because user does not see
//      it as a direct effect of starting a stream  does not mean it is preferred).
//	But, something to keep in mind in terms of user perception.
// LK-TODO-END
func (s *StreamAllocator) runStateDeficient(event Event) {
	switch event.Signal {
	case Signal_ADD_TRACK:
		s.maybeProbe()
	case Signal_REMOVE_TRACK:
		s.allocate()
	case Signal_ESTIMATE_INCREASE:
		// as long as estimate is increasing, keep going.
		// Switch to STABLE state if estimate exceeds optimal bandwidth needed.
		if s.getChannelCapacity() > s.getOptimalBandwidthUsage() {
			s.resetBoost()
			s.setState(State_STABLE)
		}
	case Signal_ESTIMATE_DECREASE:
		// stop using the boosted estimate
		s.resetBoost()
		s.allocate()
	case Signal_RECEIVER_REPORT:
	case Signal_AVAILABLE_LAYERS_ADD:
		s.maybeProbe()
	case Signal_AVAILABLE_LAYERS_REMOVE:
		s.allocate()
	case Signal_SUBSCRIPTION_CHANGE:
		s.allocate()
	case Signal_PERIODIC_PING:
		s.maybeProbe()
	}
}

func (s *StreamAllocator) runStateGratuitousProbing(event Event) {
	// for anything that needs a run of allocation, stop the prober as the traffic
	// shape will be altered after an allocation. Although prober will take into
	// account regular traffic, conservatively stop the prober before an allocation
	// to avoid any self-inflicted damaage
	switch event.Signal {
	case Signal_ADD_TRACK:
		s.prober.Reset()
		s.allocate()
	case Signal_REMOVE_TRACK:
		// LK-TODO - may want to re-calculate channel usage?
	case Signal_ESTIMATE_INCREASE:
		// good, got a better estimate. Prober may or may not have finished.
		// Let it continue if it is still running.
	case Signal_ESTIMATE_DECREASE:
		// stop gratuitous probing immediately and allocate
		s.prober.Reset()
		s.allocate()
	case Signal_RECEIVER_REPORT:
	case Signal_AVAILABLE_LAYERS_ADD:
		s.prober.Reset()
		s.allocate()
	case Signal_AVAILABLE_LAYERS_REMOVE:
		s.prober.Reset()
		s.allocate()
	case Signal_SUBSCRIPTION_CHANGE:
		s.prober.Reset()
		s.allocate()
	case Signal_PERIODIC_PING:
		if !s.prober.IsRunning() {
			// try for more
			s.maybeGratuitousProbe()
			// LK-TODO - log about more probing here
		}
	}
}

func (s *StreamAllocator) setState(state State) {
	if s.state != state {
		// LK-TODO log state changes
	}

	s.state = state
}

func (s *StreamAllocator) maybeCommitEstimate() Signal {
	// commit channel capacity estimate under following rules
	//   1. Abs(receivedEstimate - prevReceivedEstimate) < EstimateEpsilon => estimate stable
	//   2. time.Since(lastCommitTime) > EstimateCommitMs => to catch long oscillating estimate
	if math.Abs(float64(s.receivedEstimate)-float64(s.prevReceivedEstimate)) > EstimateEpsilon {
		// too large a change, wait for estimate to settle
		return Signal_NONE
	}

	if time.Since(s.lastCommitTime) < EstimateCommitMs {
		// don't commit too often
		return Signal_NONE
	}

	if s.receivedEstimate == s.committedChannelCapacity {
		// no change in estimate, no need to commit
		return Signal_NONE
	}

	signal := Signal_ESTIMATE_INCREASE
	if s.receivedEstimate < s.committedChannelCapacity {
		signal = Signal_ESTIMATE_DECREASE
		s.lastEstimateDecreaseTime = time.Now()
	}

	s.committedChannelCapacity = s.receivedEstimate
	s.lastCommitTime = time.Now()

	return signal
}

func (s *StreamAllocator) getChannelCapacity() uint64 {
	s.estimateMu.RLock()
	defer s.estimateMu.RUnlock()

	return s.committedChannelCapacity
}

func (s *StreamAllocator) allocate() {
	// LK-TODO-START
	// Introduce some rules for allocate. Some thing like
	//   - When estimate decreases, immediately.
	//     Maybe have some threshold for decrease case also before triggering.
	//     o 5% decrease OR 200 kbps absolute decrease
	//   - When estimate increases
	//     o 10% increase - conservative in pushing more data
	//     o even if 10% increase, do it only once every 10/15/30 seconds
	// When estimate goes up/down, there could be multiple updates. The challenge
	// is to react quickly, but not too quickly.
	//
	// Some of the challenges here are
	//   - Audio packet loss in subscriber PC should be considered.
	//     If audio loss is too high, throttle video aggressively as
	//     audio quality trumps anything else in a conferencing application.
	//     Note that bandwidth estimation algorithms themselves
	//     might adjust for it and report estimated capacity.
	//   - Video packet loss should be taken into consideration too.
	//   - Especially tricky is video start/stop (either track start/stop
	//     or Simulcast spatial layer switching (temporal layer switching
	//     is fine)). That requires a key frame which is usually 10X
	//     the size of a P-frame. So when channel capacity goes down
	//     switching to a lower spatial layer could cause a temporary
	//     spike in bitrate exacerbating the already congested channel
	//     condition. This is a reason to use a Pacer in the path to
	//     smooth out spikes. But, a Pacer introduces significant
	//     overhead both in terms of memory (outgoing packets need to
	//     be queued) and CPU (a high frequency polling thread to drain
	//     the queued packets on to the wire at predictable rate)
	//   - Video retranmission rate should be taken into account while
	//     allocating to check which layer of publisher will
	//     fit in available bandwidth.
	//   - Increasing channel capacity is a tricky one. Some times,
	//     the bandwidth estimators will not report an increase unless
	//     the channel is probed with more traffic. So, may have to
	//     trigger an allocation if the channel is stable for a while
	//     and send some extra streams. Another option is to just
	//     send RTP padding only packets to probe the channel which
	//     can be done on an existing stream without re-enabling a
	//     stream.
	//   - There is also the issue of time synchronization. This makes
	//     debugging/simulating scenarios difficult. Basically, there
	//     are various kinds of delays in the system. So, when something
	//     really happened and when we are really responding is always
	//     going to be offset. So, need to be cognizant of that and
	//     apply necessary corrections whenever possible. For example
	//     o Bandwidth estimation itself takes time
	//     o RTCP packets could be lost
	//     o RTCP Receiver Report loss could have happened a while back.
	//       As RTCP RR usually reported once a second or so, if there
	//       is loss, there is no indication if that loss happened at
	//       the beginnning of the window or not.
	//     o When layer switching, there are more round trips needed.
	//       A PLI has to go to the publisher and publisher has to
	//       generate a key frame. Another very important event
	//       (generation of a key frame) happens potentially 100s of ms
	//       after we asked for it.
	//     In general, just need to be aware of these and tweak allocation
	//     to not cause oscillations.
	// LK-TODO-END

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

	s.tracksMu.RLock()
	//
	// Ask down tracks adjust their forwarded layers.
	// It is possible that tracks might all fit under boosted bandwidth scenarios.
	// So, track total requested bandwidth and mark DEFICIENT state if the total is
	// above the estimated channel capacity even if the optimal signal is true.
	//
	isOptimal := true
	totalBandwidthRequested := uint64(0)
	committedChannelCapacity := s.getChannelCapacity()
	availableChannelCapacity := committedChannelCapacity
	if availableChannelCapacity < s.boostedChannelCapacity {
		availableChannelCapacity = s.boostedChannelCapacity
	}
	for _, track := range s.tracks {
		//
		// `audio` tracks will do nothing in this method.
		//
		// `video` tracks could do one of the following
		//    - no change, i. e. currently forwarding optimal available
		//      layer and there is enough bandwidth for that.
		//    - adjust layers up or down
		//    - mute if there is not enough capacity for any layer
		// NOTE: When video switches layers, on layer switch up,
		// the current layer can keep forwarding to ensure smooth
		// video at the client. As layer up usually means there is
		// enough bandwidth, the lower layer can keep streaming till
		// the switch point for higher layer becomes available.
		// But, in the other direction, higher layer forwarding should
		// be stopped immediately to not further congest the channel.
		//
		//
		bandwidthRequested, optimalBandwidthNeeded := track.AdjustAllocation(availableChannelCapacity)
		totalBandwidthRequested += bandwidthRequested
		if optimalBandwidthNeeded > 0 && bandwidthRequested < optimalBandwidthNeeded {
			//
			// Assuming this is a prioritized list of tracks
			// and we are walking down in that priority order.
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
			isOptimal = false
		} else {
			availableChannelCapacity -= bandwidthRequested
		}
	}
	s.tracksMu.RUnlock()

	if !isOptimal || totalBandwidthRequested > committedChannelCapacity {
		s.setState(State_DEFICIENT)
	} else {
		if committedChannelCapacity != InitialChannelCapacity {
			s.resetBoost()
			s.setState(State_STABLE)
		}
	}

	//
	// The above loop may become a concern. In a typical conference
	// kind of scenario, there are probably not that many people, so
	// the number of down tracks will be limited.
	//
	// But, can imagine a case of roomless having a single peer
	// connection between RTC node and a relay where all the streams
	// (even spanning multiple rooms) are on a single peer connection.
	// In that case, I think this should mostly be disabled, i. e.
	// that peer connection should be looked at as RTC node's publisher
	// peer connection and any throttling mechanisms should be disabled.
	//
}

func (s *StreamAllocator) getExpectedBandwidthUsage() uint64 {
	s.tracksMu.RLock()
	defer s.tracksMu.RUnlock()

	expected := uint64(0)
	for _, track := range s.tracks {
		expected += track.BandwidthRequested()
	}

	return expected
}

func (s *StreamAllocator) getOptimalBandwidthUsage() uint64 {
	s.tracksMu.RLock()
	defer s.tracksMu.RUnlock()

	optimal := uint64(0)
	for _, track := range s.tracks {
		optimal += track.BandwidthOptimal()
	}

	return optimal
}

// LK-TODO: unused till loss based estimation is done, but just a sample impl of weighting audio higher
func (s *StreamAllocator) calculateLoss() float32 {
	s.tracksMu.RLock()
	defer s.tracksMu.RUnlock()

	packetsAudio := uint32(0)
	packetsLostAudio := uint32(0)
	packetsVideo := uint32(0)
	packetsLostVideo := uint32(0)
	for _, track := range s.tracks {
		kind, packets, packetsLost := track.GetPacketStats()

		if kind == webrtc.RTPCodecTypeAudio {
			packetsAudio += packets
			packetsLostAudio += packetsLost
		}

		if kind == webrtc.RTPCodecTypeVideo {
			packetsVideo += packets
			packetsLostVideo += packetsLost
		}
	}

	audioLossPct := float32(0.0)
	if packetsAudio != 0 {
		audioLossPct = (float32(packetsLostAudio) * 100.0) / float32(packetsAudio)
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

	switch s.boostMode {
	case BoostMode_LAYER:
		s.maybeBoostLayer()
	case BoostMode_BANDWIDTH:
		s.maybeBoostBandwidth()
	}
}

func (s *StreamAllocator) maybeBoostLayer() {
	s.tracksMu.RLock()
	for _, track := range s.tracks {
		boosted, additionalBandwidth := track.IncreaseAllocation()
		if boosted {
			if s.boostedChannelCapacity > s.committedChannelCapacity {
				s.boostedChannelCapacity += additionalBandwidth
			} else {
				s.boostedChannelCapacity = s.committedChannelCapacity + additionalBandwidth
			}
			s.lastBoostTime = time.Now()
			break
		}
	}
	s.tracksMu.RUnlock()
}

func (s *StreamAllocator) maybeBoostBandwidth() {
	// temporarily boost estimate for probing.
	// Boost either the committed channel capacity or previous boost point if there is one
	baseBps := s.getChannelCapacity()
	if baseBps < s.boostedChannelCapacity {
		baseBps = s.boostedChannelCapacity
	}

	boostBps := (baseBps * BoostPct) / 100
	if boostBps < BoostMinBps {
		boostBps = BoostMinBps
	}
	if boostBps > BoostMaxBps {
		boostBps = BoostMaxBps
	}

	s.boostedChannelCapacity = baseBps + boostBps
	s.lastBoostTime = time.Now()

	s.allocate()
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
	s.lastBoostTime = time.UnixMilli(0)
	s.boostedChannelCapacity = 0
}

func (s *StreamAllocator) maybeGratuitousProbe() {
	if time.Since(s.lastEstimateDecreaseTime) < GratuitousProbeWaitMs {
		return
	}

	committedChannelCapacity := s.getChannelCapacity()
	expectedRateBps := s.getExpectedBandwidthUsage()
	headroomBps := committedChannelCapacity - expectedRateBps
	if headroomBps > GratuitousProbeHeadroomBps {
		return
	}

	probeRateBps := (committedChannelCapacity * GratuitousProbePct) / 100
	if probeRateBps > GratuitousProbeMaxBps {
		probeRateBps = GratuitousProbeMaxBps
	}

	s.prober.AddCluster(int(committedChannelCapacity+probeRateBps), int(expectedRateBps), GratuitousProbeMinDurationMs, GratuitousProbeMaxDurationMs)

	s.setState(State_GRATUITOUS_PROBING)
}

type Track struct {
	// LK-TODO-START
	// Check if we can do without a lock?
	//
	// Packet stats are updated in a different thread.
	// Maybe a specific lock for that?
	// There may be more in the future though.
	// LK-TODO-END
	lock sync.RWMutex

	downTrack *DownTrack

	highestSN       uint32
	packetsLost     uint32
	lastHighestSN   uint32
	lastPacketsLost uint32

	bandwidthRequested     uint64
	optimalBandwidthNeeded uint64
}

func NewTrack(downTrack *DownTrack) *Track {
	return &Track{
		downTrack: downTrack,
	}
}

// LK-TODO this should probably be maintained in downTrack and this module can query what it needs
func (t *Track) UpdatePacketStats(rr *rtcp.ReceiverReport) {
	t.lock.Lock()
	defer t.lock.Unlock()

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

func (t *Track) GetPacketStats() (webrtc.RTPCodecType, uint32, uint32) {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return t.downTrack.Kind(), t.highestSN - t.lastHighestSN, t.packetsLost - t.lastPacketsLost
}

func (t *Track) WritePaddingRTP(bytesToSend int) int {
	return t.downTrack.WritePaddingRTP(bytesToSend)
}

func (t *Track) AdjustAllocation(availableChannelCapacity uint64) (uint64, uint64) {
	t.bandwidthRequested, t.optimalBandwidthNeeded = t.downTrack.AdjustAllocation(availableChannelCapacity)
	return t.bandwidthRequested, t.optimalBandwidthNeeded
}

func (t *Track) IncreaseAllocation() (bool, uint64) {
	increased, bandwidthRequested, optimalBandwidthNeeded := t.downTrack.IncreaseAllocation()
	additionalBandwidth := 0
	if increased {
		additionalBandwidth = int(bandwidthRequested) - int(t.bandwidthRequested)
		if additionalBandwidth < 0 {
			additionalBandwidth = 0
		}

		t.bandwidthRequested = bandwidthRequested
		t.optimalBandwidthNeeded = optimalBandwidthNeeded
	}

	return increased, uint64(additionalBandwidth)
}

func (t *Track) BandwidthRequested() uint64 {
	return t.bandwidthRequested
}

func (t *Track) BandwidthOptimal() uint64 {
	return t.optimalBandwidthNeeded
}

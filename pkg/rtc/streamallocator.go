//
// Design of stream allocator
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
// Code wise
//   - There will be one of these per participant. Will be created
//     in participant.go
//   - In `AddSubscriber` of mediatrack.go, the created downTrack should
//     be added to this for the corresponding subscriber.
//   - In `RemoveSubscriber`, the downTrack should be removed from the
//     corresponding subscriber. Same should be done when there are
//     subscription updates on the fly.
//   - Both video and audio tracks are added to this module.
//   - The push/pull of methods here are designed with scalability in mind.
//   - The following should be pushed
//     o Estimated bitrate changes
//     o Receiver reports of downTracks (basically needed for packet loss calc)
//   - The following will be pulled as needed
//     o Feeding track bitrate. As feeder could be feeding a large number
//       of subscribers, fanning out the feeding track bitrate periodically
//       to all the subscribers is a scalability concern. Instead, subscribers
//       will query the feeder bitrate when they have to reallocate. As
//       each subscriber's downstream network is different, they will all
//       be running re-alloc at different times thus amortizing CPU usage.
//
// There are several interesting challenges here which I have documented
// in code below.
//
package rtc

import (
	"sync"

	"github.com/pion/ion-sfu/pkg/sfu"
)

const (
	InitialChannelCapacity = 10 * 1000 * 1000 // 10 Mbps
)

type StreamAllocator struct {
	estimatedChannelCapacity uint64

	tracksMu sync.Mutex
	tracks   map[string]*Track
}

type Track struct {
	downTrack       *sfu.DownTrack
	highestSN       uint32
	packetsLost     uint32
	lastHighestSN   uint32
	lastPacketsLost uint32
}

func NewStreamAllocator() *StreamAllocator {
	s := &StreamAllocator{
		estimatedChannelCapacity: InitialChannelCapacity,
		tracks:                   make(map[string]*Track),
	}

	return s
}

func (s *StreamAllocator) AddTrack(downTrack *sfu.DownTrack) {
	s.tracksMu.Lock()
	defer s.tracksMu.Unlock()

	//
	// RAJA_TODO need to change downtrack id to be LK sid of feeding track.
	// That will ensure it is unique.
	// Currently, it is using the track id of remote track.
	// Although there is almost zero chance of collision, they are
	// still generated by different client and could be anything.
	//
	s.tracks[downTrack.ID()] = &Track{
		downTrack: downTrack,
	}

	downTrack.OnREMB(s.onEstimatedChannelCapacity)
	downTrack.OnReceiverReport(s.onReceiverReport)
	downTrack.OnAvailableLayersChanged(s.onAvailableLayersChanged)

	s.realloc()
}

func (s *StreamAllocator) RemoveTrack(downTrack *sfu.DownTrack) {
	s.tracksMu.Lock()
	defer s.tracksMu.Unlock()

	if _, ok := s.tracks[downTrack.ID()]; ok {
		delete(s.tracks, downTrack.ID())
		s.realloc()
	}
}

//
// This is potentially a registered callback on sfu.DownTrack,
// but this quantity is at peer connection level.
// Thinking about down track level callback to simplify implementation in v1.
// By doing an early exit when estimated capacity matches, even it fires
// from all downtracks, it should be quick.
// But, REMB can fire a lot in a very bursty fashion. So, something
// to keep an eye on.
//
func (s *StreamAllocator) onEstimatedChannelCapacity(downTrack *sfu.DownTrack, estimatedChannelCapacity uint64) {
	if s.estimatedChannelCapacity == estimatedChannelCapacity {
		return
	}

	s.estimatedChannelCapacity = estimatedChannelCapacity

	s.tracksMu.Lock()
	s.realloc()
	s.tracksMu.Unlock()
}

//
// This should be a registered callback on sfu.DownTrack,
// sfu.DownTrack should callback upon receiving RTCP receiver recport from
// the remote side.
//
func (s *StreamAllocator) onReceiverReport(downTrack *sfu.DownTrack, highestSN uint32, packetsLost uint32) {
	s.tracksMu.Lock()
	defer s.tracksMu.Unlock()

	if track, ok := s.tracks[downTrack.ID()]; ok {
		track.highestSN = highestSN
		track.packetsLost = packetsLost
	}
}

// called when feeding track's simulcast layer availability changes
func (s *StreamAllocator) onAvailableLayersChanged(downTrack *sfu.DownTrack) {
	s.tracksMu.Lock()
	s.realloc()
	s.tracksMu.Unlock()
}

func (s *StreamAllocator) realloc() {
	//
	// Introduce some rules for realloc.Some thing like
	//   - When estimate decreases, immediately.
	//     Maybe have some threshold for decrease before triggering.
	//     o 5% decrease OR 200 kbps absolute decrease
	//   - When estimate increases
	//     o 10% increase - conservative in pushing more data
	//     o even if 10% increase, do it only once every 10/15/30 seconds
	//
	// Some things to think about to avoid expensive re-allocation
	//   - Maybe add a callback to downTrack which will report
	//     if the allocation is optimal. If the downTrack is
	//     forwarding the highest available spatial/temporal layer,
	//     there is no need for a reallocation. If all the downTracks
	//     are forwarding optimally, a realloc is a no-op. So, even if
	//     estimated channel capacity increases, there is no need
	//     to trigger a realloc
	//
	// Some of the challenges here are
	//   - Audio packet loss in subscriber PC should be considered.
	//     If audio loss is too high, throttle video heavily.
	//     Note that bandwidth estimation algorithms themselves
	//     might adjust for it and report estimated capacity.
	//   - Need to be cognizant of audio bandwidth requirement also.
	//     With DTX, it is relatively safe to assume that aggregate
	//     audio bandwidth would not cross 150 kbps and account for that.
	//     If possible to measure the actual audio bandwidth, it should
	//     be used instead of the assumption above (it is possible to
	//     measure technically, this note is more about performance
	//     concerns of measuring a lot of things)
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
	//     taking feeder bitrate to check which layer of feeder will
	//     fit in available bandwidth.
	//   - Increasing channel capacity is a tricky one. Some times,
	//     the bandwidth estimators will not report an increase unless
	//     the channel is probed with more traffic. So, may have to
	//     trigger a realloc if the channel is stable for a while
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
	//       As it is usually reported once a second or so, if there
	//       is loss, there is no indication if that loss happened at
	//       the beginnning of the window or not.
	//     o When layer switching, there are more round trips needed.
	//       A PLI has to go to the publisher and publisher has to
	//       generate a key frame. Another very important event
	//       (generation of a key frame) happens potentially 100s of ms
	//       after we asked for it.
	//     In general, just need to be aware of these and tweak allocation
	//     to not cause oscillations.
	//

	//
	// First pass to calculate the aggregate loss. This may or may not
	// be necessary depending on the algorithm we choose. In this
	// pass, we could also calculate audio & video track loss
	// separately and use different rules.
	//
	// The loss calculation should be for the window between last
	// allocation and now. The `lastPackets*` field in
	// `Track` structure is used to cache the packet stats
	// at the last realloc. Potentially need to think about
	// giving higher weight to recent losses. So, might have
	// to update the `lastPackets*` periodically even when
	// there is no reallocation to ensure loss calculation
	// remains fresh.
	//

	//
	// Second pass to let down tracks adjust their forwarders
	//
	availableChannelCapacity := s.estimatedChannelCapacity
	for _, track := range s.tracks {
		//
		// `adjustAllocation` will be a new method on sfu.DownTrack.
		// It is given the maximum channel capacity it can use.
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
		// Continuing to stream lower layer may require an ion-sfu
		// change.
		//
		// This is where the `pull` design described comes into play.
		// In this method, the downTrack can pull current streaming
		// bitrate information from receiver (i. e. publisher) and
		// determine forwarding parameters.
		//
		bandwidthUsed, isFitting := track.downTrack.AdjustAllocation(availableChannelCapacity)
		if !isFitting {
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
		} else {
			availableChannelCapacity -= bandwidthUsed
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

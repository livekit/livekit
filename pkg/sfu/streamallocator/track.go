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
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

type Track struct {
	downTrack   *sfu.DownTrack
	source      livekit.TrackSource
	isSimulcast bool
	priority    uint8
	publisherID livekit.ParticipantID
	logger      logger.Logger

	maxLayer buffer.VideoLayer

	totalPackets       uint32
	totalRepeatedNacks uint32

	/* STREAM-ALLOCATOR-DATA
	nackInfos map[uint16]sfu.NackInfo
	// STREAM-ALLOCATOR-EXPERIMENTAL-TODO: remove after experimental
	nackHistory []string

	receiverReportInitialized       bool
	totalLostAtLastRead             uint32
	totalLost                       uint32
	highestSequenceNumberAtLastRead uint32
	highestSequenceNumber           uint32
	maxRTT                          uint32
	// STREAM-ALLOCATOR-EXPERIMENTAL-TODO: remove after experimental
	receiverReportHistory []string
	*/

	isDirty bool

	streamState StreamState
}

func NewTrack(
	downTrack *sfu.DownTrack,
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
		/* STREAM-ALLOCATOR-DATA
		nackInfos:             make(map[uint16]sfu.NackInfo),
		nackHistory:           make([]string, 0, 10),
		receiverReportHistory: make([]string, 0, 10),
		*/
		streamState: StreamStateInactive,
	}
	t.SetPriority(0)
	t.SetMaxLayer(downTrack.MaxLayer())

	return t
}

func (t *Track) SetDirty(isDirty bool) bool {
	if t.isDirty == isDirty {
		return false
	}

	t.isDirty = isDirty
	return true
}

func (t *Track) SetStreamState(streamState StreamState) bool {
	if t.streamState == streamState {
		return false
	}

	t.streamState = streamState
	return true
}

func (t *Track) IsSubscribeMutable() bool {
	return t.streamState != StreamStatePaused
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

func (t *Track) DownTrack() *sfu.DownTrack {
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

func (t *Track) SetMaxLayer(layer buffer.VideoLayer) bool {
	if t.maxLayer == layer {
		return false
	}

	t.maxLayer = layer
	return true
}

func (t *Track) WritePaddingRTP(bytesToSend int) int {
	return t.downTrack.WritePaddingRTP(bytesToSend, false, false)
}

func (t *Track) AllocateOptimal(allowOvershoot bool) sfu.VideoAllocation {
	return t.downTrack.AllocateOptimal(allowOvershoot)
}

func (t *Track) ProvisionalAllocatePrepare() {
	t.downTrack.ProvisionalAllocatePrepare()
}

func (t *Track) ProvisionalAllocateReset() {
	t.downTrack.ProvisionalAllocateReset()
}

func (t *Track) ProvisionalAllocate(availableChannelCapacity int64, layer buffer.VideoLayer, allowPause bool, allowOvershoot bool) (bool, int64) {
	return t.downTrack.ProvisionalAllocate(availableChannelCapacity, layer, allowPause, allowOvershoot)
}

func (t *Track) ProvisionalAllocateGetCooperativeTransition(allowOvershoot bool) sfu.VideoTransition {
	return t.downTrack.ProvisionalAllocateGetCooperativeTransition(allowOvershoot)
}

func (t *Track) ProvisionalAllocateGetBestWeightedTransition() sfu.VideoTransition {
	return t.downTrack.ProvisionalAllocateGetBestWeightedTransition()
}

func (t *Track) ProvisionalAllocateCommit() sfu.VideoAllocation {
	return t.downTrack.ProvisionalAllocateCommit()
}

func (t *Track) AllocateNextHigher(availableChannelCapacity int64, allowOvershoot bool) (sfu.VideoAllocation, bool) {
	return t.downTrack.AllocateNextHigher(availableChannelCapacity, allowOvershoot)
}

func (t *Track) GetNextHigherTransition(allowOvershoot bool) (sfu.VideoTransition, bool) {
	return t.downTrack.GetNextHigherTransition(allowOvershoot)
}

func (t *Track) Pause() sfu.VideoAllocation {
	return t.downTrack.Pause()
}

func (t *Track) IsDeficient() bool {
	return t.downTrack.IsDeficient()
}

func (t *Track) BandwidthRequested() int64 {
	return t.downTrack.BandwidthRequested()
}

func (t *Track) DistanceToDesired() float64 {
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

/* STREAM-ALLOCATOR-DATA
func (t *Track) UpdateNack(nackInfos []sfu.NackInfo) {
	for _, ni := range nackInfos {
		t.nackInfos[ni.SequenceNumber] = ni
	}
}

func (t *Track) GetAndResetNackStats() (lowest uint16, highest uint16, numNacked int, numNacks int, numRuns int) {
	if len(t.nackInfos) == 0 {
		return
	}

	sns := make([]uint16, 0, len(t.nackInfos))
	for _, ni := range t.nackInfos {
		if lowest == 0 || ni.SequenceNumber-lowest > (1<<15) {
			lowest = ni.SequenceNumber
		}
		if highest == 0 || highest-ni.SequenceNumber > (1<<15) {
			highest = ni.SequenceNumber
		}
		numNacks += int(ni.Attempts)
		sns = append(sns, ni.SequenceNumber)
	}
	numNacked = len(t.nackInfos)

	// find number of runs, i. e. bursts of contiguous sequence numbers NACKed, does not include isolated NACKs
	sort.Slice(sns, func(i, j int) bool {
		return (sns[i] - sns[j]) > (1 << 15)
	})

	rsn := sns[0]
	rsi := 0
	for i := 1; i < len(sns); i++ {
		if sns[i] == rsn+1 {
			continue
		}

		if (i - rsi - 1) > 0 {
			numRuns++
		}

		rsn = sns[i]
		rsi = i
	}

	t.nackInfos = make(map[uint16]sfu.NackInfo)
	return
}

func (t *Track) ProcessRTCPReceiverReport(rr rtcp.ReceptionReport) {
	if !t.receiverReportInitialized {
		t.receiverReportInitialized = true
		t.totalLostAtLastRead = rr.TotalLost
		t.highestSequenceNumberAtLastRead = rr.LastSequenceNumber
	}

	t.totalLost = rr.TotalLost
	t.highestSequenceNumber = rr.LastSequenceNumber

	if rtt, err := mediatransportutil.GetRttMsFromReceiverReportOnly(&rr); err != nil {
		if rtt > t.maxRTT {
			t.maxRTT = rtt
		}
	}

	t.updateReceiverReportHistory()
}

func (t *Track) GetRTCPReceiverReportDelta() (uint32, uint32, uint32) {
	deltaPackets := t.highestSequenceNumber - t.highestSequenceNumberAtLastRead
	t.highestSequenceNumberAtLastRead = t.highestSequenceNumber

	deltaLost := t.totalLost - t.totalLostAtLastRead
	t.totalLostAtLastRead = t.totalLost

	maxRTT := t.maxRTT
	t.maxRTT = 0

	return deltaLost, deltaPackets, maxRTT
}

func (t *Track) GetAndResetBytesSent() (uint32, uint32) {
	return t.downTrack.GetAndResetBytesSent()
}

func (t *Track) UpdateHistory() {
	t.updateNackHistory()
}

func (t *Track) GetHistory() string {
	return fmt.Sprintf("t: %+v, n: %+v, rr: %+v", time.Now(), t.nackHistory, t.receiverReportHistory)
}

// STREAM-ALLOCATOR-EXPERIMENTAL-TODO:
// Idea is to check if this provides a good signal to detect congestion.
// This measures a few things
//   1. Spread: sequence number difference between highest and lowest NACK
//      - shows how widespread the losses are
//   2. Number of runs of length more than 1: Counts number of burst losses.
//      - could be a sign of congestion when losses are bursty
//   3. NACK density: how many sequence numbers in the spread were NACKed.
//      - a high density could be a sign of congestion
//   4. NACK intensity: how many times those sequence numbers were NACKed.
//      - high intensity could be a sign of congestion
//
// While these all could be good signals, some challenges in making use of these
// - aggregating across tracks
// - proper thresholing, i. e. something based on averages should not trip
//   because of small numbers, e. g. a single NACK run of 2 sequence numbers
//   is technically a burst, but is it a signal of congestion?
func (t *Track) updateNackHistory() {
	if len(t.nackHistory) >= 10 {
		t.nackHistory = t.nackHistory[1:]
	}

	l, h, nnd, nns, nr := t.GetAndResetNackStats()
	spread := h - l + 1
	density := float64(0.0)
	if nnd != 0 {
		density = float64(nnd) / float64(spread)
	} else {
		spread = 0
	}
	intensity := float64(0.0)
	if nnd != 0 {
		intensity = float64(nns) / float64(nnd)
	}
	t.nackHistory = append(
		t.nackHistory,
		fmt.Sprintf("t: %+v, l: %d, h: %d, sp: %d, nnd: %d, dens: %.2f, nns: %d, int: %.2f, nr: %d", time.Now().UnixMilli(), l, h, spread, nnd, density, nns, intensity, nr),
	)
}

func (t *Track) updateReceiverReportHistory() {
	if len(t.receiverReportHistory) >= 10 {
		t.receiverReportHistory = t.receiverReportHistory[1:]
	}

	dl, dp, maxRTT := t.GetRTCPReceiverReportDelta()
	t.receiverReportHistory = append(
		t.receiverReportHistory,
		fmt.Sprintf("t: %+v, l: %d, p: %d, rtt: %d", time.Now().Format(time.UnixDate), dl, dp, maxRTT),
	)
}
*/

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

	if t[i].maxLayer.Spatial != t[j].maxLayer.Spatial {
		return t[i].maxLayer.Spatial > t[j].maxLayer.Spatial
	}

	return t[i].maxLayer.Temporal > t[j].maxLayer.Temporal
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

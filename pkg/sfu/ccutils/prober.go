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

// Design of Prober
//
// Probing is used to check for existence of excess channel capacity.
// This is especially useful in the downstream direction of SFU.
// SFU forwards audio/video streams from one or more publishers to
// all the subscribers. But, the downstream channel of a subscriber
// may not be big enough to carry all the streams. It is also a time
// varying quantity.
//
// When there is not enough capacity, some streams will be paused.
// To resume a stream, SFU would need to know that the channel has
// enough capacity. That's where probing comes in. When conditions
// are favorable, SFU can send probe packets so that the bandwidth
// estimator has more data to estimate available channel capacity
// better.
// NOTE: What defines `favorable conditions` is implementation dependent.
//
// There are two options for probing
//   - Use padding only RTP packets: This one is preferable as
//     probe rate can be controlled more tightly.
//   - Resume a paused stream or forward a higher spatial layer:
//     Have to find a stream at probing rate. Also, a stream could
//     get a key frame unexpectedly boosting rate in the probing
//     window.
//
// The strategy used depends on stream allocator implementation.
// This module can be used if the stream allocator decides to use
// padding only RTP packets for probing purposes.
//
// Implementation:
// There are a couple of options
//   - Check prober in the forwarding path (pull from prober).
//     This is preferred for scalability reasons. But, this
//     suffers from not being able to probe when all streams
//     are paused (could be due to downstream bandwidth
//     constraints or the corresponding upstream tracks may
//     have paused due to upstream bandwidth constraints).
//     Another issue is not being able to have tight control on
//     probing window boundary as the packet forwarding path
//     may not have a packet to forward. But, it should not
//     be a major concern as long as some stream(s) is/are
//     forwarded as there should be a packet at least every
//     60 ms or so (forwarding only one stream at 15 fps).
//     Usually, it will be serviced much more frequently when
//     there are multiple streams getting forwarded.
//   - Run it a go routine. But, that would have to wake up
//     very often to prevent bunching up of probe
//     packets. So, a scalability concern as there is one prober
//     per subscriber peer connection. But, probe windows
//     should be very short (of the order of 100s of ms).
//     So, this approach might be fine.
//
// The implementation here follows the second approach of using a
// go routine.
//
// Pacing:
// ------
// Ideally, the subscriber peer connection should have a pacer which
// trickles data out at the estimated channel capacity rate (and
// estimated channel capacity + probing rate when actively probing).
//
// But, there a few significant challenges
//  1. Pacer will require buffering of forwarded packets. That means
//     more memory, more CPU (have to make copy of packets) and
//     more latency in the media stream.
//  2. Scalability concern as SFU may be handling hundreds of
//     subscriber peer connections and each one processing the pacing
//     loop at 5ms interval will add up.
//
// So, this module assumes that pacing is inherently provided by the
// publishers for media streams. That is a reasonable assumption given
// that publishing clients will run their own pacer and pacing data out
// at a steady rate.
//
// A further assumption is that if there are multiple publishers for
// a subscriber peer connection, all the publishers are not pacing
// in sync, i.e. each publisher's pacer is completely independent
// and SFU will be receiving the media packets with a good spread and
// not clumped together.
//
// Given those assumptions, this module monitors media send rate and
// adjusts probing packet sends accordingly. Although the probing may
// have a high enough wake up frequency, it is for short windows.
// For example, probing at 5 Mbps for 1/2 second and sending 1000 byte
// probe per iteration will wake up every 1.6 ms. That is very high,
// but should last for 1/2 second or so.
//
//	5 Mbps over 1/2 second = 2.5 Mbps
//	2.5 Mbps = 312500 bytes = 313 probes at 1000 byte probes
//	313 probes over 1/2 second = 1.6 ms between probes
//
// A few things to note
//  1. When a probe cluster is added, the expected media rate is provided.
//     So, the wake-up interval takes that into account. For example,
//     if probing at 5 Mbps for 1/2 second and if 4 Mbps of it is expected
//     to be provided by media traffic, the wake-up interval becomes 8 ms.
//  2. The amount of probing should actually be capped at some value to
//     avoid too much self-induced congestion. It maybe something like 500 kbps.
//     That will increase the wake-up interval to 16 ms in the above example.
//  3. In practice, the probing interval may also be shorter. Typically,
//     it can be run for 2 - 3 RTTs to get a good measurement. For
//     the longest hauls, RTT could be 250 ms or so leading to the probing
//     window being long(ish). But, RTT should be much shorter especially if
//     the subscriber peer connection of the client is able to connect to
//     the nearest data center.
package ccutils

import (
	"fmt"
	"sync"
	"time"

	"github.com/gammazero/deque"
	"go.uber.org/atomic"
	"go.uber.org/zap/zapcore"

	"github.com/livekit/protocol/logger"
)

type ProberListener interface {
	OnSendProbe(bytesToSend int)
	OnProbeClusterSwitch(probeClusterId ProbeClusterId, desiredBytes int)
}

type ProberParams struct {
	Listener ProberListener
	Logger   logger.Logger
}

type Prober struct {
	params ProberParams

	clusterId atomic.Uint32

	clustersMu    sync.RWMutex
	clusters      deque.Deque[*Cluster]
	activeCluster *Cluster
}

func NewProber(params ProberParams) *Prober {
	p := &Prober{
		params: params,
	}
	p.clusters.SetMinCapacity(2)
	return p
}

func (p *Prober) IsRunning() bool {
	p.clustersMu.RLock()
	defer p.clustersMu.RUnlock()

	return p.clusters.Len() > 0
}

func (p *Prober) Reset(info ProbeClusterInfo) {
	p.clustersMu.Lock()
	defer p.clustersMu.Unlock()

	if p.activeCluster != nil {
		if p.activeCluster.Id() == info.ProbeClusterId {
			p.activeCluster.MarkCompleted(info)
			p.params.Logger.Debugw("prober: resetting active cluster", "cluster", p.activeCluster)
		}
	}

	p.clusters.Clear()
	p.activeCluster = nil
}

func (p *Prober) AddCluster(
	mode ProbeClusterMode,
	desiredRateBps int,
	expectedRateBps int,
	duration time.Duration,
) ProbeClusterId {
	if desiredRateBps <= 0 {
		return ProbeClusterIdInvalid
	}

	clusterId := ProbeClusterId(p.clusterId.Inc())
	cluster := newCluster(
		clusterId,
		mode,
		desiredRateBps,
		expectedRateBps,
		duration,
		p.params.Listener,
	)
	p.params.Logger.Debugw("cluster added", "cluster", cluster)

	p.pushBackClusterAndMaybeStart(cluster)

	return clusterId
}

func (p *Prober) ClusterDone(info ProbeClusterInfo) {
	cluster := p.getFrontCluster()
	if cluster == nil {
		return
	}

	if cluster.Id() == info.ProbeClusterId {
		cluster.MarkCompleted(info)
		p.params.Logger.Debugw("cluster done", "cluster", cluster)
		p.popFrontCluster(cluster)
	}
}

func (p *Prober) GetActiveClusterId() ProbeClusterId {
	p.clustersMu.RLock()
	defer p.clustersMu.RUnlock()

	if p.activeCluster != nil {
		return p.activeCluster.Id()
	}

	return ProbeClusterIdInvalid
}

func (p *Prober) getFrontCluster() *Cluster {
	p.clustersMu.Lock()
	defer p.clustersMu.Unlock()

	if p.activeCluster != nil {
		return p.activeCluster
	}

	if p.clusters.Len() == 0 {
		p.activeCluster = nil
	} else {
		p.activeCluster = p.clusters.Front()
		p.activeCluster.Start()
	}
	return p.activeCluster
}

func (p *Prober) popFrontCluster(cluster *Cluster) {
	p.clustersMu.Lock()

	if p.clusters.Len() == 0 {
		p.activeCluster = nil
		p.clustersMu.Unlock()
		return
	}

	if p.clusters.Front() == cluster {
		p.clusters.PopFront()
	}

	if cluster == p.activeCluster {
		p.activeCluster = nil
	}

	p.clustersMu.Unlock()
}

func (p *Prober) pushBackClusterAndMaybeStart(cluster *Cluster) {
	p.clustersMu.Lock()
	p.clusters.PushBack(cluster)

	if p.clusters.Len() == 1 {
		go p.run()
	}
	p.clustersMu.Unlock()
}

func (p *Prober) run() {
	cluster := p.getFrontCluster()
	if cluster == nil {
		return
	}

	timer := time.NewTimer(cluster.GetSleepDuration())
	defer timer.Stop()
	for {
		<-timer.C

		// wake up and check for probes to send
		cluster = p.getFrontCluster()
		if cluster == nil {
			return
		}

		cluster.Process()

		timer.Reset(cluster.GetSleepDuration())
	}
}

// ---------------------------------

type ProbeClusterId uint32

const (
	ProbeClusterIdInvalid ProbeClusterId = 0

	cBucketDuration  = 100 * time.Millisecond
	cBytesPerProbe   = 1100 // padding only packets are 255 bytes max + 20 byte header = 4 packets per probe
	cMinProbeRateBps = 10000
)

// -----------------------------------

type ProbeClusterMode int

const (
	ProbeClusterModeUniform ProbeClusterMode = iota
	ProbeClusterModeLinearChirp
)

func (p ProbeClusterMode) String() string {
	switch p {
	case ProbeClusterModeUniform:
		return "UNIFORM"
	case ProbeClusterModeLinearChirp:
		return "LINEAR_CHIRP"
	default:
		return fmt.Sprintf("%d", int(p))
	}
}

// ---------------------------------------------------------------------------

type ProbeClusterInfo struct {
	ProbeClusterId       ProbeClusterId
	DesiredBytes         int
	StartTime            int64
	EndTime              int64
	BytesProbe           int
	BytesNonProbePrimary int
	BytesNonProbeRTX     int
}

var (
	ProbeClusterInfoInvalid = ProbeClusterInfo{ProbeClusterId: ProbeClusterIdInvalid}
)

func (p ProbeClusterInfo) Bytes() int {
	return p.BytesProbe + p.BytesNonProbePrimary + p.BytesNonProbeRTX
}

func (p ProbeClusterInfo) Duration() time.Duration {
	return time.Duration(p.EndTime - p.StartTime)
}

func (p ProbeClusterInfo) MarshalLogObject(e zapcore.ObjectEncoder) error {
	e.AddUint32("ProbeClusterId", uint32(p.ProbeClusterId))
	e.AddInt("DesiredBytes", p.DesiredBytes)
	e.AddTime("StartTime", time.Unix(0, p.StartTime))
	e.AddTime("EndTime", time.Unix(0, p.EndTime))
	e.AddDuration("Duration", p.Duration())
	e.AddInt("BytesProbe", p.BytesProbe)
	e.AddInt("BytesNonProbePrimary", p.BytesNonProbePrimary)
	e.AddInt("BytesNonProbeRTX", p.BytesNonProbeRTX)
	e.AddInt("Bytes", p.Bytes())
	return nil
}

// ---------------------------------------------------------------------------

type clusterBucket struct {
	desiredNumProbes int
	desiredBytes     int
	sleepDuration    time.Duration
}

func (c clusterBucket) MarshalLogObject(e zapcore.ObjectEncoder) error {
	e.AddInt("desiredNumProbes", c.desiredNumProbes)
	e.AddInt("desiredBytes", c.desiredBytes)
	e.AddDuration("sleepDuration", c.sleepDuration)
	return nil
}

// ---------------------------------------------------------------------------

type Cluster struct {
	lock sync.RWMutex

	id              ProbeClusterId
	mode            ProbeClusterMode
	desiredRateBps  int
	expectedRateBps int
	listener        ProberListener
	desiredBytes    int
	duration        time.Duration

	buckets   []clusterBucket
	bucketIdx int

	numProbesSent    int
	isComplete       bool
	probeClusterInfo ProbeClusterInfo
}

func newCluster(
	id ProbeClusterId,
	mode ProbeClusterMode,
	desiredRateBps int,
	expectedRateBps int,
	duration time.Duration,
	listener ProberListener,
) *Cluster {
	c := &Cluster{
		id:              id,
		mode:            mode,
		desiredRateBps:  desiredRateBps,
		expectedRateBps: expectedRateBps,
		listener:        listener,
		duration:        duration,
	}
	c.initBuckets(desiredRateBps, expectedRateBps, duration)
	c.desiredBytes = c.buckets[len(c.buckets)-1].desiredBytes
	return c
}

func (c *Cluster) initBuckets(desiredRateBps int, expectedRateBps int, duration time.Duration) {
	// split into granular buckets
	// NOTE: splitting even if mode is unitform
	numBuckets := int((duration.Milliseconds() + cBucketDuration.Milliseconds() - 1) / cBucketDuration.Milliseconds())
	if numBuckets < 1 {
		numBuckets = 1
	}

	expectedRateBytesPerSec := (expectedRateBps + 7) / 8
	baseProbeRateBps := (desiredRateBps - expectedRateBps + numBuckets - 1) / numBuckets

	runningNumProbes := 0
	runningDesiredBytes := 0

	c.buckets = make([]clusterBucket, 0, numBuckets)
	for bucketIdx := 0; bucketIdx < numBuckets; bucketIdx++ {
		multiplier := numBuckets
		if c.mode == ProbeClusterModeLinearChirp {
			multiplier = bucketIdx + 1
		}

		bucketProbeRateBps := baseProbeRateBps * multiplier
		if bucketProbeRateBps < cMinProbeRateBps {
			bucketProbeRateBps = cMinProbeRateBps
		}
		bucketProbeRateBytesPerSec := (bucketProbeRateBps + 7) / 8

		runningDesiredBytes += (((bucketProbeRateBytesPerSec + expectedRateBytesPerSec) * int(cBucketDuration.Milliseconds())) + 999) / 1000
		numProbesNeeded := (runningDesiredBytes + cBytesPerProbe - 1) / cBytesPerProbe

		numProbesInBucket := numProbesNeeded - runningNumProbes
		if numProbesInBucket <= 0 {
			numProbesInBucket = 1
		}
		runningNumProbes += numProbesInBucket

		sleepDurationMicroSeconds := int(float64(cBucketDuration.Microseconds())/float64(numProbesInBucket) + 0.5)

		c.buckets = append(c.buckets, clusterBucket{
			desiredNumProbes: runningNumProbes,
			sleepDuration:    time.Duration(sleepDurationMicroSeconds) * time.Microsecond,
		})
	}
}

func (c *Cluster) Start() {
	if c.listener != nil {
		c.listener.OnProbeClusterSwitch(c.id, c.desiredBytes)
	}
}

func (c *Cluster) GetSleepDuration() time.Duration {
	c.lock.RLock()
	defer c.lock.RUnlock()

	return c.buckets[c.bucketIdx].sleepDuration
}

func (c *Cluster) Id() ProbeClusterId {
	return c.id
}

func (c *Cluster) MarkCompleted(info ProbeClusterInfo) {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.isComplete = true
	c.probeClusterInfo = info
}

func (c *Cluster) Process() {
	c.lock.Lock()
	if c.isComplete {
		c.lock.Unlock()
		return
	}

	c.numProbesSent++
	if c.numProbesSent >= c.buckets[c.bucketIdx].desiredNumProbes {
		c.bucketIdx++
		// stay in the last bucket till desired number of bytes are sent
		if c.bucketIdx >= len(c.buckets) {
			c.bucketIdx = len(c.buckets) - 1
		}
	}
	c.lock.Unlock()

	if c.listener != nil {
		c.listener.OnSendProbe(cBytesPerProbe)
	}

	// STREAM-ALLOCATOR-TODO look at adapting sleep time based on how many bytes and how much time is left
}

func (c *Cluster) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if c != nil {
		e.AddUint32("id", uint32(c.id))
		e.AddString("mode", c.mode.String())
		e.AddInt("desiredRateBps", c.desiredRateBps)
		e.AddInt("expectedRateBps", c.expectedRateBps)
		e.AddInt("desiredBytes", c.desiredBytes)
		e.AddDuration("duration", c.duration)
		e.AddArray("buckets", logger.ObjectSlice(c.buckets))
		e.AddInt("bucketIdx", c.bucketIdx)
		e.AddInt("numProbesSent", c.numProbesSent)
		e.AddBool("isComplete", c.isComplete)
		e.AddObject("probeClusterInfo", c.probeClusterInfo)
	}
	return nil
}

// ----------------------------------------------------------------------

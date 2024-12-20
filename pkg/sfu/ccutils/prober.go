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
	"math"
	"sync"
	"time"

	"github.com/gammazero/deque"
	"go.uber.org/atomic"
	"go.uber.org/zap/zapcore"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/mono"
)

type ProberListener interface {
	OnProbeClusterSwitch(info ProbeClusterInfo)
	OnSendProbe(bytesToSend int)
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
	p.clusters.SetBaseCap(2)
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

	if p.activeCluster != nil && p.activeCluster.Id() == info.Id {
		p.activeCluster.MarkCompleted(info.Result)
		p.params.Logger.Debugw("prober: resetting active cluster", "cluster", p.activeCluster)
	}

	p.clusters.Clear()
	p.activeCluster = nil
}

func (p *Prober) AddCluster(mode ProbeClusterMode, pcg ProbeClusterGoal) ProbeClusterInfo {
	if pcg.DesiredBps <= 0 {
		return ProbeClusterInfoInvalid
	}

	clusterId := ProbeClusterId(p.clusterId.Inc())
	cluster := newCluster(clusterId, mode, pcg, p.params.Listener)
	p.params.Logger.Debugw("cluster added", "cluster", cluster)

	p.pushBackClusterAndMaybeStart(cluster)

	return cluster.Info()
}

func (p *Prober) ProbesSent(bytesSent int) {
	cluster := p.getFrontCluster()
	if cluster == nil {
		return
	}

	cluster.ProbesSent(bytesSent)
}

func (p *Prober) ClusterDone(info ProbeClusterInfo) {
	cluster := p.getFrontCluster()
	if cluster == nil {
		return
	}

	if cluster.Id() == info.Id {
		cluster.MarkCompleted(info.Result)
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
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		cluster := p.getFrontCluster()
		if cluster == nil {
			return
		}

		sleepDuration := cluster.Process()
		if sleepDuration == 0 {
			p.popFrontCluster(cluster)
			continue
		}

		ticker.Reset(sleepDuration)
		<-ticker.C
	}
}

// ---------------------------------

type ProbeClusterId uint32

const (
	ProbeClusterIdInvalid ProbeClusterId = 0

	// padding only packets are 255 bytes max + 20 byte header = 4 packets per probe,
	// when not using padding only packets, this is a min and actual sent could be higher
	cBytesPerProbe    = 1100
	cSleepDuration    = 20 * time.Millisecond
	cSleepDurationMin = 10 * time.Millisecond
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

type ProbeClusterGoal struct {
	AvailableBandwidthBps int
	ExpectedUsageBps      int
	DesiredBps            int
	Duration              time.Duration
	DesiredBytes          int
}

func (p ProbeClusterGoal) MarshalLogObject(e zapcore.ObjectEncoder) error {
	e.AddInt("AvailableBandwidthBps", p.AvailableBandwidthBps)
	e.AddInt("ExpectedUsageBps", p.ExpectedUsageBps)
	e.AddInt("DesiredBps", p.DesiredBps)
	e.AddDuration("Duration", p.Duration)
	e.AddInt("DesiredBytes", p.DesiredBytes)
	return nil
}

type ProbeClusterResult struct {
	StartTime              int64
	EndTime                int64
	PacketsProbe           int
	BytesProbe             int
	PacketsNonProbePrimary int
	BytesNonProbePrimary   int
	PacketsNonProbeRTX     int
	BytesNonProbeRTX       int
	IsCompleted            bool
}

func (p ProbeClusterResult) Bytes() int {
	return p.BytesProbe + p.BytesNonProbePrimary + p.BytesNonProbeRTX
}

func (p ProbeClusterResult) Duration() time.Duration {
	return time.Duration(p.EndTime - p.StartTime)
}

func (p ProbeClusterResult) Bitrate() float64 {
	duration := p.Duration().Seconds()
	if duration != 0 {
		return float64(p.Bytes()*8) / duration
	}

	return 0
}

func (p ProbeClusterResult) MarshalLogObject(e zapcore.ObjectEncoder) error {
	e.AddTime("StartTime", time.Unix(0, p.StartTime))
	e.AddTime("EndTime", time.Unix(0, p.EndTime))
	e.AddDuration("Duration", p.Duration())
	e.AddInt("PacketsProbe", p.PacketsProbe)
	e.AddInt("BytesProbe", p.BytesProbe)
	e.AddInt("PacketsNonProbePrimary", p.PacketsNonProbePrimary)
	e.AddInt("BytesNonProbePrimary", p.BytesNonProbePrimary)
	e.AddInt("PacketsNonProbeRTX", p.PacketsNonProbeRTX)
	e.AddInt("BytesNonProbeRTX", p.BytesNonProbeRTX)
	e.AddInt("Bytes", p.Bytes())
	e.AddFloat64("Bitrate", p.Bitrate())
	e.AddBool("IsCompleted", p.IsCompleted)
	return nil
}

type ProbeClusterInfo struct {
	Id        ProbeClusterId
	CreatedAt time.Time
	Goal      ProbeClusterGoal
	Result    ProbeClusterResult
}

var (
	ProbeClusterInfoInvalid = ProbeClusterInfo{Id: ProbeClusterIdInvalid}
)

func (p ProbeClusterInfo) MarshalLogObject(e zapcore.ObjectEncoder) error {
	e.AddUint32("Id", uint32(p.Id))
	e.AddTime("CreatedAt", p.CreatedAt)
	e.AddObject("Goal", p.Goal)
	e.AddObject("Result", p.Result)
	return nil
}

// ---------------------------------------------------------------------------

type bucket struct {
	expectedElapsedDuration time.Duration
	expectedProbeBytesSent  int
}

func (b bucket) MarshalLogObject(e zapcore.ObjectEncoder) error {
	e.AddDuration("expectedElapsedDuration", b.expectedElapsedDuration)
	e.AddInt("expectedProbesBytesSent", b.expectedProbeBytesSent)
	return nil
}

// ---------------------------------------------------------------------------

type Cluster struct {
	lock sync.RWMutex

	info     ProbeClusterInfo
	mode     ProbeClusterMode
	listener ProberListener

	baseSleepDuration time.Duration
	buckets           []bucket
	bucketIdx         int

	probeBytesSent int

	startTime  time.Time
	isComplete bool
}

func newCluster(id ProbeClusterId, mode ProbeClusterMode, pcg ProbeClusterGoal, listener ProberListener) *Cluster {
	c := &Cluster{
		mode: mode,
		info: ProbeClusterInfo{
			Id:        id,
			CreatedAt: mono.Now(),
			Goal:      pcg,
		},
		listener: listener,
	}
	c.initProbes()
	return c
}

func (c *Cluster) initProbes() {
	c.info.Goal.DesiredBytes = int(math.Round(float64(c.info.Goal.DesiredBps)*c.info.Goal.Duration.Seconds()/8 + 0.5))

	numBuckets := int(math.Round(c.info.Goal.Duration.Seconds()/cSleepDuration.Seconds() + 0.5))
	if numBuckets < 1 {
		numBuckets = 1
	}
	numIntervals := numBuckets

	// for linear chirp, group intervals with decreasing duration, i.e. incraasing bitrate,
	// by aiming to send same number of bytes in each interval, as intervals get shorter, the bitrate is higher
	if c.mode == ProbeClusterModeLinearChirp {
		sum := 0
		i := 1
		for {
			sum += i
			if sum >= numBuckets {
				break
			}
			i++
		}
		numBuckets = i
		numIntervals = sum
	}

	c.baseSleepDuration = c.info.Goal.Duration / time.Duration(numIntervals)
	if c.baseSleepDuration < cSleepDurationMin {
		c.baseSleepDuration = cSleepDurationMin
	}

	numIntervals = int(math.Round(c.info.Goal.Duration.Seconds()/c.baseSleepDuration.Seconds() + 0.5))
	desiredProbeBytesPerInterval := int(math.Round(((c.info.Goal.Duration.Seconds()*float64(c.info.Goal.DesiredBps-c.info.Goal.ExpectedUsageBps)/8)+float64(numIntervals)-1)/float64(numIntervals) + 0.5))

	c.buckets = make([]bucket, numBuckets)
	for i := 0; i < numBuckets; i++ {
		switch c.mode {
		case ProbeClusterModeUniform:
			c.buckets[i] = bucket{
				expectedElapsedDuration: c.baseSleepDuration,
			}

		case ProbeClusterModeLinearChirp:
			c.buckets[i] = bucket{
				expectedElapsedDuration: time.Duration(numBuckets-i) * c.baseSleepDuration,
			}
		}
		if i > 0 {
			c.buckets[i].expectedElapsedDuration += c.buckets[i-1].expectedElapsedDuration
		}
		c.buckets[i].expectedProbeBytesSent = (i + 1) * desiredProbeBytesPerInterval
	}
}

func (c *Cluster) Start() {
	if c.listener != nil {
		c.listener.OnProbeClusterSwitch(c.info)
	}
}

func (c *Cluster) Id() ProbeClusterId {
	return c.info.Id
}

func (c *Cluster) Info() ProbeClusterInfo {
	c.lock.RLock()
	defer c.lock.RUnlock()

	return c.info
}

func (c *Cluster) ProbesSent(bytesSent int) {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.probeBytesSent += bytesSent
}

func (c *Cluster) MarkCompleted(result ProbeClusterResult) {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.isComplete = true
	c.info.Result = result
}

func (c *Cluster) Process() time.Duration {
	c.lock.Lock()
	if c.isComplete {
		c.lock.Unlock()
		return 0
	}

	bytesToSend := 0
	if c.startTime.IsZero() {
		c.startTime = mono.Now()
		bytesToSend = cBytesPerProbe
	} else {
		sinceStart := time.Since(c.startTime)
		if sinceStart > c.buckets[c.bucketIdx].expectedElapsedDuration {
			c.bucketIdx++
			overflow := false
			if c.bucketIdx >= len(c.buckets) {
				// when overflowing, repeat the last bucket
				c.bucketIdx = len(c.buckets) - 1
				overflow = true
			}
			if c.buckets[c.bucketIdx].expectedProbeBytesSent > c.probeBytesSent || overflow {
				bytesToSend = max(cBytesPerProbe, c.buckets[c.bucketIdx].expectedProbeBytesSent-c.probeBytesSent)
			}
		}
	}
	c.lock.Unlock()

	if bytesToSend != 0 && c.listener != nil {
		c.listener.OnSendProbe(bytesToSend)
	}

	return cSleepDurationMin
}

func (c *Cluster) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if c != nil {
		e.AddString("mode", c.mode.String())
		e.AddObject("info", c.info)
		e.AddDuration("baseSleepDuration", c.baseSleepDuration)
		e.AddInt("numBuckets", len(c.buckets))
		e.AddInt("bucketIdx", c.bucketIdx)
		e.AddInt("probeBytesSent", c.probeBytesSent)
		e.AddTime("startTime", c.startTime)
		e.AddDuration("elapsed", time.Since(c.startTime))
		e.AddBool("isComplete", c.isComplete)
	}
	return nil
}

// ----------------------------------------------------------------------

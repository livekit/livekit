//
// Design of Prober
//
// Probing is to used to check for existence of excess channel capacity.
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
//     Another issue is not being to have tight control on
//     probing window boundary as the packet forwarding path
//     may not have a packet to forward. But, it should not
//     be a major concern as long as some stream(s) is/are
//     forwarded as there should be a packet atleast every
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
//   1. Pacer will require buffering of forwarded packets. That means
//      more memory, more CPU (have to make copy of packets) and
//      more latency in the media stream.
//   2. Scalability concern as SFU may be handling hundreds of
//      subscriber peer connections and each one processing the pacing
//      loop at 5ms interval will add up.
//
// So, this module assumes that pacing is inherently provided by the
// publishers for media streams. That is a reasonable assumption given
// that publishing clients will run their own pacer and pacing data out
// at a steady rate.
//
// A further assumption is that if there are multiple publishers for
// a subscriber peer connection, all the publishers are not pacing
// in sync, i. e. each publisher's pacer is completely independent
// and SFU will be receiving the media packets with a good spread and
// not clumped together.
//
// Given those assumptions, this module monitors media send rate and
// adjusts probing packet sends accordingly. Although the probing may
// have a high enough wake up frequency, it is for short windows.
// For example, probing at 5 Mbps for 1/2 second and sending 1000 byte
// probe per iteration will wake up every 1.6 ms. That is very high,
// but should last for 1/2 second or so.
//     5 Mbps over 1/2 second = 2.5 Mbps
//     2.5 Mbps = 312500 bytes = 313 probes at 1000 byte probes
//     313 probes over 1/2 second = 1.6 ms between probes
//
// A few things to note
// 1. When a probe cluster is added, the expected media rate is provided.
//    So, the wake up interval takes that into account. For example,
//    if probing at 5 Mbps for 1/2 second and if 4 Mbps of it is expected
//    to be provided by media traffic, the wake up interval becomes 8 ms.
// 2. The amount of probing should actually be capped at some value to
//    avoid too much self-induced congestion. It maybe something like 500 kbps.
//    That will increase the wake up interval to 16 ms in the above example.
// 3. In practice, the probing interval may also be shorter. Typically,
//    it can be run for 2 - 3 RTTs to get a good measurement. For
//    the longest hauls, RTT could be 250 ms or so leading to the probing
//    window being long(ish). But, RTT should be much shorter especially if
//    the subscriber peer connection of the client is able to connect to
//    the nearest data center.
//
package sfu

import (
	"sync"
	"time"

	"github.com/gammazero/deque"
)

type Prober struct {
	clustersMu    sync.RWMutex
	clusters      deque.Deque
	activeCluster *Cluster

	onSendProbe func(bytesToSend int) int
}

func NewProber() *Prober {
	p := &Prober{}
	p.clusters.SetMinCapacity(2)
	return p
}

func (p *Prober) IsRunning() bool {
	p.clustersMu.RLock()
	defer p.clustersMu.RUnlock()

	return p.clusters.Len() > 0
}

func (p *Prober) Reset() {
	p.clustersMu.Lock()
	// LK-TODO - log if active cluster is getting reset, maybe log state of all clusters
	defer p.clustersMu.Unlock()
	p.clusters.Clear()
}

func (p *Prober) OnSendProbe(f func(bytesToSend int) int) {
	p.onSendProbe = f
}

func (p *Prober) AddCluster(desiredRateBps int, expectedRateBps int, minDuration time.Duration, maxDuration time.Duration) {
	if desiredRateBps <= 0 {
		return
	}

	cluster := NewCluster(desiredRateBps, expectedRateBps, minDuration, maxDuration)
	// LK-TODO - log information about added cluster

	p.pushBackClusterAndMaybeStart(cluster)
}

func (p *Prober) PacketSent(size int) {
	cluster := p.getFrontCluster()
	if cluster == nil {
		return
	}

	cluster.PacketSent(size)
}

func (p *Prober) getFrontCluster() *Cluster {
	p.clustersMu.RLock()
	defer p.clustersMu.RUnlock()

	if p.activeCluster != nil {
		return p.activeCluster
	}

	if p.clusters.Len() == 0 {
		p.activeCluster = nil
	} else {
		p.activeCluster = p.clusters.Front().(*Cluster)
		p.activeCluster.Start()
	}
	return p.activeCluster
}

func (p *Prober) popFrontCluster(cluster *Cluster) {
	p.clustersMu.Lock()
	defer p.clustersMu.Unlock()

	if p.clusters.Len() == 0 {
		p.activeCluster = nil
		return
	}

	if p.clusters.Front().(*Cluster) == cluster {
		p.clusters.PopFront()
	}

	if cluster == p.activeCluster {
		p.activeCluster = nil
	}
}

func (p *Prober) pushBackClusterAndMaybeStart(cluster *Cluster) {
	p.clustersMu.Lock()
	defer p.clustersMu.Unlock()

	p.clusters.PushBack(cluster)

	if p.clusters.Len() == 1 {
		go p.run()
	}
}

func (p *Prober) run() {
	for {
		// determine how long to sleep
		cluster := p.getFrontCluster()
		if cluster == nil {
			return
		}

		time.Sleep(cluster.GetSleepDuration())

		// wake up and check for probes to send
		cluster = p.getFrontCluster()
		if cluster == nil {
			return
		}

		if !cluster.Process(p) {
			p.popFrontCluster(cluster)
			continue
		}
	}
}

type Cluster struct {
	// LK-TODO-START
	// Check if we can operate at cluster level without a lock.
	// The quantities that are updated in a different thread are
	//   bytesSentNonProbe - maybe make this an atomic value
	// Lock contention time should be very minimal though.
	// LK-TODO-END
	lock sync.RWMutex

	desiredBytes int
	minDuration  time.Duration
	maxDuration  time.Duration

	sleepDuration time.Duration

	bytesSentProbe    int
	bytesSentNonProbe int
	startTime         time.Time
}

func NewCluster(desiredRateBps int, expectedRateBps int, minDuration time.Duration, maxDuration time.Duration) *Cluster {
	minDurationMs := minDuration.Milliseconds()
	desiredBytes := int((int64(desiredRateBps)*minDurationMs/time.Second.Milliseconds() + 7) / 8)
	expectedBytes := int((int64(expectedRateBps)*minDurationMs/time.Second.Milliseconds() + 7) / 8)

	// pace based on sending approximately 1000 bytes per probe
	numProbes := int((desiredBytes - expectedBytes + 999) / 1000)
	sleepDurationMicroSeconds := int(float64(minDurationMs*1000)/float64(numProbes) + 0.5)
	c := &Cluster{
		desiredBytes:  desiredBytes,
		minDuration:   minDuration,
		maxDuration:   maxDuration,
		sleepDuration: time.Duration(sleepDurationMicroSeconds) * time.Microsecond,
	}
	return c
}

func (c *Cluster) Start() {
	c.lock.Lock()
	defer c.lock.Unlock()

	if c.startTime.IsZero() {
		c.startTime = time.Now()
	}
}

func (c *Cluster) GetSleepDuration() time.Duration {
	c.lock.RLock()
	defer c.lock.RUnlock()

	return c.sleepDuration
}

func (c *Cluster) PacketSent(size int) {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.bytesSentNonProbe += size
}

func (c *Cluster) Process(p *Prober) bool {
	c.lock.RLock()

	// if already past deadline, end the cluster
	timeElapsed := time.Since(c.startTime)
	if timeElapsed > c.maxDuration {
		// LK-TODO log information about short fall in probing
		c.lock.RUnlock()
		return false
	}

	// Calculate number of probe bytes that should have been sent since start.
	// Overall goal is to send desired number of probe bytes in minDuration.
	// However it is possible that timeElapsed is more than minDuration due
	// to scheduling variance. When overshooting time budget, use a capped
	// short fall if there a grace period given.
	windowDone := float64(timeElapsed) / float64(c.minDuration)
	if windowDone > 1.0 {
		// cluster has been running for longer than  minDuration
		windowDone = 1.0
	}

	bytesShouldHaveBeenSent := int(windowDone * float64(c.desiredBytes))
	bytesShortFall := bytesShouldHaveBeenSent - c.bytesSentProbe - c.bytesSentNonProbe
	if bytesShortFall < 0 {
		bytesShortFall = 0
	}
	// cap short fall to limit to 8 packets in an iteration
	// 275 bytes per packet (255 max RTP padding payload + 20 bytes RTP header)
	if bytesShortFall > (275 * 8) {
		bytesShortFall = 275 * 8
	}
	// round up to packet size
	bytesShortFall = ((bytesShortFall + 274) / 275) * 275
	c.lock.RUnlock()

	bytesSent := 0
	if bytesShortFall > 0 && p.onSendProbe != nil {
		bytesSent = p.onSendProbe(bytesShortFall)
	}

	c.lock.Lock()
	c.bytesSentProbe += bytesSent

	// do not end cluster until minDuration elapses even if rate is achieved.
	// Ensures that the next cluster (if any) does not start early.
	if (c.bytesSentProbe+c.bytesSentNonProbe) >= c.desiredBytes && timeElapsed >= c.minDuration {
		// LK-TODO - log data about how much time the probe finished compared to min/max
		c.lock.Unlock()
		return false
	}

	// LK-TODO look at adapting sleep time based on how many bytes and how much time is left
	c.lock.Unlock()
	return true
}

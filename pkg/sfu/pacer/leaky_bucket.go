package pacer

import (
	"sync"
	"time"

	"github.com/gammazero/deque"
	"github.com/livekit/protocol/logger"
)

const (
	maxOvershootFactor = 2.0
)

type LeakyBucket struct {
	*Base

	logger logger.Logger

	lock      sync.RWMutex
	packets   deque.Deque[Packet]
	interval  time.Duration
	bitrate   int
	isStopped bool
}

func NewLeakyBucket(logger logger.Logger, interval time.Duration, bitrate int) *LeakyBucket {
	l := &LeakyBucket{
		Base:     NewBase(logger),
		logger:   logger,
		interval: interval,
		bitrate:  bitrate,
	}
	l.packets.SetMinCapacity(9)

	go l.sendWorker()
	return l
}

func (l *LeakyBucket) SetInterval(interval time.Duration) {
	l.lock.Lock()
	defer l.lock.Unlock()

	l.interval = interval
}

func (l *LeakyBucket) SetBitrate(bitrate int) {
	l.lock.Lock()
	defer l.lock.Unlock()

	l.bitrate = bitrate
}

func (l *LeakyBucket) Stop() {
	l.lock.Lock()
	if l.isStopped {
		l.lock.Unlock()
		return
	}

	l.isStopped = true
	l.lock.Unlock()
}

func (l *LeakyBucket) Enqueue(p Packet) {
	l.lock.Lock()
	defer l.lock.Unlock()

	if !l.isStopped {
		l.packets.PushBack(p)
	}
}

func (l *LeakyBucket) sendWorker() {
	l.lock.RLock()
	interval := l.interval
	bitrate := l.bitrate
	l.lock.RUnlock()

	timer := time.NewTimer(interval)
	overage := 0

	for {
		<-timer.C

		l.lock.RLock()
		interval = l.interval
		bitrate = l.bitrate
		l.lock.RUnlock()

		// calculate number of bytes that can be sent in this interval
		// adjusting for overage.
		intervalBytes := int(interval.Seconds() * float64(bitrate) / 8.0)
		maxOvershootBytes := int(float64(intervalBytes) * maxOvershootFactor)
		toSendBytes := intervalBytes - overage
		if toSendBytes < 0 {
			// too much overage, wait for next interval
			overage = -toSendBytes
			timer.Reset(interval)
			continue
		}

		// do not allow too much overshoot in an interval
		if toSendBytes > maxOvershootBytes {
			toSendBytes = maxOvershootBytes
		}

		for {
			l.lock.Lock()
			if l.isStopped {
				l.lock.Unlock()
				return
			}

			if l.packets.Len() == 0 {
				l.lock.Unlock()
				// allow overshoot in next interval with shortage in this interval
				overage = -toSendBytes
				timer.Reset(interval)
				break
			}
			p := l.packets.PopFront()
			l.lock.Unlock()

			written, _ := l.Base.SendPacket(&p)
			toSendBytes -= written
			if toSendBytes < 0 {
				// overage, wait for next interval
				overage = -toSendBytes
				timer.Reset(interval)
				break
			}
		}
	}
}

// ------------------------------------------------

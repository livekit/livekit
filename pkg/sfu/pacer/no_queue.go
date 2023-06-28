package pacer

import (
	"sync"

	"github.com/gammazero/deque"
	"github.com/livekit/protocol/logger"
)

type NoQueue struct {
	*Base

	logger logger.Logger

	lock      sync.RWMutex
	packets   deque.Deque[Packet]
	wake      chan struct{}
	isStopped bool
}

func NewNoQueue(logger logger.Logger) *NoQueue {
	n := &NoQueue{
		Base:   NewBase(logger),
		logger: logger,
		wake:   make(chan struct{}, 1),
	}
	n.packets.SetMinCapacity(9)

	go n.sendWorker()
	return n
}

func (n *NoQueue) Stop() {
	n.lock.Lock()
	if n.isStopped {
		n.lock.Unlock()
		return
	}

	close(n.wake)
	n.isStopped = true
	n.lock.Unlock()
}

func (n *NoQueue) Enqueue(p Packet) {
	n.lock.Lock()
	defer n.lock.Unlock()

	n.packets.PushBack(p)
	if n.packets.Len() == 1 && !n.isStopped {
		select {
		case n.wake <- struct{}{}:
		default:
		}
	}
}

func (n *NoQueue) sendWorker() {
	for {
		<-n.wake
		for {
			n.lock.Lock()
			if n.isStopped {
				n.lock.Unlock()
				return
			}

			if n.packets.Len() == 0 {
				n.lock.Unlock()
				break
			}
			p := n.packets.PopFront()
			n.lock.Unlock()

			n.Base.SendPacket(&p)
		}
	}
}

// ------------------------------------------------

package utils

import (
	"sync"
)

const (
	MaxOps = 50
)

type OpsQueue struct {
	lock      sync.RWMutex
	ops       chan func()
	isStopped bool
}

func NewOpsQueue() *OpsQueue {
	return &OpsQueue{
		ops: make(chan func(), MaxOps),
	}
}

func (oq *OpsQueue) Start() {
	go oq.process()
}

func (oq *OpsQueue) Stop() {
	oq.lock.Lock()
	if oq.isStopped {
		oq.lock.Unlock()
		return
	}

	oq.isStopped = true
	close(oq.ops)
	oq.lock.Unlock()
}

func (oq *OpsQueue) Enqueue(op func()) {
	oq.lock.RLock()
	if oq.isStopped {
		oq.lock.RUnlock()
		return
	}

	oq.ops <- op
	oq.lock.RUnlock()
}

func (oq *OpsQueue) process() {
	for op := range oq.ops {
		op()
	}
}

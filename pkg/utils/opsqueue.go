package utils

import (
	"sync"

	"github.com/livekit/protocol/logger"
)

type OpsQueue struct {
	logger logger.Logger
	name   string
	size   int

	lock      sync.RWMutex
	ops       chan func()
	isStarted bool
	isStopped bool
}

func NewOpsQueue(logger logger.Logger, name string, size int) *OpsQueue {
	return &OpsQueue{
		logger: logger,
		name:   name,
		size:   size,
		ops:    make(chan func(), size),
	}
}

func (oq *OpsQueue) SetLogger(logger logger.Logger) {
	oq.logger = logger
}

func (oq *OpsQueue) Start() {
	oq.lock.Lock()
	if oq.isStarted {
		oq.lock.Unlock()
		return
	}

	oq.isStarted = true
	oq.lock.Unlock()

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

func (oq *OpsQueue) IsStarted() bool {
	oq.lock.RLock()
	defer oq.lock.RUnlock()

	return oq.isStarted
}

func (oq *OpsQueue) Enqueue(op func()) {
	oq.lock.RLock()
	if oq.isStopped {
		oq.lock.RUnlock()
		return
	}

	select {
	case oq.ops <- op:
	default:
		oq.logger.Errorw("ops queue full", nil, "name", oq.name, "size", oq.size)
	}
	oq.lock.RUnlock()
}

func (oq *OpsQueue) process() {
	for op := range oq.ops {
		op()
	}
}

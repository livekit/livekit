package rpc

import (
	"context"
	"sync/atomic"
	"unsafe"
)

type raceResult[T any] struct {
	i   int
	val *T
	err error
}

type Race[T any] struct {
	ctx       context.Context
	cancel    context.CancelFunc
	nextIndex int
	res       unsafe.Pointer
}

// NewRace creates a race to yield the result from one or more candidate
// functions
func NewRace[T any](ctx context.Context) *Race[T] {
	ctx, cancel := context.WithCancel(ctx)
	return &Race[T]{
		ctx:    ctx,
		cancel: cancel,
	}
}

// Go adds a candidate function to the race by running it in a new goroutine
func (r *Race[T]) Go(fn func(ctx context.Context) (*T, error)) {
	i := r.nextIndex
	r.nextIndex++

	go func() {
		val, err := fn(r.ctx)
		atomic.CompareAndSwapPointer(&r.res, nil, (unsafe.Pointer)(&raceResult[T]{i, val, err}))
		r.cancel()
	}()
}

// Wait awaits the first complete function and returns the index and results
// or -1 if the context is cancelled before any candidate finishes.
func (r *Race[T]) Wait() (int, *T, error) {
	<-r.ctx.Done()
	if r := (*raceResult[T])(atomic.LoadPointer(&r.res)); r != nil {
		return r.i, r.val, r.err
	}
	return -1, nil, r.ctx.Err()
}

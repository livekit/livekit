package utils

import (
	"sync"
	"time"
)

func NewDebouncer(after time.Duration) *Debouncer {
	return &Debouncer{
		after: after,
	}
}

type Debouncer struct {
	mu    sync.Mutex
	after time.Duration
	timer *time.Timer
}

func (d *Debouncer) Add(f func()) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.timer != nil {
		d.timer.Stop()
	}
	d.timer = time.AfterFunc(d.after, f)
}

func (d *Debouncer) SetDuration(after time.Duration) {
	d.mu.Lock()
	d.after = after
	d.mu.Unlock()
}

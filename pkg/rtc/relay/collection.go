package relay

import (
	"sync"
)

type Collection struct {
	relays []Relay
	fs     []func(relay Relay)

	mu sync.Mutex
}

func NewCollection() *Collection {
	return &Collection{}
}

// TODO: async
func (c *Collection) AddRelay(r Relay) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.relays = append(c.relays, r)
	for _, f := range c.fs {
		f(r)
	}
}

// TODO: async
func (c *Collection) OnceForEach(f func(relay Relay)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.fs = append(c.fs, f)
	for _, r := range c.relays {
		f(r)
	}
}

func (c *Collection) ForEach(f func(relay Relay)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, r := range c.relays {
		f(r)
	}
}

func (c *Collection) RemoveRelay(relay Relay) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i, r := range c.relays {
		if r.ID() == relay.ID() {
			c.relays = append(c.relays[:i], c.relays[i+1:]...)
			break
		}
	}
	for i, f := range c.fs {
		if f == nil {
			c.fs = append(c.fs[:i], c.fs[i+1:]...)
			break
		}
	}
}
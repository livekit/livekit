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

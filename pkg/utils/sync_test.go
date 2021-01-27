package utils

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAtomicFlag(t *testing.T) {
	t.Run("single thread, basic functionality", func(t *testing.T) {
		f := AtomicFlag{}
		assert.False(t, f.TrySet(false))
		assert.False(t, f.Get())
		assert.True(t, f.TrySet(true))
		assert.True(t, f.Get())
	})

	t.Run("only one thread should succeed", func(t *testing.T) {
		var numSets uint32
		f := AtomicFlag{}
		wg := sync.WaitGroup{}
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				if f.TrySet(true) {
					atomic.AddUint32(&numSets, 1)
				}
				wg.Done()
			}()
		}
		wg.Wait()

		assert.Equal(t, 1, int(numSets))
	})
}

func TestCalmChannel(t *testing.T) {
	t.Run("should receive nil after close", func(t *testing.T) {
		c := NewCalmChannel(1)
		c.Close()

		assert.Equal(t, nil, <-c.ReadChan())
	})
}

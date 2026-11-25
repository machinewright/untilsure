// Package example is a small cache with a flaky test, used in untilsure's README.
package example

import (
	"sync"
	"time"
)

// Cache stores values and refreshes them in the background.
type Cache struct {
	mu   sync.Mutex
	vals map[string]string
}

func New() *Cache { return &Cache{vals: map[string]string{}} }

// Refresh loads a new value in the background and returns at once.
// The returned channel is closed when the value is stored.
func (c *Cache) Refresh(key string, load func() string) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		v := load()
		c.mu.Lock()
		c.vals[key] = v
		c.mu.Unlock()
		close(done)
	}()
	return done
}

func (c *Cache) Get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.vals[key]
	return v, ok
}

// slowLoad stands in for a network call that usually takes ~1ms but sometimes longer.
func slowLoad(jitter func() time.Duration) func() string {
	return func() string { time.Sleep(time.Millisecond + jitter()); return "fresh" }
}

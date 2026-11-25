package example

import (
	"math/rand/v2"
	"os"
	"testing"
	"time"
)

// Network latency: usually about 1ms, occasionally a slow request.
func jitter() time.Duration {
	if rand.IntN(25) == 0 {
		return 20 * time.Millisecond
	}
	return time.Duration(rand.IntN(500)) * time.Microsecond
}

// The flaky tests run only under untilsure (which sets UNTILSURE_RUN), so that
// "go test ./..." stays reliable.
func demoOnly(t *testing.T) {
	if os.Getenv("UNTILSURE_RUN") == "" {
		t.Skip("deliberately flaky demo; run it with untilsure")
	}
}

// Flaky: waits a fixed 10ms and hopes the refresh is done by then.
func TestRefreshSleep(t *testing.T) {
	demoOnly(t)
	c := New()
	c.Refresh("k", slowLoad(jitter))
	time.Sleep(10 * time.Millisecond)
	if v, _ := c.Get("k"); v != "fresh" {
		t.Fatalf("got %q, want fresh", v)
	}
}

// The tempting fix: a longer sleep. Faster, rarer, still there.
func TestRefreshLongerSleep(t *testing.T) {
	demoOnly(t)
	c := New()
	c.Refresh("k", slowLoad(jitter))
	time.Sleep(15 * time.Millisecond)
	if v, _ := c.Get("k"); v != "fresh" {
		t.Fatalf("got %q, want fresh", v)
	}
}

// The fix: wait for the refresh itself.
func TestRefreshWait(t *testing.T) {
	c := New()
	<-c.Refresh("k", slowLoad(jitter))
	if v, _ := c.Get("k"); v != "fresh" {
		t.Fatalf("got %q, want fresh", v)
	}
}

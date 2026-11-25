package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Result is one run of the command.
type Result struct {
	Index    int // 1-based, in start order
	Failed   bool
	TimedOut bool
	ExitCode int // -1 if killed or not started
	Dur      time.Duration
	Output   []byte // combined stdout+stderr, last outputCap bytes
	StartErr error  // the command could not be started at all
}

const outputCap = 256 << 10

// tailBuffer keeps the last outputCap bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > outputCap {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-outputCap:]...)
	}
	return len(p), nil
}

// Runner starts runs of Args on Jobs workers. Results are handed to the
// caller strictly in start order (run 1, 2, 3, ...), whatever order they
// finish in: if failures take longer than passes (timeouts, retries), deciding
// on finish order would let quick passes reach a verdict before slow failures
// are counted.
type Runner struct {
	Args    []string
	Jobs    int
	Timeout time.Duration // 0 = none; a run that times out counts as a failure
	Limit   int           // start at most this many runs (0 = no limit)
	Until   time.Time     // start no runs after this (zero = no limit)

	running atomic.Int64
}

// Running reports how many runs are in flight.
func (r *Runner) Running() int { return int(r.running.Load()) }

// Run starts runs until handle returns true (stop), ctx is cancelled, or a
// limit is reached. handle is called from a single goroutine, in start order.
// Runs still in flight when it stops are killed; runs that finished out of
// order after the stopping one are discarded. Run returns the number of
// discarded runs.
func (r *Runner) Run(ctx context.Context, handle func(Result) bool) int {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var next atomic.Int64
	type item struct {
		res Result
		ack chan struct{} // closed once the result has been handled (or set aside)
	}
	results := make(chan item)
	var wg sync.WaitGroup
	for w := 0; w < max(r.Jobs, 1); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if !r.Until.IsZero() && time.Now().After(r.Until) {
					return
				}
				i := int(next.Add(1))
				if r.Limit > 0 && i > r.Limit {
					return
				}
				res := r.runOnce(ctx, i)
				if ctx.Err() != nil && !res.TimedOut && res.StartErr == nil && res.ExitCode == -1 {
					return // killed because we're stopping: not a real result
				}
				it := item{res, make(chan struct{})}
				select {
				case results <- it:
				case <-ctx.Done():
					return
				}
				// Wait until the result is handled, so that a verdict stops
				// this worker before it starts another run (exactly, with -j 1).
				select {
				case <-it.ack:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() { wg.Wait(); close(results) }()

	pending := map[int]Result{}
	want, discarded, stopped := 1, 0, false
	for it := range results {
		if stopped {
			discarded++
			close(it.ack)
			continue
		}
		res := it.res
		pending[res.Index] = res
		for {
			p, ok := pending[want]
			if !ok {
				break
			}
			delete(pending, want)
			want++
			if handle(p) {
				stopped = true
				cancel()
				break
			}
		}
		if stopped {
			discarded += len(pending)
		}
		close(it.ack)
	}
	return discarded
}

func (r *Runner) runOnce(ctx context.Context, i int) Result {
	r.running.Add(1)
	defer r.running.Add(-1)
	rctx := ctx
	if r.Timeout > 0 {
		var c context.CancelFunc
		rctx, c = context.WithTimeout(ctx, r.Timeout)
		defer c()
	}
	cmd := exec.CommandContext(rctx, r.Args[0], r.Args[1:]...)
	cmd.Env = append(os.Environ(), "UNTILSURE_RUN="+strconv.Itoa(i))
	var out tailBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	setProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second
	start := time.Now()
	err := cmd.Run()
	res := Result{Index: i, Dur: time.Since(start), Output: out.buf, ExitCode: 0}
	if err == nil {
		return res
	}
	res.Failed = true
	res.ExitCode = -1
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee) && ee.Exited():
		res.ExitCode = ee.ExitCode()
	case cmd.Process == nil:
		res.StartErr = err
	}
	if r.Timeout > 0 && errors.Is(rctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		res.TimedOut = true
		res.ExitCode = -1
	}
	return res
}

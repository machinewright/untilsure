# untilsure

You fixed a flaky test. Did it work? Run it 10 times? 100? `untilsure` reruns it until the evidence is
decisive, then says **FIXED**, **NOT FIXED** or, if it hits its cap, **UNDECIDED**, with the chance of a wrong
verdict worked out exactly.

```console
$ untilsure --was 2% -j 4 -- go test -count=1 -run 'TestRefreshWait$' ./example
untilsure: still failing 2.0% of runs, or at least 10x less often (0.2%)?
  Wrong verdict: 4.9% if it's still broken, 2.3% if it's fixed. Expected runs: 124 if broken, 209 if fixed; at most 2090.
FIXED: 0 failures in 164 runs. A test still failing 2.0% of the time gets this verdict 4.9% of the time.
```

It runs any command: a single test, a test file, a script. A run fails if the command exits non-zero or hits
`--timeout`.

## Install

```sh
go install github.com/machinewright/untilsure@latest
```

Or download a binary for Linux or macOS from the [releases](https://github.com/machinewright/untilsure/releases)
page, unpack it and put `untilsure` on your PATH. On macOS, a downloaded binary isn't notarized: if it's blocked,
run `xattr -d com.apple.quarantine untilsure` once, or use `go install`.

Linux and macOS. (It builds elsewhere, but only Unix systems let it stop the processes a run leaves behind.)
Needs Go 1.22 or newer to build; no dependencies outside the standard library.

## Use

### A worked example

[`example/`](example) has a cache that refreshes values in the background, and a test that waits a fixed 10ms
for the refresh before checking it. The refresh usually takes about 1ms, but one call in 25 is slow (20ms). All
the output below is real, from a laptop with `-j 4`.

**1. Know the old failure rate.** If you know roughly how often it failed before (from CI history, say 1 run in
20), pass that as `--was 5%` (or `0.05`, or `1/20`). If you don't, measure it before fixing:

```console
$ untilsure --measure --runs 200 -j 4 -- go test -count=1 -run 'TestRefreshSleep$' ./example
9 failures in 200 runs: 4.5% (95% interval 2.0% to 8.4%).
  To check a fix, a cautious --was is the low end: if --was is above the real old rate,
  a test that is still broken is more likely to be called FIXED.
  untilsure --was 2.0% -j 4 -- go test -count=1 -run 'TestRefreshSleep$' ./example
Failure logs (first 5 of 9): /tmp/untilsure-1144086051/run-39.log /tmp/untilsure-1144086051/run-69.log /tmp/untilsure-1144086051/run-78.log /tmp/untilsure-1144086051/run-110.log /tmp/untilsure-1144086051/run-127.log
```

The counts differ from run to run (another measurement here gave 6 failures and `--was 1.1%`); the plan lines
below depend only on the options.

Why the low end: the guarantee ("a test still failing at the old rate is called FIXED at most 5% of the time")
holds for any rate at or above `--was`. If `--was` is above the real rate, a test that is still broken sits
between the two hypotheses and can come out FIXED more often. The low end of a 95% interval is above the real
rate only about 2.5% of the time. The price is that FIXED now means "10x below the low end", a stricter bar
that takes more runs. (`-count=1` stops Go from caching a passing result.)

**2. Check the fix.** The tempting fix is a longer sleep (15ms). It makes failures rarer but doesn't remove them:

```console
$ untilsure --was 2% -j 4 -- go test -count=1 -run 'TestRefreshLongerSleep$' ./example
untilsure: still failing 2.0% of runs, or at least 10x less often (0.2%)?
  Wrong verdict: 4.9% if it's still broken, 2.3% if it's fixed. Expected runs: 124 if broken, 209 if fixed; at most 2090.
NOT FIXED: 2 failures in 43 runs (4.7%).
Failure logs: /tmp/untilsure-2616951037/run-24.log /tmp/untilsure-2616951037/run-43.log
```

The real fix waits for the refresh to finish (`TestRefreshWait`). That is the FIXED run at the top of this page.
Exit status 0 means FIXED, 1 NOT FIXED, 2 UNDECIDED, 3 a usage error or a command that can't be started, so you
can use it in a script or in CI.

**What FIXED means.** By default it means "fails at least 10 times less often than before", not "never fails".
Nothing short of infinitely many runs shows "never". Change the bar with `--better`: `--better 2` asks only
whether it got twice as good, which takes far more runs to decide (about 300 at 5%). `--risk` sets the chance of
a wrong verdict each way (default 5%).

## How it decides

It runs Wald's sequential probability ratio test between two hypotheses: "still failing at the old rate" and
"failing `--better` times less often". After every run it weighs the evidence so far. Each pass is a little
evidence for "fixed", and each failure is a lot of evidence for "still broken". It stops as soon as the balance
passes a threshold, so you never have to choose a run count in advance. A test that fails visibly often is caught
quickly (the longer-sleep "fix" above: 43 runs), while confirming a fix takes as many runs as the evidence needs.

The plan printed at the start gives the exact error rates and expected lengths. These are computed by dynamic
programming over every possible sequence of passes and failures, cap included, not taken from Wald's
approximations. Wald's textbook thresholds turn out to overshoot slightly. At a 1% failure rate, for example, they
call a still-broken test FIXED 5.1% of the time instead of 5%. So `untilsure` moves the threshold until the exact
rate is within `--risk`. The test suite checks this over a grid of rates, ratios and risks, and checks the exact
numbers against simulation.

**Parallel runs (`-j`) are counted in start order.** Failures are often slower than passes (timeouts, retries,
teardown). If results were counted as they finish, a burst of quick passes could reach FIXED while slow failures
were still running. `untilsure` hands results to the test in the order the runs started, and ignores runs that
finish after the verdict. Running in parallel also changes the load on the machine, which can change how often a
test flakes. Use the same `-j` when you measure and when you check.

## Options

```
--was RATE      how often it failed before the fix: 5%, 0.05 or 1/20
--better X      "fixed" means failing at least X times less often (default 10)
--risk RATE     allowed chance of a wrong verdict, each way (default 5%)
--max N         stop UNDECIDED after N runs (default: 10x the expected length)
--measure       estimate the failure rate instead (with --runs N, default 100, or --for 10m)
-j N            runs at once (default 1)
--timeout D     per-run limit, e.g. 30s; a run that hits it counts as a failure
--logs DIR      where to save failure output (default: a new temporary directory)
--keep N        save the output of at most N failures (default 5)
-q              no progress line
```

Each run gets `UNTILSURE_RUN=<n>` in its environment, which is useful for a unique port, temp dir or seed.
Each run is started in its own process group, and anything it leaves running (a server, a browser) is killed
when it ends, so runs stay independent. Ctrl-C, `kill` or a closed terminal stops the runs and reports what was
seen so far (UNDECIDED); a second Ctrl-C exits at once. If the very first run exits with 126 or 127,
which a shell uses for "not executable" or "command not found", it stops with an error instead of calling that a
flaky failure.

## Limits

- Very rare failures can't be checked this way. Below roughly 1 failure in 30,000 runs (with the default `--better 10`), FIXED would
  need more than 100,000 passing runs in a row, and `untilsure` says so instead of starting.

- The statistics assume runs are independent with a steady failure rate. A test that fails only after another
  test has left state behind, or only at certain times of day, breaks that assumption. So does a failure rate
  that creeps up as runs pile up on the machine. Rerunning one test in a loop may simply never hit the failure CI
  sees. Then `--measure` reports few or no failures, which is itself worth knowing before you call anything fixed.
- FIXED is a statement about the rate in this environment. It says nothing about why the test flaked.

## Development

`go test ./...` runs the tests: exact error rates against simulation, the risk guarantee over a grid,
Clopper-Pearson intervals against binomial sums, start-order delivery with slow failures, timeouts, killing
runs in flight, and the command-line behaviour.

Made by Machinewright, an AI agent (Claude) run by a human owner. Code: MIT license. Bug reports and
corrections are welcome as issues.

// Command untilsure reruns a flaky test until it is sure whether your fix worked.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const usage = `untilsure reruns a flaky command until it is sure whether your fix worked.

Check a fix (the test used to fail about 1 run in 20):
  untilsure --was 5%% -- go test -run TestUpload ./api

Measure how often it fails (before a fix, or to learn the rate for --was):
  untilsure --measure -- npm test -- -t "upload"

It runs the command again and again, stops as soon as the evidence is decisive,
and prints FIXED, NOT FIXED or UNDECIDED. A run fails if the command exits
non-zero or times out. Each run gets UNTILSURE_RUN=<n> in its environment.

Exit status: 0 FIXED, 1 NOT FIXED, 2 UNDECIDED, 3 usage or start error
(--measure: 0 when done, 3 on error).

Options:
`

// buildVersion is set by release builds (-ldflags "-X main.buildVersion=..."); go install uses the module version.
var buildVersion string

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(argv []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("untilsure", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintf(stderr, usage); fs.PrintDefaults() }
	was := fs.String("was", "", "how often it failed before the fix: 5%, 0.05 or 1/20")
	better := fs.Float64("better", 10, "\"fixed\" means failing at least this many times less often")
	risk := fs.String("risk", "5%", "allowed chance of a wrong verdict, each way")
	maxRuns := fs.Int("max", 0, "stop UNDECIDED after this many runs (default: 10x the expected length)")
	measure := fs.Bool("measure", false, "estimate the failure rate instead of checking a fix")
	runs := fs.Int("runs", 100, "with --measure: number of runs")
	dur := fs.Duration("for", 0, "with --measure: run for this long instead of --runs (e.g. 10m)")
	jobs := fs.Int("j", 1, "runs at once (parallel runs can change how often a test flakes)")
	timeout := fs.Duration("timeout", 0, "per-run time limit; a run that hits it counts as a failure")
	logs := fs.String("logs", "", "directory for failure logs (default: a new temporary directory)")
	keep := fs.Int("keep", 5, "save the output of at most this many failures")
	quiet := fs.Bool("q", false, "no progress line")
	version := fs.Bool("version", false, "print version")
	if err := fs.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 3
	}
	if *version {
		v := buildVersion
		if bi, ok := debug.ReadBuildInfo(); v == "" && ok && bi.Main.Version != "" {
			v = bi.Main.Version
		}
		if v == "" {
			v = "(devel)"
		}
		fmt.Fprintln(stdout, "untilsure", v)
		return 0
	}
	args := fs.Args()
	if len(args) == 0 {
		fmt.Fprintln(stderr, "untilsure: no command given. Put it after --, e.g. untilsure --was 5% -- go test ./...")
		return 3
	}
	if *measure == (*was != "") {
		fmt.Fprintln(stderr, "untilsure: give either --was RATE (to check a fix) or --measure (to estimate the rate)")
		return 3
	}
	rk, err := parseRate(*risk)
	if err != nil {
		fmt.Fprintln(stderr, "untilsure: --risk:", err)
		return 3
	}

	if *jobs < 1 {
		fmt.Fprintln(stderr, "untilsure: -j must be at least 1")
		return 3
	}
	if *measure && *runs < 1 {
		fmt.Fprintln(stderr, "untilsure: --runs must be at least 1")
		return 3
	}
	if !*measure {
		bad := ""
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "runs" || f.Name == "for" {
				bad = f.Name
			}
		})
		if bad != "" {
			fmt.Fprintf(stderr, "untilsure: --%s only applies with --measure\n", bad)
			return 3
		}
	}

	// Ctrl-C, kill or a closed terminal stop the runs and report what was seen.
	// A second signal exits at once.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	go func() { <-ctx.Done(); stop() }()
	r := &Runner{Args: args, Jobs: *jobs, Timeout: *timeout}
	saver := &logSaver{dir: *logs, keep: *keep}
	prog := newProgress(stderr, *quiet)

	if *measure {
		if *dur > 0 {
			r.Until = time.Now().Add(*dur)
		} else {
			r.Limit = *runs
		}
		var n, f int
		var startErr error
		r.Run(ctx, func(res Result) bool {
			if bad := badStart(res, n); bad != nil {
				startErr = bad
				return true
			}
			n++
			if res.Failed {
				f++
				saver.save(res)
			}
			prog.show(n, f, r.Running())
			return false
		})
		prog.clear()
		if startErr != nil {
			fmt.Fprintln(stderr, "untilsure:", startErr)
			return 3
		}
		if n == 0 {
			fmt.Fprintln(stderr, "untilsure: no runs finished")
			return 3
		}
		extra := ""
		if *jobs != 1 {
			extra += fmt.Sprintf(" -j %d", *jobs)
		}
		if *timeout > 0 {
			extra += " --timeout " + timeout.String()
		}
		fmt.Fprintln(stdout, measureReport(n, f, args, extra))
		if ctx.Err() != nil {
			fmt.Fprintln(stdout, "(interrupted)")
		}
		saver.report(stdout)
		return 0
	}

	p0, err := parseRate(*was)
	if err != nil {
		if v, e := strconv.ParseFloat(strings.TrimSpace(*was), 64); e == nil && v >= 1 && v < 100 {
			err = fmt.Errorf("%w; did you mean %s%%?", err, strings.TrimSpace(*was))
		}
		fmt.Fprintln(stderr, "untilsure: --was:", err)
		return 3
	}
	pl, err := NewPlan(p0, *better, rk, *maxRuns)
	if err != nil {
		fmt.Fprintln(stderr, "untilsure:", err)
		return 3
	}
	if pl.MinRunsToFix() == 0 {
		fmt.Fprintf(stderr, "untilsure: with --max %d this check can never say FIXED; leave out --max, or raise it\n", pl.Max)
		return 3
	}
	fmt.Fprint(stderr, planText(pl))
	r.Limit = pl.Max
	var n, f int
	verdict := Continue
	var startErr error
	discarded := r.Run(ctx, func(res Result) bool {
		if bad := badStart(res, n); bad != nil {
			startErr = bad
			return true
		}
		n++
		if res.Failed {
			f++
			saver.save(res)
		}
		prog.show(n, f, r.Running())
		verdict = pl.Decide(n, f)
		return verdict != Continue
	})
	prog.clear()
	if startErr != nil {
		fmt.Fprintln(stderr, "untilsure:", startErr)
		return 3
	}
	if verdict == Continue {
		verdict = Undecided // interrupted
	}
	fmt.Fprintln(stdout, verdictText(pl, verdict, n, f, ctx.Err() != nil))
	if discarded > 0 {
		fmt.Fprintf(stdout, "(%d %s that finished after the verdict %s not counted: results are counted in start order.)\n",
			discarded, plural(discarded, "run", "runs"), plural(discarded, "was", "were"))
	}
	saver.report(stdout)
	return map[Verdict]int{Fixed: 0, NotFixed: 1, Undecided: 2}[verdict]
}

// badStart returns an error if the run shows the command itself is wrong
// rather than flaky: it couldn't be started, or the very first run says
// "command not found" / "not executable" in the shell's way.
func badStart(res Result, countedSoFar int) error {
	if res.StartErr != nil {
		return fmt.Errorf("could not start the command: %v", res.StartErr)
	}
	if countedSoFar == 0 && (res.ExitCode == 126 || res.ExitCode == 127) {
		msg := fmt.Errorf("the first run exited with %d, which usually means the command was not found or not executable; not counting it as a flaky failure", res.ExitCode)
		if len(bytes.TrimSpace(res.Output)) > 0 {
			msg = fmt.Errorf("%v.\n%s", msg, lastLines(res.Output, 5))
		}
		return msg
	}
	return nil
}

// parseRate reads "5%", "0.05" or "1/20".
func parseRate(s string) (float64, error) {
	s = strings.TrimSpace(s)
	var v float64
	var err error
	switch {
	case strings.HasSuffix(s, "%"):
		v, err = strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(s, "%")), 64)
		v /= 100
	case strings.Contains(s, "/"):
		a, b, _ := strings.Cut(s, "/")
		var x, y float64
		x, err = strconv.ParseFloat(strings.TrimSpace(a), 64)
		if err == nil {
			y, err = strconv.ParseFloat(strings.TrimSpace(b), 64)
		}
		if err == nil && y == 0 {
			err = errors.New("division by zero")
		}
		if err == nil {
			v = x / y
		}
	default:
		v, err = strconv.ParseFloat(s, 64)
	}
	if err != nil {
		return 0, fmt.Errorf("can't read %q as a rate (use 5%%, 0.05 or 1/20)", s)
	}
	if !(v > 0 && v < 1) {
		return 0, fmt.Errorf("%q is not a rate between 0 and 1", s)
	}
	return v, nil
}

// pctDown and pctUp round to the precision pct shows, down or up, for bounds
// whose rounding must not make a claim stronger than it is.
func pctDown(x float64) string { return pct(roundAt(x, math.Floor)) }
func pctUp(x float64) string   { return pct(roundAt(x, math.Ceil)) }

func roundAt(x float64, r func(float64) float64) float64 {
	if x <= 0 || x >= 1 {
		return x
	}
	d := 0.0 // digits after the decimal point in percent, as pct prints them
	switch {
	case x >= 0.1:
	case x >= 0.01:
		d = 1
	default:
		d = 1 - math.Floor(math.Log10(100*x)) // two significant digits
	}
	k := math.Pow(10, d) * 100
	return r(x*k) / k
}

func pct(x float64) string {
	if x > 0 && x < 1e-6 {
		return "<0.0001%"
	}
	switch {
	case x == 0:
		return "0%"
	case x >= 0.1:
		return strconv.FormatFloat(100*x, 'f', 0, 64) + "%"
	case x >= 0.01:
		return strconv.FormatFloat(100*x, 'f', 1, 64) + "%"
	}
	return strconv.FormatFloat(100*x, 'g', 2, 64) + "%"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func planText(pl *Plan) string {
	b, f := pl.Exact(pl.P0), pl.Exact(pl.P1)
	return fmt.Sprintf(`untilsure: still failing %s of runs, or at least %gx less often (%s)?
  Wrong verdict: %s if it's still broken, %s if it's fixed. Expected runs: %.0f if broken, %.0f if fixed; at most %d.
`, pct(pl.P0), pl.P0/pl.P1, pct(pl.P1), pct(b.PFixed), pct(f.PNotFixed), b.MeanRuns, f.MeanRuns, pl.Max)
}

func verdictText(pl *Plan, v Verdict, n, f int, interrupted bool) string {
	obs := fmt.Sprintf("%d %s in %d %s", f, plural(f, "failure", "failures"), n, plural(n, "run", "runs"))
	lo, hi := Interval(f, n, 0.95)
	switch v {
	case Fixed:
		return fmt.Sprintf("FIXED: %s. A test still failing %s of the time gets this verdict %s of the time.",
			obs, pct(pl.P0), pct(pl.Exact(pl.P0).PFixed))
	case NotFixed:
		return fmt.Sprintf("NOT FIXED: %s (%s).", obs, pct(float64(f)/float64(n)))
	}
	why := fmt.Sprintf("reached --max %d", pl.Max)
	if interrupted {
		why = "interrupted"
	}
	if interrupted && n > 0 {
		return fmt.Sprintf("UNDECIDED (interrupted): %s (%s), not yet enough to decide.", fmt.Sprintf("%d %s in %d %s", f, plural(f, "failure", "failures"), n, plural(n, "run", "runs")), pct(float64(f)/float64(n)))
	}
	if n == 0 {
		return "UNDECIDED (" + why + "): no runs finished."
	}
	s := fmt.Sprintf("UNDECIDED (%s): %s (%s).", why, obs, pct(float64(f)/float64(n)))
	switch {
	case hi < pl.P0 && lo > pl.P1:
		s += fmt.Sprintf("\n  It fails less often than before (%s), but not clearly %gx less.", pct(pl.P0), pl.P0/pl.P1)
	case lo > pl.P1:
		s += fmt.Sprintf("\n  Not clearly fixed: the rate may still be near %s.", pct(pl.P0))
	default:
		s += "\n  Not enough runs to tell. Raise --max, or leave out --max for the automatic cap."
	}
	return s
}

func measureReport(n, f int, args []string, extra string) string {
	obs := fmt.Sprintf("%d %s in %d %s", f, plural(f, "failure", "failures"), n, plural(n, "run", "runs"))
	if f == 0 {
		return fmt.Sprintf("%s: the failure rate is at most %s (95%% upper bound).\n"+
			"  No failures, so there is nothing to check a fix against yet. Try more runs, -j to add load, or --for 10m.",
			obs, pctUp(UpperBound(0, n, 0.95)))
	}
	lo, hi := Interval(f, n, 0.95)
	s := fmt.Sprintf("%s: %s (95%% interval %s to %s).", obs, pct(float64(f)/float64(n)), pctDown(lo), pctUp(hi))
	if f < n {
		s += fmt.Sprintf("\n  To check a fix, a cautious --was is the low end: if --was is above the real old rate,\n"+
			"  a test that is still broken is more likely to be called FIXED.\n"+
			"  untilsure --was %s%s -- %s", pctDown(lo), extra, strings.Join(quoteArgs(args), " "))
	} else {
		s += "\n  It failed every time: that's a broken test, not a flaky one."
	}
	return s
}

func quoteArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\"'$`\\|&;<>()*?[]{}~!#") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		out[i] = a
	}
	return out
}

func lastLines(b []byte, k int) string {
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > k {
		lines = lines[len(lines)-k:]
	}
	return "  | " + strings.Join(lines, "\n  | ")
}

// logSaver writes the output of the first few failures (in start order).
type logSaver struct {
	dir   string
	keep  int
	saved []string
	err   error
	total int
}

func (s *logSaver) save(res Result) {
	s.total++
	if len(s.saved) >= s.keep || s.err != nil {
		return
	}
	if s.dir == "" {
		s.dir, s.err = os.MkdirTemp("", "untilsure-")
	} else {
		s.err = os.MkdirAll(s.dir, 0o755)
	}
	if s.err != nil {
		return
	}
	head := fmt.Sprintf("# run %d: ", res.Index)
	if res.TimedOut {
		head += "timed out"
	} else {
		head += fmt.Sprintf("exit code %d", res.ExitCode)
	}
	head += fmt.Sprintf(" after %s\n", res.Dur.Round(time.Millisecond))
	p := filepath.Join(s.dir, fmt.Sprintf("run-%d.log", res.Index))
	if s.err = os.WriteFile(p, append([]byte(head), res.Output...), 0o644); s.err == nil {
		s.saved = append(s.saved, p)
	}
}

func (s *logSaver) report(w io.Writer) {
	if s.err != nil {
		fmt.Fprintln(w, "Could not save failure logs:", s.err)
	}
	if len(s.saved) == 0 {
		return
	}
	more := ""
	if s.total > len(s.saved) {
		more = fmt.Sprintf(" (first %d of %d)", len(s.saved), s.total)
	}
	fmt.Fprintf(w, "Failure logs%s: %s\n", more, strings.Join(s.saved, " "))
}

// progress draws a one-line status on a terminal.
type progress struct {
	w    io.Writer
	on   bool
	last time.Time
}

func newProgress(w io.Writer, quiet bool) *progress {
	on := false
	if f, ok := w.(*os.File); ok && !quiet {
		if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			on = true
		}
	}
	return &progress{w: w, on: on}
}

func (p *progress) show(n, f, running int) {
	if !p.on || time.Since(p.last) < 100*time.Millisecond {
		return
	}
	p.last = time.Now()
	fmt.Fprintf(p.w, "\r\033[K  %d %s, %d %s, %d running", n, plural(n, "run", "runs"), f, plural(f, "failure", "failures"), running)
}

func (p *progress) clear() {
	if p.on {
		fmt.Fprint(p.w, "\r\033[K")
	}
}

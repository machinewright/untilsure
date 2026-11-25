package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Every 4th run fails, and failures are slow: with 8 workers, passes finish
// first, but results must still reach the handler in start order.
func TestRunnerDeliversInStartOrder(t *testing.T) {
	script := `n=$UNTILSURE_RUN; if [ $((n % 4)) -eq 0 ]; then sleep 0.2; exit 1; fi; exit 0`
	r := &Runner{Args: []string{"sh", "-c", script}, Jobs: 8, Limit: 24}
	var got []int
	r.Run(context.Background(), func(res Result) bool {
		got = append(got, res.Index)
		if want := res.Index%4 == 0; res.Failed != want {
			t.Errorf("run %d: failed=%v, want %v", res.Index, res.Failed, want)
		}
		return false
	})
	if len(got) != 24 {
		t.Fatalf("got %d results, want 24", len(got))
	}
	for i, idx := range got {
		if idx != i+1 {
			t.Fatalf("results out of order: %v", got)
		}
	}
}

func TestRunnerStopsAndKills(t *testing.T) {
	r := &Runner{Args: []string{"sh", "-c", `if [ "$UNTILSURE_RUN" -le 3 ]; then exit 0; fi; sleep 30`}, Jobs: 4}
	start := time.Now()
	r.Run(context.Background(), func(res Result) bool { return res.Index == 3 })
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("stopping took %v; in-flight runs were not killed", d)
	}
}

func TestTimeoutCountsAsFailure(t *testing.T) {
	r := &Runner{Args: []string{"sleep", "5"}, Jobs: 1, Limit: 1, Timeout: 100 * time.Millisecond}
	var res Result
	r.Run(context.Background(), func(x Result) bool { res = x; return false })
	if !res.Failed || !res.TimedOut {
		t.Errorf("got %+v, want a timed-out failure", res)
	}
}

func cli(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestCLIAlwaysPassingIsFixed(t *testing.T) {
	code, out, _ := cli(t, "--was", "5%", "-logs", t.TempDir(), "--", "true")
	pl, _ := NewPlan(0.05, 10, 0.05, 0)
	n := 1
	for pl.Decide(n, 0) == Continue {
		n++
	}
	if code != 0 || !strings.Contains(out, "FIXED: 0 failures in "+strconv.Itoa(n)+" runs") {
		t.Errorf("exit %d, output %q; want FIXED after %d runs", code, out, n)
	}
}

func TestCLIAlwaysFailingIsNotFixed(t *testing.T) {
	dir := t.TempDir()
	code, out, _ := cli(t, "--was", "5%", "-logs", dir, "--", "sh", "-c", "echo boom; exit 1")
	if code != 1 || !strings.HasPrefix(out, "NOT FIXED: ") {
		t.Errorf("exit %d, output %q", code, out)
	}
	b, err := os.ReadFile(filepath.Join(dir, "run-1.log"))
	if err != nil || !strings.Contains(string(b), "boom") || !strings.Contains(string(b), "exit code 1") {
		t.Errorf("failure log: %q, %v", b, err)
	}
}

func TestCLIBadCommand(t *testing.T) {
	if code, _, e := cli(t, "--was", "5%", "--", "/no/such/command"); code != 3 || !strings.Contains(e, "could not start") {
		t.Errorf("missing binary: exit %d, %q", code, e)
	}
	if code, _, e := cli(t, "--was", "5%", "--", "sh", "-c", "nosuchcommand-xyz"); code != 3 || !strings.Contains(e, "127") {
		t.Errorf("shell command not found: exit %d, %q", code, e)
	}
	if code, _, _ := cli(t, "--", "true"); code != 3 {
		t.Errorf("no --was and no --measure: exit %d", code)
	}
}

func TestCLIMeasure(t *testing.T) {
	code, out, _ := cli(t, "--measure", "--runs", "40", "-j", "4", "-logs", t.TempDir(), "--", "sh", "-c", `[ $((UNTILSURE_RUN % 10)) -ne 0 ]`)
	if code != 0 || !strings.HasPrefix(out, "4 failures in 40 runs: 10%") || !strings.Contains(out, "untilsure --was ") {
		t.Errorf("exit %d, output %q", code, out)
	}
}

// A run that exits 0 but leaves a background process holding its output open
// is a pass, and must not wait for that process.
func TestBackgroundProcessIsAPass(t *testing.T) {
	r := &Runner{Args: []string{"sh", "-c", "sleep 5 & echo started; exit 0"}, Jobs: 1, Limit: 2}
	start := time.Now()
	var got []Result
	r.Run(context.Background(), func(res Result) bool { got = append(got, res); return false })
	if len(got) != 2 || got[0].Failed || got[1].Failed {
		t.Errorf("got %+v, want two passes", got)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v; runs waited for their background process", d)
	}
}

func TestCLIRejectsBadInput(t *testing.T) {
	for _, args := range [][]string{
		{"--measure", "--runs", "0", "--", "true"},
		{"--was", "5%", "--better", "inf", "--", "true"},
		{"--was", "1e-7", "--", "true"},
		{"--was", "5%", "--max", "10", "--", "true"},
		{"--was", "5%", "--runs", "10", "--", "true"},
	} {
		if code, _, e := cli(t, args...); code != 3 {
			t.Errorf("%v: exit %d (%q), want 3", args, code, e)
		}
	}
	if _, _, e := cli(t, "--was", "5", "--", "true"); !strings.Contains(e, "did you mean 5%") {
		t.Errorf("--was 5: %q", e)
	}
}

func TestRounding(t *testing.T) {
	for _, c := range []struct {
		x        float64
		down, up string
	}{{0.016501, "1.6%", "1.7%"}, {0.015402, "1.5%", "1.6%"}, {0.25, "25%", "25%"}, {0.00123, "0.12%", "0.13%"}} {
		if d, u := pctDown(c.x), pctUp(c.x); d != c.down || u != c.up {
			t.Errorf("%g: down %s up %s, want %s %s", c.x, d, u, c.down, c.up)
		}
	}
}

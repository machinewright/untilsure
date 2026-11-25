package main

import (
	"math"
	"math/rand/v2"
	"testing"
)

// simulate runs the plan's decision rule on random runs, independently of Exact.
func simulate(pl *Plan, p float64, trials int, rng *rand.Rand) (fixed, notFixed, undecided, meanRuns float64) {
	for t := 0; t < trials; t++ {
		n, f := 0, 0
		for {
			n++
			if rng.Float64() < p {
				f++
			}
			v := pl.Decide(n, f)
			if v == Continue {
				continue
			}
			switch v {
			case Fixed:
				fixed++
			case NotFixed:
				notFixed++
			case Undecided:
				undecided++
			}
			break
		}
		meanRuns += float64(n)
	}
	k := float64(trials)
	return fixed / k, notFixed / k, undecided / k, meanRuns / k
}

func TestExactMatchesSimulation(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for _, c := range []struct{ p0, better, risk float64 }{{0.05, 10, 0.05}, {0.2, 4, 0.01}, {0.01, 10, 0.05}} {
		pl, err := NewPlan(c.p0, c.better, c.risk, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range []float64{pl.P0, pl.P1, math.Sqrt(pl.P0 * pl.P1)} {
			oc := pl.Exact(p)
			if s := oc.PFixed + oc.PNotFixed + oc.PUndecided; math.Abs(s-1) > 1e-9 {
				t.Errorf("%+v p=%g: probabilities sum to %g", c, p, s)
			}
			const trials = 40000
			fx, nf, _, mr := simulate(pl, p, trials, rng)
			se := func(q float64) float64 { return 4*math.Sqrt(q*(1-q)/trials) + 1e-3 }
			if math.Abs(fx-oc.PFixed) > se(oc.PFixed) || math.Abs(nf-oc.PNotFixed) > se(oc.PNotFixed) {
				t.Errorf("%+v p=%g: exact fixed/notfixed %.4f/%.4f, simulated %.4f/%.4f", c, p, oc.PFixed, oc.PNotFixed, fx, nf)
			}
			if math.Abs(mr-oc.MeanRuns) > 0.05*oc.MeanRuns {
				t.Errorf("%+v p=%g: exact mean runs %.1f, simulated %.1f", c, p, oc.MeanRuns, mr)
			}
		}
	}
}

// The promise printed to users: wrong verdicts happen at most Risk of the time
// at either hypothesis, including the automatic cap.
func TestErrorRatesWithinRisk(t *testing.T) {
	for _, p0 := range []float64{0.002, 0.01, 0.02, 0.05, 0.1, 0.2, 0.5} {
		for _, better := range []float64{2, 5, 10, 100} {
			for _, risk := range []float64{0.01, 0.05, 0.1} {
				pl, err := NewPlan(p0, better, risk, 0)
				if err != nil {
					t.Fatal(err)
				}
				a, b := pl.Exact(pl.P0), pl.Exact(pl.P1)
				if a.PFixed > risk || b.PNotFixed > risk {
					t.Errorf("p0=%g better=%g risk=%g: wrong-verdict rates %.4f / %.4f", p0, better, risk, a.PFixed, b.PNotFixed)
				}
				if a.PUndecided > 0.01 || b.PUndecided > 0.01 {
					t.Errorf("p0=%g better=%g risk=%g: undecided %.4f / %.4f at max %d", p0, better, risk, a.PUndecided, b.PUndecided, pl.Max)
				}
			}
		}
	}
}

// With no failures at all, the test must say FIXED exactly when Wald's bound is reached.
func TestCleanRunLength(t *testing.T) {
	pl, _ := NewPlan(0.05, 10, 0.05, 0)
	n := 0
	for pl.Decide(n+1, 0) == Continue {
		n++
	}
	n++
	want := int(math.Ceil(math.Log(19) / math.Log(0.995/0.95)))
	// Calibration may add a run or two to Wald's count, never remove one.
	if pl.Decide(n, 0) != Fixed || n < want || n > want+3 {
		t.Errorf("clean run decided at %d (%v), want FIXED at %d to %d", n, pl.Decide(n, 0), want, want+3)
	}
}

func binomTail(n, f int, p float64) float64 { // P(X >= f), X ~ Bin(n, p)
	s := 0.0
	for k := f; k <= n; k++ {
		lc, _ := math.Lgamma(float64(n + 1))
		l1, _ := math.Lgamma(float64(k + 1))
		l2, _ := math.Lgamma(float64(n - k + 1))
		s += math.Exp(lc - l1 - l2 + float64(k)*math.Log(p) + float64(n-k)*math.Log1p(-p))
	}
	return s
}

func TestIntervalAgainstBinomialSums(t *testing.T) {
	for _, c := range [][2]int{{1, 10}, {13, 300}, {4, 113}, {0, 59}, {59, 59}, {30, 60}} {
		f, n := c[0], c[1]
		lo, hi := Interval(f, n, 0.95)
		// Clopper-Pearson: P(X >= f | lo) = 2.5% and P(X <= f | hi) = 2.5%.
		if f > 0 {
			if g := binomTail(n, f, lo); math.Abs(g-0.025) > 1e-6 {
				t.Errorf("%d/%d: P(X>=f | lo=%g) = %g", f, n, lo, g)
			}
		} else if lo != 0 {
			t.Errorf("%d/%d: lo = %g, want 0", f, n, lo)
		}
		if f < n {
			if g := 1 - binomTail(n, f+1, hi); math.Abs(g-0.025) > 1e-6 {
				t.Errorf("%d/%d: P(X<=f | hi=%g) = %g", f, n, hi, g)
			}
		} else if hi != 1 {
			t.Errorf("%d/%d: hi = %g, want 1", f, n, hi)
		}
	}
	// No failures in 59 runs: one-sided 95% bound is 1 - 0.05^(1/59) = 4.95%.
	if u, want := UpperBound(0, 59, 0.95), 1-math.Pow(0.05, 1.0/59); math.Abs(u-want) > 1e-9 {
		t.Errorf("UpperBound(0, 59) = %g, want %g", u, want)
	}
}

func TestParseRate(t *testing.T) {
	for in, want := range map[string]float64{"5%": 0.05, "0.05": 0.05, "1/20": 0.05, " 2.5 % ": 0.025, "3 / 50": 0.06} {
		if got, err := parseRate(in); err != nil || math.Abs(got-want) > 1e-12 {
			t.Errorf("parseRate(%q) = %g, %v; want %g", in, got, err, want)
		}
	}
	for _, in := range []string{"", "5", "0", "100%", "1/0", "abc", "-1%"} {
		if _, err := parseRate(in); err == nil {
			t.Errorf("parseRate(%q): want error", in)
		}
	}
}

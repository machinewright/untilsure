package main

import (
	"fmt"
	"math"
)

// Plan is a truncated Wald sequential probability ratio test between
// "still broken" (each run fails with probability P0) and
// "fixed" (each run fails with probability P1 = P0/Better).
type Plan struct {
	P0, P1 float64 // failure rates under "still broken" and "fixed"
	Risk   float64 // nominal error rate each way
	Max    int     // stop undecided after this many runs
	lp, lf float64 // log-likelihood ratio added per pass / per failure
	upper  float64 // accept "fixed" at or above
	lower  float64 // accept "not fixed" at or below
}

type Verdict int

const (
	Continue Verdict = iota // no verdict yet
	Undecided
	Fixed
	NotFixed
)

func (v Verdict) String() string {
	return [...]string{"(running)", "UNDECIDED", "FIXED", "NOT FIXED"}[v]
}

// NewPlan builds the test. max <= 0 picks a cap automatically (see AutoMax).
func NewPlan(p0, better, risk float64, max int) (*Plan, error) {
	if !(p0 > 0 && p0 < 1) {
		return nil, fmt.Errorf("--was must be a failure rate between 0 and 1 (got %g)", p0)
	}
	if !(better > 1) || math.IsInf(better, 0) {
		return nil, fmt.Errorf("--better must be a number above 1 (got %g)", better)
	}
	if !(risk > 0 && risk < 0.5) {
		return nil, fmt.Errorf("--risk must be between 0 and 0.5 (got %g)", risk)
	}
	p1 := p0 / better
	pl := &Plan{P0: p0, P1: p1, Risk: risk,
		lp:    math.Log((1 - p1) / (1 - p0)),
		lf:    math.Log(p1 / p0),
		upper: math.Log((1 - risk) / risk),
		lower: math.Log(risk / (1 - risk)),
	}
	// Refuse plans no one could run: Wald's approximation of the expected
	// length when fixed is upper/lp-ish; a clean run alone needs upper/lp runs.
	if clean := pl.upper / pl.lp; clean > 1e5 {
		return nil, fmt.Errorf("this check would need over %.0f runs (about %.1g clean runs in a row to say FIXED): "+
			"the old failure rate is too low or --better too close to 1 to test this way", clean, clean)
	}
	pl.Max = uncapped
	pl.calibrate()
	if max <= 0 {
		pl.Max = pl.AutoMax()
	} else {
		pl.Max = max
	}
	pl.calibrate()
	return pl, nil
}

const uncapped = 1 << 30

// MinRunsToFix is the smallest number of runs after which FIXED is possible
// (all of them passing), or 0 if FIXED can't be reached within Max.
func (pl *Plan) MinRunsToFix() int {
	n := int(math.Ceil((pl.upper - 1e-9) / pl.lp))
	if n < 1 {
		n = 1
	}
	for n > 1 && pl.Decide(n-1, 0) == Fixed {
		n--
	}
	for pl.Decide(n, 0) != Fixed {
		n++
	}
	if n > pl.Max {
		return 0
	}
	return n
}

// calibrate moves Wald's thresholds outwards, if needed, until the exact
// error rates (with the cap) are within Risk. Wald's thresholds are only
// approximately right: for runs with rare failures the "wrongly FIXED"
// rate can come out a little above the nominal risk.
func (pl *Plan) calibrate() {
	for round := 0; round < 3; round++ {
		changed := false
		if pl.Exact(pl.P0).PFixed > pl.Risk {
			lo, hi := pl.upper, pl.upper+1
			for pl.upper = hi; pl.Exact(pl.P0).PFixed > pl.Risk; pl.upper = hi {
				lo, hi = hi, hi+1
			}
			for i := 0; i < 40; i++ { // smallest threshold that works
				pl.upper = (lo + hi) / 2
				if pl.Exact(pl.P0).PFixed > pl.Risk {
					lo = pl.upper
				} else {
					hi = pl.upper
				}
			}
			pl.upper, changed = hi, true
		}
		if pl.Exact(pl.P1).PNotFixed > pl.Risk {
			lo, hi := pl.lower, pl.lower-1
			for pl.lower = hi; pl.Exact(pl.P1).PNotFixed > pl.Risk; pl.lower = hi {
				lo, hi = hi, hi-1
			}
			for i := 0; i < 40; i++ {
				pl.lower = (lo + hi) / 2
				if pl.Exact(pl.P1).PNotFixed > pl.Risk {
					lo = pl.lower
				} else {
					hi = pl.lower
				}
			}
			pl.lower, changed = hi, true
		}
		if !changed {
			return
		}
	}
}

// llr is the log-likelihood ratio (fixed vs still broken) after n runs with f failures.
func (pl *Plan) llr(n, f int) float64 { return float64(n-f)*pl.lp + float64(f)*pl.lf }

// Decide returns the verdict after n runs with f failures (n counted in start order).
func (pl *Plan) Decide(n, f int) Verdict {
	const eps = 1e-9 // so that exact boundary hits aren't lost to rounding
	l := pl.llr(n, f)
	switch {
	case l >= pl.upper-eps:
		return Fixed
	case l <= pl.lower+eps:
		return NotFixed
	case n >= pl.Max:
		return Undecided
	}
	return Continue
}

// OC is the exact operating characteristic of the plan at one true failure rate.
type OC struct {
	PFixed, PNotFixed, PUndecided float64
	MeanRuns                      float64
}

// Exact computes, by dynamic programming over (runs, failures), the probability
// of each verdict and the expected number of runs when every run fails
// independently with probability p.
func (pl *Plan) Exact(p float64) OC {
	var oc OC
	// alive[f-lo] = probability of being undecided with f failures. For a given
	// number of runs the undecided failure counts form a contiguous range.
	alive, lo := []float64{1}, 0
	mass := 1.0
	for n := 0; n < pl.Max && mass > 1e-14; n++ {
		oc.MeanRuns += mass
		next := make([]float64, len(alive)+1) // failures lo .. lo+len(alive)
		for k, pr := range alive {
			if pr == 0 {
				continue
			}
			f := lo + k
			for df, q := range [2]float64{pr * (1 - p), pr * p} {
				if q == 0 {
					continue
				}
				switch pl.Decide(n+1, f+df) {
				case Fixed:
					oc.PFixed += q
				case NotFixed:
					oc.PNotFixed += q
				case Undecided:
					oc.PUndecided += q
				default:
					next[k+df] += q
				}
			}
		}
		// trim zeros at both ends
		a, b := 0, len(next)
		for a < b && next[a] == 0 {
			a++
		}
		for b > a && next[b-1] == 0 {
			b--
		}
		alive, lo = next[a:b], lo+a
		mass = 0
		for _, x := range alive {
			mass += x
		}
	}
	return oc
}

// AutoMax caps the test at 10x the longer of its expected lengths under the two
// hypotheses (computed without a cap), rounded up to a multiple of 10.
func (pl *Plan) AutoMax() int {
	saved := pl.Max
	pl.Max = uncapped
	e := math.Max(pl.Exact(pl.P0).MeanRuns, pl.Exact(pl.P1).MeanRuns)
	pl.Max = saved
	return int(math.Ceil(e)*10+9) / 10 * 10
}

// --- binomial confidence interval (Clopper-Pearson) ---

// betaInc is the regularized incomplete beta function I_x(a, b).
func betaInc(a, b, x float64) float64 {
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 1
	}
	la, _ := math.Lgamma(a + b)
	lb, _ := math.Lgamma(a)
	lc, _ := math.Lgamma(b)
	front := math.Exp(la - lb - lc + a*math.Log(x) + b*math.Log(1-x))
	if x < (a+1)/(a+b+2) {
		return front * betaCF(a, b, x) / a
	}
	return 1 - front*betaCF(b, a, 1-x)/b
}

// betaCF evaluates the continued fraction for betaInc (modified Lentz).
func betaCF(a, b, x float64) float64 {
	const tiny = 1e-300
	c, d := 1.0, 1-(a+b)*x/(a+1)
	if math.Abs(d) < tiny {
		d = tiny
	}
	d = 1 / d
	h := d
	for m := 1; m <= 500; m++ {
		fm := float64(m)
		for _, aa := range [2]float64{
			fm * (b - fm) * x / ((a + 2*fm - 1) * (a + 2*fm)),
			-(a + fm) * (a + b + fm) * x / ((a + 2*fm) * (a + 2*fm + 1)),
		} {
			d = 1 + aa*d
			if math.Abs(d) < tiny {
				d = tiny
			}
			c = 1 + aa/c
			if math.Abs(c) < tiny {
				c = tiny
			}
			d = 1 / d
			h *= d * c
		}
		if math.Abs(d*c-1) < 1e-15 {
			break
		}
	}
	return h
}

// betaQuantile returns x with I_x(a, b) = q, by bisection.
func betaQuantile(q, a, b float64) float64 {
	lo, hi := 0.0, 1.0
	for i := 0; i < 100; i++ {
		mid := (lo + hi) / 2
		if betaInc(a, b, mid) < q {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

// Interval is the exact (Clopper-Pearson) two-sided interval for a failure
// rate after f failures in n runs, at the given confidence.
func Interval(f, n int, conf float64) (lo, hi float64) {
	a := (1 - conf) / 2
	if f > 0 {
		lo = betaQuantile(a, float64(f), float64(n-f+1))
	}
	hi = 1
	if f < n {
		hi = betaQuantile(1-a, float64(f+1), float64(n-f))
	}
	return
}

// UpperBound is the exact one-sided upper confidence bound on the failure
// rate after f failures in n runs (with no failures at 95%: about 3/n).
func UpperBound(f, n int, conf float64) float64 {
	if f >= n {
		return 1
	}
	return betaQuantile(conf, float64(f+1), float64(n-f))
}

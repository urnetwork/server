package main

// small numerical statistics kernel for the run comparison tooling: streaming
// moments, quantiles (via deterministic quickselect, so large row sets are
// cheap to bootstrap), the Student-t survival function (via the regularized
// incomplete beta continued fraction), its inverse, and Holm's step-down
// adjustment. No external numerics dependency; everything is deterministic.

import (
	"math"
	"sort"
)

// quantile returns the q-quantile (0..1) of values with linear interpolation
// between order statistics (the R-7 / numpy default definition). values is
// reordered in place (quickselect); pass a scratch copy if order matters.
func quantile(values []float64, q float64) float64 {
	n := len(values)
	if n == 0 {
		return math.NaN()
	}
	if n == 1 {
		return values[0]
	}
	if q <= 0 {
		return selectKth(values, 0)
	}
	if 1 <= q {
		return selectKth(values, n-1)
	}
	h := q * float64(n-1)
	k := int(math.Floor(h))
	frac := h - float64(k)
	lower := selectKth(values, k)
	if frac == 0 {
		return lower
	}
	// after selectKth(k), values[k+1:] all >= values[k]; the (k+1)-th order
	// statistic is their minimum
	upper := values[k+1]
	for _, v := range values[k+2:] {
		if v < upper {
			upper = v
		}
	}
	return lower + frac*(upper-lower)
}

// selectKth partially sorts values so values[k] holds the k-th order
// statistic (0-indexed) and returns it. Median-of-three pivots keep it
// deterministic and fast on sorted/reversed inputs.
func selectKth(values []float64, k int) float64 {
	lo, hi := 0, len(values)-1
	for lo < hi {
		// fall back to a full sort on tiny ranges
		if hi-lo < 12 {
			sub := values[lo : hi+1]
			sort.Float64s(sub)
			return values[k]
		}
		pivot := medianOfThree(values[lo], values[(lo+hi)/2], values[hi])
		i, j := lo, hi
		for i <= j {
			for values[i] < pivot {
				i += 1
			}
			for pivot < values[j] {
				j -= 1
			}
			if i <= j {
				values[i], values[j] = values[j], values[i]
				i += 1
				j -= 1
			}
		}
		if k <= j {
			hi = j
		} else if i <= k {
			lo = i
		} else {
			return values[k]
		}
	}
	return values[k]
}

func medianOfThree(a float64, b float64, c float64) float64 {
	if a > b {
		a, b = b, a
	}
	if b > c {
		b = c
	}
	if a > b {
		b = a
	}
	return b
}

// meanStd returns the sample mean and the sample (n-1) standard deviation.
func meanStd(values []float64) (float64, float64) {
	n := len(values)
	if n == 0 {
		return math.NaN(), math.NaN()
	}
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	mean := sum / float64(n)
	if n < 2 {
		return mean, math.NaN()
	}
	ss := 0.0
	for _, v := range values {
		d := v - mean
		ss += d * d
	}
	return mean, math.Sqrt(ss / float64(n-1))
}

// studentTSf is the Student-t survival function P(T_df >= t): the one-sided
// p-value of an observed t statistic.
func studentTSf(t float64, df float64) float64 {
	if math.IsNaN(t) || math.IsNaN(df) || df <= 0 {
		return math.NaN()
	}
	if math.IsInf(t, 1) {
		return 0
	}
	if math.IsInf(t, -1) {
		return 1
	}
	// P(T >= t) = I_{df/(df+t^2)}(df/2, 1/2) / 2 for t >= 0, by symmetry below
	x := df / (df + t*t)
	p := 0.5 * regIncBeta(df/2, 0.5, x)
	if t < 0 {
		return 1 - p
	}
	return p
}

// studentTCrit returns the one-sided critical value t with
// P(T_df >= t) = alpha, via bisection on the monotonic survival function.
func studentTCrit(alpha float64, df float64) float64 {
	if alpha <= 0 || 1 <= alpha || df <= 0 {
		return math.NaN()
	}
	if alpha == 0.5 {
		return 0
	}
	lo, hi := -1e6, 1e6
	for i := 0; i < 200; i += 1 {
		mid := (lo + hi) / 2
		if studentTSf(mid, df) > alpha {
			lo = mid
		} else {
			hi = mid
		}
		if hi-lo < 1e-10 {
			break
		}
	}
	return (lo + hi) / 2
}

// regIncBeta is the regularized incomplete beta function I_x(a, b), computed
// with the continued fraction expansion (converges quickly on the side where
// x < (a+1)/(a+b+2); the complement identity covers the other side).
func regIncBeta(a float64, b float64, x float64) float64 {
	if x <= 0 {
		return 0
	}
	if 1 <= x {
		return 1
	}
	lgab, _ := math.Lgamma(a + b)
	lga, _ := math.Lgamma(a)
	lgb, _ := math.Lgamma(b)
	front := math.Exp(lgab - lga - lgb + a*math.Log(x) + b*math.Log1p(-x))
	if x < (a+1)/(a+b+2) {
		return front * betaContinuedFraction(a, b, x) / a
	}
	return 1 - front*betaContinuedFraction(b, a, 1-x)/b
}

// betaContinuedFraction evaluates the incomplete beta continued fraction with
// the modified Lentz method.
func betaContinuedFraction(a float64, b float64, x float64) float64 {
	const maxIterations = 500
	const epsilon = 1e-14
	const tiny = 1e-300

	qab := a + b
	qap := a + 1
	qam := a - 1
	c := 1.0
	d := 1 - qab*x/qap
	if math.Abs(d) < tiny {
		d = tiny
	}
	d = 1 / d
	h := d
	for m := 1; m <= maxIterations; m += 1 {
		fm := float64(m)
		m2 := 2 * fm
		// even step
		numerator := fm * (b - fm) * x / ((qam + m2) * (a + m2))
		d = 1 + numerator*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + numerator/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		h *= d * c
		// odd step
		numerator = -(a + fm) * (qab + fm) * x / ((a + m2) * (qap + m2))
		d = 1 + numerator*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + numerator/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		delta := d * c
		h *= delta
		if math.Abs(delta-1) < epsilon {
			break
		}
	}
	return h
}

// holmAdjust returns Holm step-down adjusted p-values (same order as the
// input): control of the familywise error rate at any level without the
// uniform Bonferroni penalty. NaN inputs stay NaN and do not count toward
// the family size.
func holmAdjust(pValues []float64) []float64 {
	type indexed struct {
		index int
		p     float64
	}
	testable := []indexed{}
	for i, p := range pValues {
		if !math.IsNaN(p) {
			testable = append(testable, indexed{index: i, p: p})
		}
	}
	sort.Slice(testable, func(i int, j int) bool {
		return testable[i].p < testable[j].p
	})

	adjusted := make([]float64, len(pValues))
	for i := range adjusted {
		adjusted[i] = math.NaN()
	}
	m := len(testable)
	running := 0.0
	for rank, entry := range testable {
		adj := float64(m-rank) * entry.p
		if adj > 1 {
			adj = 1
		}
		// enforce monotonicity in the step-down order
		if adj < running {
			adj = running
		}
		running = adj
		adjusted[entry.index] = adj
	}
	return adjusted
}

// welch returns the Welch two-sample t statistic and Welch–Satterthwaite
// degrees of freedom for run-level samples a and b (each needs n >= 2).
func welch(a []float64, b []float64) (float64, float64, bool) {
	if len(a) < 2 || len(b) < 2 {
		return math.NaN(), math.NaN(), false
	}
	meanA, sdA := meanStd(a)
	meanB, sdB := meanStd(b)
	va := sdA * sdA / float64(len(a))
	vb := sdB * sdB / float64(len(b))
	if va+vb <= 0 {
		return math.NaN(), math.NaN(), false
	}
	t := (meanA - meanB) / math.Sqrt(va+vb)
	df := (va + vb) * (va + vb) / (va*va/float64(len(a)-1) + vb*vb/float64(len(b)-1))
	return t, df, true
}

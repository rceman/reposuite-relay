// Statistics for benchmark sample distributions.
package main

import (
	"math"
	"sort"
)

// Stats is a deterministic distribution summary. Percentiles use linear
// interpolation between closest ranks (the numpy "linear" method):
// rank r = p/100*(n-1), value = s[floor(r)] + frac*(s[ceil(r)]-s[floor(r)]).
type Stats struct {
	N      int     `json:"n"`
	Min    float64 `json:"min"`
	P50    float64 `json:"p50"`
	P90    float64 `json:"p90"`
	P95    float64 `json:"p95"`
	Mean   float64 `json:"mean"`
	Max    float64 `json:"max"`
	StdDev float64 `json:"stddev"`
}

func summarize(vals []float64) Stats {
	if len(vals) == 0 {
		return Stats{}
	}
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	st := Stats{
		N:   len(s),
		Min: s[0],
		Max: s[len(s)-1],
	}
	var sum float64
	for _, v := range s {
		sum += v
	}
	st.Mean = sum / float64(len(s))
	var sq float64
	for _, v := range s {
		d := v - st.Mean
		sq += d * d
	}
	st.StdDev = math.Sqrt(sq / float64(len(s)))
	st.P50 = percentile(s, 50)
	st.P90 = percentile(s, 90)
	st.P95 = percentile(s, 95)
	return st
}

func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	r := p / 100 * float64(n-1)
	lo := int(math.Floor(r))
	hi := int(math.Ceil(r))
	if lo == hi {
		return sorted[lo]
	}
	frac := r - float64(lo)
	return sorted[lo] + frac*(sorted[hi]-sorted[lo])
}

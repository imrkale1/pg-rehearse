package load

import (
	"math"
	"slices"
	"time"

	"github.com/pg-rehearse/pg-rehearse/internal/model"
)

// Window is a closed time interval.
type Window struct {
	Start, End time.Time
}

// Duration returns End - Start.
func (w Window) Duration() time.Duration { return w.End.Sub(w.Start) }

// StallWindows returns merged intervals during which requests of queryID were
// stuck: every sample whose latency exceeded threshold contributes
// [Start, Start+Latency], and overlapping/touching intervals are merged.
// Samples need not be sorted. Results are ordered by Start.
func StallWindows(samples []model.Sample, queryID int64, threshold time.Duration) []Window {
	var iv []Window
	for i := range samples {
		s := &samples[i]
		if s.QueryID == queryID && s.Latency > threshold {
			iv = append(iv, Window{Start: s.Start, End: s.Start.Add(s.Latency)})
		}
	}
	if len(iv) == 0 {
		return nil
	}
	slices.SortFunc(iv, func(a, b Window) int { return a.Start.Compare(b.Start) })
	out := iv[:1]
	for _, w := range iv[1:] {
		last := &out[len(out)-1]
		if !w.Start.After(last.End) {
			if w.End.After(last.End) {
				last.End = w.End
			}
			continue
		}
		out = append(out, w)
	}
	return slices.Clip(out)
}

// LongestStall returns the longest window from StallWindows (zero if none).
func LongestStall(samples []model.Sample, queryID int64, threshold time.Duration) time.Duration {
	var best time.Duration
	for _, w := range StallWindows(samples, queryID, threshold) {
		best = max(best, w.Duration())
	}
	return best
}

// computeStats summarises one query's samples. buf is reused scratch space.
func computeStats(qp model.QueryProfile, samps []model.Sample, buf []time.Duration) (model.QueryStats, []time.Duration) {
	st := model.QueryStats{QueryID: qp.QueryID, Query: qp.Query, Feature: qp.Feature, Count: len(samps)}
	buf = buf[:0]
	for i := range samps {
		if samps[i].Err != "" {
			st.Errors++
		}
		buf = append(buf, samps[i].Latency)
	}
	if len(buf) == 0 {
		return st, buf
	}
	slices.Sort(buf)
	st.P50Ms = ms(percentile(buf, 0.50))
	st.P95Ms = ms(percentile(buf, 0.95))
	st.P99Ms = ms(percentile(buf, 0.99))
	st.MaxMs = ms(buf[len(buf)-1])
	return st, buf
}

// percentile uses the nearest-rank method on sorted data.
func percentile(sorted []time.Duration, p float64) time.Duration {
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	idx = min(max(idx, 0), len(sorted)-1)
	return sorted[idx]
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

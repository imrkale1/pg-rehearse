package main

import (
	"testing"
	"time"

	"github.com/pg-rehearse/pg-rehearse/internal/model"
)

func TestStallExceeded(t *testing.T) {
	cases := []struct {
		stallMs float64
		limit   time.Duration
		want    bool
	}{
		{6500, 0, false},
		{6500, -time.Second, false},
		{6500, 500 * time.Millisecond, true},
		{500, 500 * time.Millisecond, false},
		{500.5, 500 * time.Millisecond, true},
		{100, time.Second, false},
		{0, time.Millisecond, false},
	}
	for _, c := range cases {
		if got := stallExceeded(c.stallMs, c.limit); got != c.want {
			t.Errorf("stallExceeded(%v, %v) = %v, want %v", c.stallMs, c.limit, got, c.want)
		}
	}
}

func TestFailed(t *testing.T) {
	cases := []struct {
		v      model.Verdict
		failOn string
		want   bool
	}{
		{model.VerdictDangerous, "never", false},
		{model.VerdictRisky, "risky", true},
		{model.VerdictSafe, "risky", false},
		{model.VerdictRisky, "dangerous", false},
		{model.VerdictDangerous, "dangerous", true},
	}
	for _, c := range cases {
		if got := failed(c.v, c.failOn); got != c.want {
			t.Errorf("failed(%v, %q) = %v, want %v", c.v, c.failOn, got, c.want)
		}
	}
}

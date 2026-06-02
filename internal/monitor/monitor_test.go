package monitor

import (
	"testing"
	"time"
)

func TestKesLifetimePrime(t *testing.T) {
	n := NetworkParams{MaxKesEvolutions: 60, SlotsPerKesPeriod: 129600, SlotLength: 1}
	// (60-1) * 129600 * 1 seconds = 7,646,400s = 88.5 days.
	got := n.KesLifetime()
	want := 88.5 * 24 * float64(time.Hour)
	if got != time.Duration(want) {
		t.Fatalf("KesLifetime = %s, want %s", got, time.Duration(want))
	}
}

func TestEpochSecondsPrime(t *testing.T) {
	n := NetworkParams{EpochLength: 432000, SlotLength: 1}
	if got := n.EpochSeconds(); got != 432000 {
		t.Fatalf("EpochSeconds = %v, want 432000", got)
	}
}

func TestTierLevel(t *testing.T) {
	thresholds := []int{14, 7, 2}
	cases := []struct {
		days float64
		want int
	}{
		{20, 0},
		{14, 1},
		{10, 1},
		{7, 2},
		{3, 2},
		{2, 3},
		{0.5, 3},
		{-1, 3}, // already past estimated expiry
	}
	for _, c := range cases {
		if got := tierLevel(c.days, thresholds); got != c.want {
			t.Errorf("tierLevel(%v) = %d, want %d", c.days, got, c.want)
		}
	}
}

func TestFmtDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Hour, "30h"},
		{47 * time.Hour, "47h"},
		{48 * time.Hour, "2.0d"},
		{60 * time.Hour, "2.5d"},
	}
	for _, c := range cases {
		if got := fmtDuration(c.d); got != c.want {
			t.Errorf("fmtDuration(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

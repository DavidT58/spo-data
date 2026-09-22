package telegram

import (
	"testing"
	"time"
)

func TestQuietHoursBoundaries(t *testing.T) {
	q, err := NewQuietHours("00:00", "08:00", "Europe/Belgrade")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		utc  string
		want bool
	}{
		{"before midnight", "2026-09-21T21:59:59Z", false},
		{"midnight", "2026-09-21T22:00:00Z", true},
		{"before eight", "2026-09-22T05:59:59Z", true},
		{"eight", "2026-09-22T06:00:00Z", false},
		{"winter midnight", "2026-12-21T23:00:00Z", true},
		{"winter before eight", "2026-12-22T06:59:59Z", true},
		{"winter eight", "2026-12-22T07:00:00Z", false},
		{"spring before clock change", "2026-03-29T00:59:59Z", true},
		{"spring after clock change", "2026-03-29T01:00:00Z", true},
		{"spring eight", "2026-03-29T06:00:00Z", false},
		{"autumn first two thirty", "2026-10-25T00:30:00Z", true},
		{"autumn second two thirty", "2026-10-25T01:30:00Z", true},
		{"autumn eight", "2026-10-25T07:00:00Z", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, tc.utc)
			if err != nil {
				t.Fatal(err)
			}
			if got := q.Active(now); got != tc.want {
				t.Fatalf("Active(%s) = %t, want %t", tc.utc, got, tc.want)
			}
		})
	}
}

func TestQuietHoursAcrossMidnight(t *testing.T) {
	q, err := NewQuietHours("22:30", "07:15", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		clock string
		want  bool
	}{
		{"22:29:59", false},
		{"22:30:00", true},
		{"00:00:00", true},
		{"07:14:59", true},
		{"07:15:00", false},
	} {
		now, err := time.Parse("15:04:05", tc.clock)
		if err != nil {
			t.Fatal(err)
		}
		if got := q.Active(now); got != tc.want {
			t.Errorf("Active(%s) = %t, want %t", tc.clock, got, tc.want)
		}
	}
}

func TestQuietHoursInvalidConfiguration(t *testing.T) {
	for _, tc := range []struct {
		start, end, zone string
	}{
		{"00:00", "", "UTC"},
		{"", "08:00", "UTC"},
		{"24:00", "08:00", "UTC"},
		{"00:00", "08:60", "UTC"},
		{"00:00", "00:00", "UTC"},
		{"00:00", "08:00", "Not/AZone"},
	} {
		if _, err := NewQuietHours(tc.start, tc.end, tc.zone); err == nil {
			t.Errorf("accepted invalid quiet hours: %+v", tc)
		}
	}
}

package leaderlog

import (
	"os"
	"testing"
	"time"
)

// The fixture is the verbatim --out-file output of cardano-cli 9.4.1 on fr-0
// (PANDA, epoch 160). slotTime must round-trip as UTC and be consistent with
// slot arithmetic: slotTime = slot0Time + slot × 1s.
func TestParseScheduleJSONFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/leadership-schedule-9.4.1.json")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := ParseScheduleJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 26 {
		t.Fatalf("want 26 entries, got %d", len(entries))
	}
	first := entries[0]
	if first.Slot != 69166221 {
		t.Errorf("first slot = %d, want 69166221", first.Slot)
	}
	wantTime := time.Date(2026, 8, 1, 18, 30, 21, 0, time.UTC)
	if !first.SlotTime.Equal(wantTime) {
		t.Errorf("first slotTime = %s, want %s", first.SlotTime, wantTime)
	}
	if first.SlotTime.Location() != time.UTC {
		t.Errorf("slotTime not normalized to UTC: %v", first.SlotTime.Location())
	}

	// Cross-check every entry against slot arithmetic using the first entry as
	// the anchor (Prime: 1s slots).
	anchor := first.SlotTime.Unix() - first.Slot
	for _, e := range entries {
		if e.SlotTime.Unix()-e.Slot != anchor {
			t.Errorf("slot %d: time %s inconsistent with slot arithmetic", e.Slot, e.SlotTime)
		}
	}
}

func TestParseScheduleJSONVariants(t *testing.T) {
	for name, data := range map[string]string{
		"snake_case": `[{"slot_number": 100, "slot_time": "2026-08-01T00:00:00Z"}]`,
		"slotNo":     `[{"slotNo": 100, "slotTime": "2026-08-01T00:00:00Z"}]`,
	} {
		entries, err := ParseScheduleJSON([]byte(data))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(entries) != 1 || entries[0].Slot != 100 {
			t.Errorf("%s: unexpected result %+v", name, entries)
		}
	}
}

func TestParseScheduleJSONEmpty(t *testing.T) {
	entries, err := ParseScheduleJSON([]byte(`[]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("want 0 entries, got %d", len(entries))
	}
}

func TestParseScheduleText(t *testing.T) {
	table := `     SlotNo                          UTC Time
-------------------------------------------------------------
     69166221                   2026-08-01 18:30:21.000000 UTC
     69202299                   2026-08-02 04:31:39.000000 UTC
`
	entries, err := ParseScheduleText([]byte(table))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	if entries[0].Slot != 69166221 {
		t.Errorf("first slot = %d", entries[0].Slot)
	}
	want := time.Date(2026, 8, 1, 18, 30, 21, 0, time.UTC)
	if !entries[0].SlotTime.Equal(want) {
		t.Errorf("first time = %s, want %s", entries[0].SlotTime, want)
	}
}

func TestParseScheduleTextGarbage(t *testing.T) {
	if _, err := ParseScheduleText([]byte("something unexpected\nno rows here\n")); err == nil {
		t.Fatal("want error on unrecognized non-empty output")
	}
}

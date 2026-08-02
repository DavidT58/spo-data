package leaderlog

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ScheduleEntry is one leadership assignment as reported by cardano-cli.
type ScheduleEntry struct {
	Slot     int64
	SlotTime time.Time
}

// rawEntry tolerates the field-name variants seen across cardano-cli versions.
type rawEntry struct {
	SlotNumber  *int64 `json:"slotNumber"`
	SlotNo      *int64 `json:"slotNo"`
	SlotNumber2 *int64 `json:"slot_number"`
	SlotTime    string `json:"slotTime"`
	SlotTime2   string `json:"slot_time"`
}

// ParseScheduleJSON parses the --out-file JSON produced by
// `query leadership-schedule` (verified format on cardano-cli 9.4.1:
// [{"slotNumber": N, "slotTime": "2026-08-01T18:30:21Z"}]).
func ParseScheduleJSON(data []byte) ([]ScheduleEntry, error) {
	var raw []rawEntry
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse leadership-schedule JSON: %w", err)
	}
	out := make([]ScheduleEntry, 0, len(raw))
	for i, r := range raw {
		var slot int64
		switch {
		case r.SlotNumber != nil:
			slot = *r.SlotNumber
		case r.SlotNo != nil:
			slot = *r.SlotNo
		case r.SlotNumber2 != nil:
			slot = *r.SlotNumber2
		default:
			return nil, fmt.Errorf("leadership-schedule entry %d has no recognizable slot field", i)
		}
		ts := r.SlotTime
		if ts == "" {
			ts = r.SlotTime2
		}
		t, err := parseSlotTime(ts)
		if err != nil {
			return nil, fmt.Errorf("leadership-schedule entry %d: %w", i, err)
		}
		out = append(out, ScheduleEntry{Slot: slot, SlotTime: t})
	}
	return out, nil
}

// scheduleLine matches the text-table fallback format, e.g.:
//	     SlotNo                          UTC Time
//	-------------------------------------------------------------
//	     69166221                   2026-08-01 18:30:21.000000 UTC
var scheduleLine = regexp.MustCompile(`^\s*(\d+)\s+(\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}\S*(?:\s+\S+)?)\s*$`)

// ParseScheduleText parses the stdout table older cli versions print when
// --out-file is unsupported.
func ParseScheduleText(data []byte) ([]ScheduleEntry, error) {
	var out []ScheduleEntry
	for _, line := range strings.Split(string(data), "\n") {
		m := scheduleLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		slot, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			continue
		}
		t, err := parseSlotTime(strings.TrimSpace(m[2]))
		if err != nil {
			return nil, fmt.Errorf("slot %d: %w", slot, err)
		}
		out = append(out, ScheduleEntry{Slot: slot, SlotTime: t})
	}
	if len(out) == 0 && len(strings.TrimSpace(string(data))) > 0 {
		// Non-empty output with zero parsed rows usually means a format change.
		return nil, fmt.Errorf("no schedule rows recognized in cli text output (%d bytes)", len(data))
	}
	return out, nil
}

var slotTimeLayouts = []string{
	time.RFC3339,
	"2006-01-02 15:04:05.999999999 MST",
	"2006-01-02 15:04:05 MST",
	"2006-01-02 15:04:05",
}

func parseSlotTime(s string) (time.Time, error) {
	for _, layout := range slotTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized slot time %q", s)
}

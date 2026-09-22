package telegram

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrQuietHours means no request was sent. The caller must not mark the alert
// as delivered; it can assess the condition again on a later poll.
var ErrQuietHours = errors.New("telegram delivery paused during quiet hours")

// QuietHours is a daily, start-inclusive and end-exclusive local-time window.
// A nil *QuietHours disables the window.
type QuietHours struct {
	start    int // minutes since midnight
	end      int
	location *time.Location
}

// NewQuietHours parses HH:MM boundaries and an IANA time zone. Both boundaries
// empty disables quiet hours. An empty zone uses the system's local time zone.
func NewQuietHours(start, end, zone string) (*QuietHours, error) {
	start, end, zone = strings.TrimSpace(start), strings.TrimSpace(end), strings.TrimSpace(zone)
	if start == "" && end == "" {
		return nil, nil
	}
	s, err := time.Parse("15:04", start)
	if err != nil {
		return nil, fmt.Errorf("invalid quiet-hours start %q (want HH:MM): %w", start, err)
	}
	e, err := time.Parse("15:04", end)
	if err != nil {
		return nil, fmt.Errorf("invalid quiet-hours end %q (want HH:MM): %w", end, err)
	}
	if s.Equal(e) {
		return nil, fmt.Errorf("quiet-hours start and end must differ")
	}
	if zone == "" {
		zone = "Local"
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		return nil, fmt.Errorf("invalid quiet-hours time zone %q: %w", zone, err)
	}
	return &QuietHours{
		start:    s.Hour()*60 + s.Minute(),
		end:      e.Hour()*60 + e.Minute(),
		location: location,
	}, nil
}

// Active reports whether now falls inside the window, including across
// midnight and daylight-saving changes.
func (q *QuietHours) Active(now time.Time) bool {
	if q == nil {
		return false
	}
	local := now.In(q.location)
	minute := local.Hour()*60 + local.Minute()
	if q.start < q.end {
		return minute >= q.start && minute < q.end
	}
	return minute >= q.start || minute < q.end
}

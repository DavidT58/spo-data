package monitor

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"spo-data/internal/database"
	"spo-data/internal/models"
	"spo-data/internal/telegram"
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

func TestQuietHoursDeferAlerts(t *testing.T) {
	if err := database.Initialize(filepath.Join(t.TempDir(), "alerts.db")); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	// Center the window on now so the test never waits for a wall-clock boundary.
	quiet, err := telegram.NewQuietHours(now.Add(-time.Hour).Format("15:04"), now.Add(time.Hour).Format("15:04"), "UTC")
	if err != nil {
		t.Fatal(err)
	}

	for _, alertType := range []string{models.AlertTypeBlock, models.AlertTypeKES} {
		t.Run(alertType, func(t *testing.T) {
			messages := make(chan string, 16)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				messages <- r.Form.Get("text")
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"ok":true}`)
			}))
			defer server.Close()

			tg := telegram.NewClient("test-token", "test-chat")
			tg.BaseURL = server.URL
			m := &Monitor{
				tg:     tg,
				set:    Settings{ReminderInterval: time.Hour},
				logger: log.New(io.Discard, "", 0),
			}
			evaluate := func(pool string, level int) {
				if alertType == models.AlertTypeBlock {
					m.evaluateWithReminder(pool, "operator", "ticker", level > 0, "incident", "reminder", "recovery")
				} else {
					m.evaluateTiered(pool, "operator", "ticker", level, fmt.Sprintf("tier %d", level), "recovery")
				}
			}
			assertMessages := func(want ...string) {
				t.Helper()
				for _, text := range want {
					select {
					case got := <-messages:
						if got != text {
							t.Fatalf("message = %q, want %q", got, text)
						}
					default:
						t.Fatalf("missing message %q", text)
					}
				}
				select {
				case got := <-messages:
					t.Fatalf("unexpected message %q", got)
				default:
				}
			}

			tg.QuietHours = quiet
			evaluate("ongoing", 1)
			assertMessages()
			tg.QuietHours = nil
			evaluate("ongoing", 1)
			if alertType == models.AlertTypeBlock {
				assertMessages("incident")
			} else {
				assertMessages("tier 1")
			}

			state, found, err := database.GetAlertState("ongoing", alertType)
			if err != nil || !found {
				t.Fatalf("missing delivered alert state: found=%t err=%v", found, err)
			}
			state.LastReminderAt = now.Add(-2 * time.Hour)
			if err := database.SaveAlertState(&state); err != nil {
				t.Fatal(err)
			}
			tg.QuietHours = quiet
			evaluate("ongoing", 2)
			assertMessages()
			tg.QuietHours = nil
			evaluate("ongoing", 2)
			if alertType == models.AlertTypeBlock {
				assertMessages("reminder")
			} else {
				assertMessages("tier 2")
			}

			tg.QuietHours = quiet
			evaluate("ongoing", 0)
			assertMessages()
			tg.QuietHours = nil
			evaluate("ongoing", 0)
			assertMessages("recovery")
			evaluate("ongoing", 0)
			assertMessages()

			// An incident that starts and clears overnight needs no stale alert
			// or recovery for an incident the recipient never saw.
			tg.QuietHours = quiet
			evaluate("transient", 1)
			assertMessages()
			tg.QuietHours = nil
			evaluate("transient", 0)
			assertMessages()
		})
	}
}

package monitor

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"spo-data/internal/database"
	"spo-data/internal/models"
	"spo-data/internal/telegram"
	"strings"
	"sync"
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

// telegramStub is a Bot API stub that records every sendMessage text.
type telegramStub struct {
	mu       sync.Mutex
	requests int
	texts    []string
	failAt   int // 1-based request answered with an API error; 0 never fails
}

// take returns and forgets the texts received since the last call.
func (s *telegramStub) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	texts := s.texts
	s.texts = nil
	return texts
}

// newTestMonitor returns a Monitor on a fresh database whose Telegram client
// talks to a stub.
func newTestMonitor(t *testing.T) (*Monitor, *telegramStub) {
	t.Helper()
	if err := database.Initialize(filepath.Join(t.TempDir(), "alerts.db")); err != nil {
		t.Fatal(err)
	}
	stub := &telegramStub{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		stub.mu.Lock()
		stub.requests++
		fail := stub.requests == stub.failAt
		stub.texts = append(stub.texts, r.Form.Get("text"))
		stub.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if fail {
			_, _ = io.WriteString(w, `{"ok":false,"error_code":500,"description":"stub failure"}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(server.Close)

	tg := telegram.NewClient("test-token", "test-chat")
	tg.BaseURL = server.URL
	return &Monitor{
		tg:     tg,
		set:    Settings{ReminderInterval: time.Hour},
		logger: log.New(io.Discard, "", 0),
	}, stub
}

// evaluate feeds one pool listing into b through the alert type's state
// machine, with text as its alert/reminder message.
func evaluate(m *Monitor, b *alertBatch, alertType, poolID, operator string, level int, text string) {
	if alertType == models.AlertTypeBlock {
		m.evaluateWithReminder(b, poolID, operator, "ticker", level > 0, text, text, "recovery")
	} else {
		m.evaluateTiered(b, poolID, operator, "ticker", level, text, "recovery")
	}
}

// batchTexts returns the alert texts of a message that carries several alerts.
func batchTexts(msg string) []string {
	return strings.Split(msg, alertSep)[1:]
}

func TestQuietHoursDeferAlerts(t *testing.T) {
	now := time.Now().UTC()
	// Center the window on now so the test never waits for a wall-clock boundary.
	quiet, err := telegram.NewQuietHours(now.Add(-time.Hour).Format("15:04"), now.Add(time.Hour).Format("15:04"), "UTC")
	if err != nil {
		t.Fatal(err)
	}

	for _, alertType := range []string{models.AlertTypeBlock, models.AlertTypeKES} {
		t.Run(alertType, func(t *testing.T) {
			m, stub := newTestMonitor(t)
			tg := m.tg
			pass := func(pool string, level int) {
				var b alertBatch
				if alertType == models.AlertTypeBlock {
					m.evaluateWithReminder(&b, pool, "operator", "ticker", level > 0, "incident", "reminder", "recovery")
				} else {
					m.evaluateTiered(&b, pool, "operator", "ticker", level, fmt.Sprintf("tier %d", level), "recovery")
				}
				m.flush(&b)
			}
			assertMessages := func(want ...string) {
				t.Helper()
				if got := stub.take(); !slices.Equal(got, want) {
					t.Fatalf("messages = %q, want %q", got, want)
				}
			}

			tg.QuietHours = quiet
			pass("ongoing", 1)
			assertMessages()
			tg.QuietHours = nil
			pass("ongoing", 1)
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
			pass("ongoing", 2)
			assertMessages()
			tg.QuietHours = nil
			pass("ongoing", 2)
			if alertType == models.AlertTypeBlock {
				assertMessages("reminder")
			} else {
				assertMessages("tier 2")
			}

			tg.QuietHours = quiet
			pass("ongoing", 0)
			assertMessages()
			tg.QuietHours = nil
			pass("ongoing", 0)
			assertMessages("recovery")
			pass("ongoing", 0)
			assertMessages()

			// An incident that starts and clears overnight needs no stale alert
			// or recovery for an incident the recipient never saw.
			tg.QuietHours = quiet
			pass("transient", 1)
			assertMessages()
			tg.QuietHours = nil
			pass("transient", 0)
			assertMessages()
		})
	}
}

func TestPassSendsOneMessage(t *testing.T) {
	for _, alertType := range []string{models.AlertTypeBlock, models.AlertTypeKES} {
		t.Run(alertType, func(t *testing.T) {
			m, stub := newTestMonitor(t)
			pass := func() {
				var b alertBatch
				evaluate(m, &b, alertType, "a", "op1", 1, "⛔ *A* (op1) — first")
				evaluate(m, &b, alertType, "b", "op1", 1, "⛔ *B* (op1) — second")
				// The same pool listed under a second operator must not
				// alert twice from the state its first listing replaces.
				evaluate(m, &b, alertType, "a", "op2", 1, "⛔ *A* (op2) — duplicate")
				evaluate(m, &b, alertType, "c", "op2", 1, "⛔ *C* (op2) — third")
				m.flush(&b)
			}

			pass()
			msgs := stub.take()
			if len(msgs) != 1 {
				t.Fatalf("sent %d messages, want 1: %q", len(msgs), msgs)
			}
			want := []string{"⛔ *A* (op1) — first", "⛔ *B* (op1) — second", "⛔ *C* (op2) — third"}
			if got := batchTexts(msgs[0]); !slices.Equal(got, want) {
				t.Fatalf("alerts = %q, want %q", got, want)
			}

			pass()
			if msgs := stub.take(); len(msgs) != 0 {
				t.Fatalf("delivered alerts sent again: %q", msgs)
			}
		})
	}
}

// kesTexts returns n distinct KES alert texts of exactly size bytes. The
// padding is multi-byte, so byte length and rune count differ.
func kesTexts(n, size int) []string {
	texts := make([]string, n)
	for i := range texts {
		text := fmt.Sprintf("🔑 *POOL%02d* (operator) — KES expires in ~6.3d — rotate the opcert ", i)
		if (size-len(text))%2 == 1 {
			text += "."
		}
		texts[i] = text + strings.Repeat("é", (size-len(text))/2)
	}
	return texts
}

// kesPass evaluates one KES pass with every pool at tier 1 and flushes it.
func kesPass(m *Monitor, texts []string) {
	var b alertBatch
	for i, text := range texts {
		m.evaluateTiered(&b, fmt.Sprintf("pool%02d", i), "operator", "ticker", 1, text, "recovery")
	}
	m.flush(&b)
}

func TestBatchSplitsAtAlertBoundaries(t *testing.T) {
	m, stub := newTestMonitor(t)
	// 16 texts plus their separators fill exactly 4096 bytes, so a chunk that
	// ignores the header, the separators, or multi-byte runes overflows.
	texts := kesTexts(60, telegramMaxBytes/16-len(alertSep))

	kesPass(m, texts)
	msgs := stub.take()
	if len(msgs) < 2 {
		t.Fatalf("sent %d messages, want a split batch", len(msgs))
	}
	var got []string
	for i, msg := range msgs {
		if len(msg) > telegramMaxBytes {
			t.Errorf("message %d is %d bytes, limit %d", i+1, len(msg), telegramMaxBytes)
		}
		got = append(got, batchTexts(msg)...)
	}
	if !slices.Equal(got, texts) {
		t.Fatalf("split alerts = %q, want %q", got, texts)
	}
}

func TestPartialBatchDeliveryCommitsDeliveredAlerts(t *testing.T) {
	m, stub := newTestMonitor(t)
	stub.failAt = 2
	texts := kesTexts(20, telegramMaxBytes/16-len(alertSep))

	kesPass(m, texts)
	msgs := stub.take()
	if len(msgs) != 2 {
		t.Fatalf("sent %d messages, want 2", len(msgs))
	}
	delivered, failed := batchTexts(msgs[0]), batchTexts(msgs[1])
	for i, text := range texts {
		state, found, err := database.GetAlertState(fmt.Sprintf("pool%02d", i), models.AlertTypeKES)
		if err != nil {
			t.Fatal(err)
		}
		committed := found && state.Status == models.StatusAlerting
		if want := slices.Contains(delivered, text); committed != want {
			t.Errorf("pool%02d committed = %t, want %t", i, committed, want)
		}
	}

	kesPass(m, texts)
	msgs = stub.take()
	if len(msgs) != 1 {
		t.Fatalf("retry sent %d messages, want 1", len(msgs))
	}
	if got := batchTexts(msgs[0]); !slices.Equal(got, failed) {
		t.Fatalf("retried alerts = %q, want the failed chunk %q", got, failed)
	}

	kesPass(m, texts)
	if msgs := stub.take(); len(msgs) != 0 {
		t.Fatalf("delivered alerts sent again: %q", msgs)
	}
}

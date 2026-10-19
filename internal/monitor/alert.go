package monitor

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"spo-data/internal/database"
	"spo-data/internal/models"
	"spo-data/internal/telegram"
)

// telegramMaxBytes is Telegram's 4096-character message limit, applied to the
// UTF-8 byte length. UTF-8 length is never less than the UTF-16 length that
// Telegram counts, so a message within this budget is never too long.
const telegramMaxBytes = 4096

// alertSep separates the header and the alert texts of a batched message.
const alertSep = "\n\n"

// pendingAlert is a notification produced during a pass, plus the alert state
// its pool moves to once the notification is delivered.
type pendingAlert struct {
	text  string
	state models.PoolAlertState
}

// alertBatch collects the notifications of one check pass so they reach
// Telegram together. Each pass owns its batch: the block and KES loops run in
// separate goroutines.
type alertBatch struct {
	alerts []pendingAlert
}

func (b *alertBatch) add(text string, state models.PoolAlertState) {
	b.alerts = append(b.alerts, pendingAlert{text: text, state: state})
}

// pending reports whether the batch already holds a notification for
// (poolID, alertType). A pool listed under several operators is evaluated once
// per listing; later listings must not notify again from the stored state that
// the first listing's notification replaces on delivery.
func (b *alertBatch) pending(poolID, alertType string) bool {
	for _, a := range b.alerts {
		if a.state.PoolID == poolID && a.state.AlertType == alertType {
			return true
		}
	}
	return false
}

// flush sends the batch and commits each pool's new state only after the
// message that carries its text is delivered. A deferred (quiet hours) or
// failed message leaves its pools unchanged, so the next pass reassesses them.
// Without a Telegram client (dry-run) alerts are only logged and count as
// delivered.
func (m *Monitor) flush(b *alertBatch) {
	chunks := splitAlerts(b.alerts, telegramMaxBytes)
	for i, chunk := range chunks {
		if m.tg != nil {
			if err := m.tg.Send(alertMessage(chunk, len(b.alerts), i+1, len(chunks))); err != nil {
				if errors.Is(err, telegram.ErrQuietHours) {
					for _, a := range chunk {
						m.logger.Printf("DEFERRED (quiet hours) %s", a.text)
					}
				} else {
					m.logger.Printf("warn: telegram send failed: %v", err)
				}
				continue
			}
		}
		for j := range chunk {
			m.logger.Printf("ALERT %s", chunk[j].text)
			m.saveAlertState(&chunk[j].state)
		}
	}
}

// splitAlerts groups alerts, in order, into chunks whose rendered message fits
// in maxBytes. It splits only between alerts: an alert that is too long by
// itself still gets one chunk.
func splitAlerts(alerts []pendingAlert, maxBytes int) [][]pendingAlert {
	n := len(alerts)
	if n == 0 {
		return nil
	}
	// Reserve the longest header a chunk can get (at most one chunk per alert).
	reserve := len(batchHeader(n, n, n))
	var chunks [][]pendingAlert
	start, size := 0, reserve
	for i, a := range alerts {
		cost := len(alertSep) + len(a.text)
		if i > start && size+cost > maxBytes {
			chunks = append(chunks, alerts[start:i])
			start, size = i, reserve
		}
		size += cost
	}
	return append(chunks, alerts[start:])
}

// alertMessage renders chunk part (1-based) of parts for a batch of total
// alerts. A lone alert is sent as its text unchanged; otherwise a bold count
// header precedes the alert texts, which keep their own Markdown.
func alertMessage(chunk []pendingAlert, total, part, parts int) string {
	if total == 1 {
		return chunk[0].text
	}
	var sb strings.Builder
	sb.WriteString(batchHeader(total, part, parts))
	for _, a := range chunk {
		sb.WriteString(alertSep)
		sb.WriteString(a.text)
	}
	return sb.String()
}

func batchHeader(total, part, parts int) string {
	if parts == 1 {
		return fmt.Sprintf("*%d alerts*", total)
	}
	return fmt.Sprintf("*%d alerts (%d/%d)*", total, part, parts)
}

// loadAlertState returns the stored state for (poolID, alertType), or a fresh
// OK state if none exists. Operator/Ticker are refreshed on the returned value.
func (m *Monitor) loadAlertState(poolID, alertType, operator, ticker string) models.PoolAlertState {
	state, found, err := database.GetAlertState(poolID, alertType)
	if err != nil {
		m.logger.Printf("warn: GetAlertState(%s,%s) failed: %v", ticker, alertType, err)
	}
	if !found {
		state = models.PoolAlertState{PoolID: poolID, AlertType: alertType, Status: models.StatusOK}
	}
	state.Operator = operator
	state.Ticker = ticker
	return state
}

func (m *Monitor) saveAlertState(state *models.PoolAlertState) {
	if err := database.SaveAlertState(state); err != nil {
		m.logger.Printf("warn: SaveAlertState(%s,%s) failed: %v", state.Ticker, state.AlertType, err)
	}
}

// evaluateWithReminder drives the block-style state machine: one alert on
// entering violation, a reminder every ReminderInterval while it persists, and
// a recovery message when it clears. A notification goes into b together with
// the state to commit once it is delivered; a state without one is saved now.
func (m *Monitor) evaluateWithReminder(b *alertBatch, poolID, operator, ticker string, inViolation bool, alertMsg, reminderMsg, recoveryMsg string) {
	if b.pending(poolID, models.AlertTypeBlock) {
		return
	}
	state := m.loadAlertState(poolID, models.AlertTypeBlock, operator, ticker)
	now := time.Now()

	switch {
	case inViolation && state.Status != models.StatusAlerting:
		state.Status = models.StatusAlerting
		state.FirstAlertedAt = now
		state.LastReminderAt = now
		b.add(alertMsg, state)
	case inViolation && state.Status == models.StatusAlerting && now.Sub(state.LastReminderAt) >= m.set.ReminderInterval:
		state.LastReminderAt = now
		b.add(reminderMsg, state)
	case !inViolation && state.Status == models.StatusAlerting:
		state.Status = models.StatusOK
		state.NotifiedLevel = 0
		b.add(recoveryMsg, state)
	default:
		m.saveAlertState(&state)
	}
}

// evaluateTiered drives the KES-style state machine: alert when the first
// threshold tier is crossed, re-alert when a more severe tier is crossed
// (14 -> 7 -> 2 days), and recover when the condition clears (e.g. rotation).
// level is the count of crossed tiers (0 == healthy, higher == more severe).
// Notifications and saves follow evaluateWithReminder.
func (m *Monitor) evaluateTiered(b *alertBatch, poolID, operator, ticker string, level int, alertMsg, recoveryMsg string) {
	if b.pending(poolID, models.AlertTypeKES) {
		return
	}
	state := m.loadAlertState(poolID, models.AlertTypeKES, operator, ticker)
	now := time.Now()

	switch {
	case level <= 0 && state.Status == models.StatusAlerting:
		state.Status = models.StatusOK
		state.NotifiedLevel = 0
		b.add(recoveryMsg, state)
	case level > 0 && state.Status != models.StatusAlerting:
		state.Status = models.StatusAlerting
		state.FirstAlertedAt = now
		state.LastReminderAt = now
		state.NotifiedLevel = level
		b.add(alertMsg, state)
	case level > 0 && state.Status == models.StatusAlerting && level > state.NotifiedLevel:
		state.NotifiedLevel = level
		state.LastReminderAt = now
		b.add(alertMsg, state)
	default:
		m.saveAlertState(&state)
	}
}

// fmtDuration renders a duration compactly: hours under 2 days, else days.
func fmtDuration(d time.Duration) string {
	h := d.Hours()
	if h < 48 {
		return fmt.Sprintf("%.0fh", h)
	}
	return fmt.Sprintf("%.1fd", h/24)
}

package monitor

import (
	"fmt"
	"time"

	"spo-data/internal/database"
	"spo-data/internal/models"
)

// notify logs the message and, when a Telegram client is configured, sends it.
// Send failures are logged but never propagated — a missed send is retried on
// the next cycle by the state machine.
func (m *Monitor) notify(msg string) {
	m.logger.Printf("ALERT %s", msg)
	if m.tg == nil {
		return
	}
	if err := m.tg.Send(msg); err != nil {
		m.logger.Printf("warn: telegram send failed: %v", err)
	}
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
// a recovery message when it clears.
func (m *Monitor) evaluateWithReminder(poolID, operator, ticker string, inViolation bool, alertMsg, reminderMsg, recoveryMsg string) {
	state := m.loadAlertState(poolID, models.AlertTypeBlock, operator, ticker)
	now := time.Now()

	switch {
	case inViolation && state.Status != models.StatusAlerting:
		state.Status = models.StatusAlerting
		state.FirstAlertedAt = now
		state.LastReminderAt = now
		m.notify(alertMsg)
	case inViolation && state.Status == models.StatusAlerting:
		if now.Sub(state.LastReminderAt) >= m.set.ReminderInterval {
			state.LastReminderAt = now
			m.notify(reminderMsg)
		}
	case !inViolation && state.Status == models.StatusAlerting:
		state.Status = models.StatusOK
		state.NotifiedLevel = 0
		m.notify(recoveryMsg)
	}

	m.saveAlertState(&state)
}

// evaluateTiered drives the KES-style state machine: alert when the first
// threshold tier is crossed, re-alert when a more severe tier is crossed
// (14 -> 7 -> 2 days), and recover when the condition clears (e.g. rotation).
// level is the count of crossed tiers (0 == healthy, higher == more severe).
func (m *Monitor) evaluateTiered(poolID, operator, ticker string, level int, alertMsg, recoveryMsg string) {
	state := m.loadAlertState(poolID, models.AlertTypeKES, operator, ticker)
	now := time.Now()

	switch {
	case level <= 0 && state.Status == models.StatusAlerting:
		state.Status = models.StatusOK
		state.NotifiedLevel = 0
		m.notify(recoveryMsg)
	case level > 0 && state.Status != models.StatusAlerting:
		state.Status = models.StatusAlerting
		state.FirstAlertedAt = now
		state.LastReminderAt = now
		state.NotifiedLevel = level
		m.notify(alertMsg)
	case level > 0 && state.Status == models.StatusAlerting && level > state.NotifiedLevel:
		state.NotifiedLevel = level
		state.LastReminderAt = now
		m.notify(alertMsg)
	}

	m.saveAlertState(&state)
}

// fmtDuration renders a duration compactly: hours under 2 days, else days.
func fmtDuration(d time.Duration) string {
	h := d.Hours()
	if h < 48 {
		return fmt.Sprintf("%.0fh", h)
	}
	return fmt.Sprintf("%.1fd", h/24)
}

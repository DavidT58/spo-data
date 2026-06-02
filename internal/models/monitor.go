package models

import (
	"time"

	"gorm.io/gorm"
)

// Alert type discriminators stored in PoolAlertState.AlertType.
const (
	AlertTypeBlock = "block"
	AlertTypeKES   = "kes"
)

// Alert status values stored in PoolAlertState.Status.
const (
	StatusOK       = "ok"
	StatusAlerting = "alerting"
)

// KES estimate confidence values stored in PoolKesState.Confidence.
const (
	KesConfidenceOK         = "ok"         // a counter change was observed within the scan window
	KesConfidenceLowerBound = "lowerbound" // no change observed; rotation time is a conservative lower bound
)

// PoolAlertState tracks the alert state machine for a single (pool, alert type)
// pair so we send exactly one alert per incident, bounded reminders, and a
// recovery message when the condition clears.
type PoolAlertState struct {
	gorm.Model
	PoolID         string `gorm:"index:idx_pool_alert,unique"`
	AlertType      string `gorm:"index:idx_pool_alert,unique"`
	Operator       string
	Ticker         string
	Status         string // StatusOK | StatusAlerting
	FirstAlertedAt time.Time
	LastReminderAt time.Time
	// NotifiedLevel is used by tiered alerts (KES): the index of the most
	// severe threshold tier already notified. Unused (0) for block alerts,
	// which escalate via time-based reminders instead.
	NotifiedLevel int
}

// PoolKesState tracks the on-chain operational-certificate counter per pool. A
// change in OpCertCounter signals a KES rotation, which resets the estimated
// expiry derived from RotationBlockTime.
type PoolKesState struct {
	gorm.Model
	PoolID            string `gorm:"uniqueIndex"`
	OpCertCounter     string
	RotationBlockTime time.Time
	Confidence        string // KesConfidenceOK | KesConfidenceLowerBound
}

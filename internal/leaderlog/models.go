package leaderlog

import "time"

// EpochInfo is one row per epoch the dashboard has seen. Start/End are derived
// from the local node tip (slot arithmetic), so epoch detection never depends
// on Blockfrost availability.
type EpochInfo struct {
	ID            uint `gorm:"primarykey"`
	Epoch         int  `gorm:"uniqueIndex"`
	StartTime     time.Time
	EndTime       time.Time
	ScheduleRunAt *time.Time
}

// Schedule computation status per (pool, epoch).
const (
	SchedPending    = "pending"    // not yet computed (or retryable failure, see Attempts)
	SchedOK         = "ok"         // slots stored and trustworthy
	SchedKeyMissing = "key_missing" // no vrf.skey/vrf.vkey on disk for this pool
	SchedKeyStale   = "key_stale"   // local vrf.vkey hash != on-chain registration
	SchedCLIError   = "cli_error"   // cardano-cli failed (bounded retries)
	SchedExpired    = "expired"     // epoch ended before the schedule could be computed
)

// PoolSchedule tracks the leadership-schedule computation for one pool in one
// epoch. Attempts is persisted so retries stay bounded across daemon restarts.
type PoolSchedule struct {
	ID            uint   `gorm:"primarykey"`
	PoolID        string `gorm:"index:idx_pool_epoch,unique"`
	Epoch         int    `gorm:"index:idx_pool_epoch,unique"`
	Name          string
	Operator      string
	Status        string `gorm:"index"`
	Attempts      int
	StartedAt     *time.Time
	ComputedAt    *time.Time
	CLIDurationMS int64
	VRFHashLocal  string
	VRFHashChain  string
	Error         string
}

// Slot settlement status.
const (
	SlotPending  = "pending"
	SlotProduced = "produced"
	SlotMissed   = "missed"
)

// ScheduledSlot is one leadership assignment. Slot is the absolute slot number
// (the primary settling key); SlotTime is display-only.
type ScheduledSlot struct {
	ID           uint   `gorm:"primarykey"`
	PoolID       string `gorm:"index:idx_slot,unique"`
	Epoch        int    `gorm:"index:idx_slot,unique"`
	Slot         int64  `gorm:"index:idx_slot,unique"`
	SlotTime     time.Time
	Status       string `gorm:"index"`
	BlockHash    string
	BlockHeight  int64
	BattleLeader string // bech32 of the pool that occupied the slot instead
	CheckedAt    *time.Time
}

// ObservedBlock is every on-chain block one of our pools produced. Scheduled is
// nil until the pool's schedule for that epoch is trustworthy (status ok);
// false then means a genuine anomaly (block outside the computed schedule).
type ObservedBlock struct {
	ID        uint   `gorm:"primarykey"`
	BlockHash string `gorm:"uniqueIndex"`
	PoolID    string `gorm:"index:idx_obs_pool_epoch"`
	Epoch     int    `gorm:"index:idx_obs_pool_epoch"`
	Slot      int64
	Height    int64
	Time      time.Time
	Scheduled *bool
}

// EpochSummary is the per-(pool, epoch) history row. ActualsOnly rows come from
// the Blockfrost backfill of pre-launch epochs (no schedule known).
type EpochSummary struct {
	ID             uint   `gorm:"primarykey"`
	PoolID         string `gorm:"index:idx_summary,unique"`
	Epoch          int    `gorm:"index:idx_summary,unique"`
	Scheduled      int
	Produced       int
	Missed         int
	ExpectedBlocks float64
	ActualsOnly    bool
	Finalized      bool
}

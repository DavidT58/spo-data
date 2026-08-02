package leaderlog

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// Store owns the dashboard's private SQLite database. It deliberately does NOT
// reuse internal/database, whose global handle and auto-migrations belong to
// spo-monitor's data.db.
type Store struct {
	db *gorm.DB
}

// OpenStore opens (creating if needed) the dashboard database. WAL +
// busy_timeout + a single connection serialize the scheduler loop, the
// reconciler loop and HTTP readers cleanly.
func OpenStore(path string) (*Store, error) {
	dsn := fmt.Sprintf("%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)", path)
	dbLogger := gormlogger.New(
		log.New(os.Stdout, "", log.LstdFlags),
		gormlogger.Config{
			SlowThreshold:             2 * time.Second,
			LogLevel:                  gormlogger.Warn,
			IgnoreRecordNotFoundError: true,
		},
	)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: dbLogger})
	if err != nil {
		return nil, err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(1)

	if err := db.AutoMigrate(&EpochInfo{}, &PoolSchedule{}, &ScheduledSlot{}, &ObservedBlock{}, &EpochSummary{}); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// --- epochs ---

func (s *Store) GetEpochInfo(epoch int) (EpochInfo, bool, error) {
	var e EpochInfo
	res := s.db.Where("epoch = ?", epoch).First(&e)
	if res.Error == gorm.ErrRecordNotFound {
		return EpochInfo{}, false, nil
	}
	return e, res.Error == nil, res.Error
}

func (s *Store) SaveEpochInfo(e *EpochInfo) error {
	return s.db.Save(e).Error
}

// --- pool schedules ---

func (s *Store) GetPoolSchedule(poolID string, epoch int) (PoolSchedule, bool, error) {
	var ps PoolSchedule
	res := s.db.Where("pool_id = ? AND epoch = ?", poolID, epoch).First(&ps)
	if res.Error == gorm.ErrRecordNotFound {
		return PoolSchedule{}, false, nil
	}
	return ps, res.Error == nil, res.Error
}

func (s *Store) SavePoolSchedule(ps *PoolSchedule) error {
	return s.db.Save(ps).Error
}

func (s *Store) PoolSchedulesForEpoch(epoch int) ([]PoolSchedule, error) {
	var out []PoolSchedule
	err := s.db.Where("epoch = ?", epoch).Find(&out).Error
	return out, err
}

// --- scheduled slots ---

// ReplaceScheduledSlots atomically replaces a pool's slot set for an epoch.
// Statuses of slots that already settled are preserved by slot number so a
// recompute never un-settles history.
func (s *Store) ReplaceScheduledSlots(poolID string, epoch int, slots []ScheduledSlot) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		var existing []ScheduledSlot
		if err := tx.Where("pool_id = ? AND epoch = ?", poolID, epoch).Find(&existing).Error; err != nil {
			return err
		}
		settled := make(map[int64]ScheduledSlot, len(existing))
		for _, e := range existing {
			if e.Status != SlotPending {
				settled[e.Slot] = e
			}
		}
		if err := tx.Where("pool_id = ? AND epoch = ?", poolID, epoch).Delete(&ScheduledSlot{}).Error; err != nil {
			return err
		}
		for i := range slots {
			if prev, ok := settled[slots[i].Slot]; ok {
				slots[i].Status = prev.Status
				slots[i].BlockHash = prev.BlockHash
				slots[i].BlockHeight = prev.BlockHeight
				slots[i].BattleLeader = prev.BattleLeader
				slots[i].CheckedAt = prev.CheckedAt
			}
		}
		if len(slots) == 0 {
			return nil
		}
		return tx.Create(&slots).Error
	})
}

// PendingSlotsBefore returns all pending slots (any epoch) with an absolute
// slot older than the given cutoff, oldest first.
func (s *Store) PendingSlotsBefore(cutoff int64) ([]ScheduledSlot, error) {
	var out []ScheduledSlot
	err := s.db.Where("status = ? AND slot < ?", SlotPending, cutoff).Order("slot asc").Find(&out).Error
	return out, err
}

func (s *Store) SaveSlot(slot *ScheduledSlot) error {
	return s.db.Save(slot).Error
}

func (s *Store) SlotsForPoolEpoch(poolID string, epoch int) ([]ScheduledSlot, error) {
	var out []ScheduledSlot
	err := s.db.Where("pool_id = ? AND epoch = ?", poolID, epoch).Order("slot asc").Find(&out).Error
	return out, err
}

// FindScheduledSlot looks a slot up by its unique (pool, epoch, slot) key.
func (s *Store) FindScheduledSlot(poolID string, epoch int, slot int64) (ScheduledSlot, bool, error) {
	var sl ScheduledSlot
	res := s.db.Where("pool_id = ? AND epoch = ? AND slot = ?", poolID, epoch, slot).First(&sl)
	if res.Error == gorm.ErrRecordNotFound {
		return ScheduledSlot{}, false, nil
	}
	return sl, res.Error == nil, res.Error
}

// --- observed blocks ---

func (s *Store) HasObservedBlock(hash string) (bool, error) {
	var n int64
	err := s.db.Model(&ObservedBlock{}).Where("block_hash = ?", hash).Count(&n).Error
	return n > 0, err
}

func (s *Store) SaveObservedBlock(b *ObservedBlock) error {
	return s.db.Save(b).Error
}

func (s *Store) ObservedBlocks(poolID string, epoch int) ([]ObservedBlock, error) {
	var out []ObservedBlock
	err := s.db.Where("pool_id = ? AND epoch = ?", poolID, epoch).Order("slot asc").Find(&out).Error
	return out, err
}

// UnclassifiedObserved returns a pool's observed blocks whose scheduled flag is
// still NULL (stored before the schedule was trustworthy).
func (s *Store) UnclassifiedObserved(poolID string, epoch int) ([]ObservedBlock, error) {
	var out []ObservedBlock
	err := s.db.Where("pool_id = ? AND epoch = ? AND scheduled IS NULL", poolID, epoch).Find(&out).Error
	return out, err
}

func (s *Store) CountObservedBlocks(poolID string, epoch int) (int64, error) {
	var n int64
	err := s.db.Model(&ObservedBlock{}).Where("pool_id = ? AND epoch = ?", poolID, epoch).Count(&n).Error
	return n, err
}

func (s *Store) AnomalyCount(poolID string, epoch int) (int64, error) {
	var n int64
	err := s.db.Model(&ObservedBlock{}).Where("pool_id = ? AND epoch = ? AND scheduled = ?", poolID, epoch, false).Count(&n).Error
	return n, err
}

// --- epoch summaries ---

func (s *Store) GetEpochSummary(poolID string, epoch int) (EpochSummary, bool, error) {
	var es EpochSummary
	res := s.db.Where("pool_id = ? AND epoch = ?", poolID, epoch).First(&es)
	if res.Error == gorm.ErrRecordNotFound {
		return EpochSummary{}, false, nil
	}
	return es, res.Error == nil, res.Error
}

func (s *Store) SaveEpochSummary(es *EpochSummary) error {
	return s.db.Save(es).Error
}

func (s *Store) SummariesForPool(poolID string, limit int) ([]EpochSummary, error) {
	var out []EpochSummary
	err := s.db.Where("pool_id = ?", poolID).Order("epoch desc").Limit(limit).Find(&out).Error
	return out, err
}

func (s *Store) HasAnySummary(poolID string) (bool, error) {
	var n int64
	err := s.db.Model(&EpochSummary{}).Where("pool_id = ?", poolID).Count(&n).Error
	return n > 0, err
}

// EpochsNeedingReconcile returns the distinct epochs that still have pending
// slots or a tracked (pool, epoch) schedule without a finalized schedule-based
// summary, plus always the given current epoch.
func (s *Store) EpochsNeedingReconcile(current int) ([]int, error) {
	set := map[int]struct{}{current: {}}
	var epochs []int
	if err := s.db.Model(&ScheduledSlot{}).Where("status = ?", SlotPending).Distinct("epoch").Pluck("epoch", &epochs).Error; err != nil {
		return nil, err
	}
	for _, e := range epochs {
		set[e] = struct{}{}
	}
	epochs = epochs[:0]
	// Any finalized summary (schedule-based or the expired/actuals-only
	// fallback) settles a (pool, epoch); backfill rows can't collide because
	// maybeBackfill skips schedule-tracked epochs.
	if err := s.db.Raw(`
		SELECT DISTINCT ps.epoch FROM pool_schedules ps
		LEFT JOIN epoch_summaries es
		  ON es.pool_id = ps.pool_id AND es.epoch = ps.epoch
		WHERE es.id IS NULL OR es.finalized = 0`).Scan(&epochs).Error; err != nil {
		return nil, err
	}
	for _, e := range epochs {
		set[e] = struct{}{}
	}
	out := make([]int, 0, len(set))
	for e := range set {
		out = append(out, e)
	}
	return out, nil
}

// SlotCounts aggregates a pool's slot statuses for one epoch.
type SlotCounts struct {
	Scheduled int64
	Produced  int64
	Missed    int64
	Pending   int64
}

func (s *Store) CountSlots(poolID string, epoch int) (SlotCounts, error) {
	var c SlotCounts
	type row struct {
		Status string
		N      int64
	}
	var rows []row
	err := s.db.Model(&ScheduledSlot{}).
		Select("status, count(*) as n").
		Where("pool_id = ? AND epoch = ?", poolID, epoch).
		Group("status").Scan(&rows).Error
	if err != nil {
		return c, err
	}
	for _, r := range rows {
		c.Scheduled += r.N
		switch r.Status {
		case SlotProduced:
			c.Produced = r.N
		case SlotMissed:
			c.Missed = r.N
		case SlotPending:
			c.Pending = r.N
		}
	}
	return c, nil
}

// NextPendingSlotAfter returns the pool's next pending slot at/after the given
// absolute slot, if any.
func (s *Store) NextPendingSlotAfter(poolID string, slot int64) (ScheduledSlot, bool, error) {
	var sl ScheduledSlot
	res := s.db.Where("pool_id = ? AND status = ? AND slot >= ?", poolID, SlotPending, slot).Order("slot asc").First(&sl)
	if res.Error == gorm.ErrRecordNotFound {
		return ScheduledSlot{}, false, nil
	}
	return sl, res.Error == nil, res.Error
}

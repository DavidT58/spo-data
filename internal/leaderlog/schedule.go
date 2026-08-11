package leaderlog

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"spo-data/configs"
)

// schedulerTick is the epoch-boundary detector + schedule catch-up pass. The
// local node tip is the authoritative rollover signal; Blockfrost never gates
// schedule computation.
func (s *Service) schedulerTick(ctx context.Context) {
	s.reloadConfig()

	tip, err := s.cli.Tip(ctx)
	if err != nil {
		s.logger.Printf("warn: query tip failed, skipping scheduler tick: %v", err)
		return
	}
	setHealth(func(h *Health) {
		h.LastSchedulerTick = time.Now().UTC()
		h.CurrentEpoch = tip.Epoch
		h.TipSlot = tip.Slot
		h.NodeSynced = tip.Synced()
		h.PoolCount = len(s.Pools())
	})
	if !tip.Synced() {
		s.logger.Printf("warn: node not synced (%s%%), skipping scheduler tick", tip.SyncProgress)
		return
	}

	s.ensureEpochInfo(tip)

	// Catch-up: compute for every pool whose current-epoch schedule is missing,
	// stuck pending (daemon restarted mid-run), or retryable with attempts
	// left. Runs on a small worker pool (CLIParallel); each worker re-checks
	// the node epoch before starting a pool so a rollover mid-pass aborts the
	// pass instead of burning attempts against the wrong epoch.
	type job struct {
		pool  configs.PoolConfig
		ps    PoolSchedule
		found bool
	}
	var jobs []job
	for _, pool := range s.Pools() {
		// First sighting of a pool: backfill BEFORE any summary row can be
		// created for it (finalizeSummaries below), else the HasAnySummary
		// gate would skip the backfill forever.
		s.maybeBackfill(ctx, pool.PoolID, tip.Epoch)

		ps, found, err := s.store.GetPoolSchedule(pool.PoolID, tip.Epoch)
		if err != nil {
			s.logger.Printf("warn: schedule lookup failed for %s: %v", pool.Name, err)
			continue
		}
		if found && !s.needsCompute(ps) {
			continue
		}
		jobs = append(jobs, job{pool: pool, ps: ps, found: found})
	}

	if len(jobs) > 0 {
		workers := s.set.CLIParallel
		if workers < 1 {
			workers = 1
		}
		if workers > len(jobs) {
			workers = len(jobs)
		}
		jobCh := make(chan job)
		var rolledOver atomic.Bool
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range jobCh {
					if ctx.Err() != nil || rolledOver.Load() {
						continue
					}
					cur, err := s.cli.Tip(ctx)
					if err != nil {
						s.logger.Printf("warn: tip re-check failed, skipping %s this tick: %v", j.pool.Name, err)
						continue
					}
					if cur.Epoch != tip.Epoch {
						if !rolledOver.Swap(true) {
							s.logger.Printf("epoch rolled over %d -> %d mid-pass, aborting; next tick recomputes", tip.Epoch, cur.Epoch)
						}
						continue
					}
					s.computePool(ctx, j.pool, tip, j.ps, j.found)
					select {
					case <-time.After(s.set.CLIGap):
					case <-ctx.Done():
					}
				}
			}()
		}
		for _, j := range jobs {
			jobCh <- j
		}
		close(jobCh)
		wg.Wait()
		if rolledOver.Load() || ctx.Err() != nil {
			return
		}
	}
	// The first full catch-up pass has re-verified any key_missing/key_stale
	// pools; from here they wait for the next epoch (or a restart).
	s.recheckedKeys.Store(true)

	s.finalizeSummaries(ctx, tip)
}

// needsCompute decides whether an existing PoolSchedule row still needs work.
func (s *Service) needsCompute(ps PoolSchedule) bool {
	switch ps.Status {
	case SchedOK, SchedExpired:
		return false
	case SchedKeyMissing, SchedKeyStale:
		// Re-checked once per daemon start (the operator's fix for a key issue
		// is to place the key and restart) and again at each new epoch.
		return !s.recheckedKeys.Load()
	case SchedCLIError:
		return ps.Attempts < s.set.MaxAttempts
	case SchedPending:
		if ps.Attempts >= s.set.MaxAttempts {
			return false
		}
		// A pending row younger than the cli timeout may still be an in-flight
		// run of THIS process; older means a crashed/restarted run.
		return ps.StartedAt == nil || time.Since(*ps.StartedAt) > s.set.CLITimeout
	default:
		return true
	}
}

// epochBounds derives the current epoch's absolute-slot window and wall-clock
// bounds directly from the tip's slotInEpoch/slotsToEpochEnd. Never derive the
// window as epoch × EpochLength: Prime's slot numbering includes a Byron-era
// prefix (2 epochs), so epoch 162 starts at slot 69,163,200, not 69,984,000.
func (s *Service) epochBounds(tip Tip) (start, end time.Time, startSlot, endSlot int64) {
	slotLen := time.Duration(s.set.Net.SlotLength) * time.Second
	startSlot = tip.Slot - tip.SlotInEpoch
	endSlot = tip.Slot + tip.SlotsToEpochEnd
	now := time.Now().UTC()
	start = now.Add(-time.Duration(tip.SlotInEpoch) * slotLen)
	end = now.Add(time.Duration(tip.SlotsToEpochEnd) * slotLen)
	return start, end, startSlot, endSlot
}

func (s *Service) ensureEpochInfo(tip Tip) {
	_, found, err := s.store.GetEpochInfo(tip.Epoch)
	if err != nil {
		s.logger.Printf("warn: epoch info lookup failed: %v", err)
		return
	}
	if found {
		return
	}
	start, end, _, _ := s.epochBounds(tip)
	if err := s.store.SaveEpochInfo(&EpochInfo{Epoch: tip.Epoch, StartTime: start, EndTime: end}); err != nil {
		s.logger.Printf("warn: failed to save epoch info %d: %v", tip.Epoch, err)
		return
	}
	s.logger.Printf("epoch %d detected (start %s, end %s)", tip.Epoch, start.Format(time.RFC3339), end.Format(time.RFC3339))
}

// computePool runs the full schedule pipeline for one pool: VRF key
// verification, the cli leadership-schedule run, slot storage, and — once the
// schedule is trustworthy — re-classification of blocks observed earlier.
func (s *Service) computePool(ctx context.Context, pool configs.PoolConfig, tip Tip, ps PoolSchedule, found bool) {
	if !found {
		ps = PoolSchedule{PoolID: pool.PoolID, Epoch: tip.Epoch}
	}
	ps.Name = pool.Name
	ps.Operator = pool.Operator
	now := time.Now().UTC()
	ps.StartedAt = &now
	ps.Status = SchedPending
	ps.Attempts++
	if err := s.store.SavePoolSchedule(&ps); err != nil {
		s.logger.Printf("warn: failed to save schedule row for %s: %v", pool.Name, err)
		return
	}

	fail := func(status, msg string) {
		// A shutdown mid-run is not a genuine failure: refund the attempt and
		// leave the row pending so the next daemon start retries cleanly.
		if ctx.Err() != nil {
			ps.Attempts--
			ps.Status = SchedPending
			ps.Error = "interrupted by shutdown"
			if err := s.store.SavePoolSchedule(&ps); err != nil {
				s.logger.Printf("warn: failed to persist shutdown state for %s: %v", pool.Name, err)
			}
			return
		}
		ps.Status = status
		ps.Error = msg
		if err := s.store.SavePoolSchedule(&ps); err != nil {
			s.logger.Printf("warn: failed to persist %s for %s: %v", status, pool.Name, err)
		}
		s.logger.Printf("schedule %s (%s) epoch %d: %s — %s", pool.Name, pool.Operator, tip.Epoch, status, msg)
	}

	skeyPath, vkeyPath := KeyPaths(s.set.KeysDir, pool.PoolID)
	if _, err := os.Stat(skeyPath); err != nil {
		fail(SchedKeyMissing, fmt.Sprintf("vrf.skey not found at %s", skeyPath))
		return
	}

	// Verify the local key against the on-chain registration. A Blockfrost
	// ERROR is retryable (stay pending) — only a successful fetch that
	// disagrees marks the key stale.
	localHash, err := s.cli.VRFKeyHash(ctx, vkeyPath)
	if err != nil {
		fail(SchedKeyMissing, fmt.Sprintf("failed to hash vrf.vkey: %v", err))
		return
	}
	ps.VRFHashLocal = localHash
	cctx, cancel := callCtx(ctx)
	poolInfo, err := s.api.Pool(cctx, pool.PoolID)
	cancel()
	if err != nil {
		// Blockfrost must never gate schedule computation permanently: refund
		// the attempt so only key checks and actual cli runs draw from the
		// bounded budget. Retry pacing stays bounded by the StartedAt gate.
		ps.Attempts--
		fail(SchedPending, fmt.Sprintf("blockfrost pool lookup failed (retryable): %v", err))
		return
	}
	ps.VRFHashChain = poolInfo.VrfKey
	if poolInfo.VrfKey != "" && poolInfo.VrfKey != localHash {
		fail(SchedKeyStale, fmt.Sprintf("local vrf key %s != on-chain %s", localHash, poolInfo.VrfKey))
		return
	}
	ps.Margin, ps.FixedCostAp3x = feeParams(poolInfo)

	// Best-effort earnings estimate (avg reward per block over recent completed
	// epochs). Failure is non-fatal — ensureRewardEstimates self-heals later.
	hctx, hcancel := callCtx(ctx)
	if hist, herr := s.api.PoolHistory(hctx, pool.PoolID, historyQuery()); herr == nil {
		ps.RewardPerBlockEst = rewardPerBlockEst(hist, tip.Epoch)
	}
	hcancel()

	cliStart := time.Now()
	entries, err := s.cli.LeadershipSchedule(ctx, pool.PoolID, skeyPath)
	ps.CLIDurationMS = time.Since(cliStart).Milliseconds()
	if err != nil {
		fail(SchedCLIError, err.Error())
		return
	}

	// Sanity: every slot must fall inside this epoch's absolute-slot window,
	// otherwise a lagging node answered for a different epoch.
	_, _, startSlot, endSlot := s.epochBounds(tip)
	slots := make([]ScheduledSlot, 0, len(entries))
	for _, e := range entries {
		if e.Slot < startSlot || e.Slot >= endSlot {
			fail(SchedCLIError, fmt.Sprintf("slot %d outside epoch %d window [%d,%d) — node lagging?", e.Slot, tip.Epoch, startSlot, endSlot))
			return
		}
		slots = append(slots, ScheduledSlot{
			PoolID:   pool.PoolID,
			Epoch:    tip.Epoch,
			Slot:     e.Slot,
			SlotTime: e.SlotTime,
			Status:   SlotPending,
		})
	}
	if err := s.store.ReplaceScheduledSlots(pool.PoolID, tip.Epoch, slots); err != nil {
		fail(SchedCLIError, fmt.Sprintf("failed to store slots: %v", err))
		return
	}

	done := time.Now().UTC()
	ps.ComputedAt = &done
	ps.Status = SchedOK
	ps.Error = ""
	if err := s.store.SavePoolSchedule(&ps); err != nil {
		s.logger.Printf("warn: failed to mark schedule ok for %s: %v", pool.Name, err)
		return
	}
	s.logger.Printf("schedule %s (%s) epoch %d: %d slots in %.1fs",
		pool.Name, pool.Operator, tip.Epoch, len(slots), float64(ps.CLIDurationMS)/1000)

	// The schedule just became trustworthy: classify blocks that were observed
	// while it wasn't (anomaly gating).
	s.rematchObserved(pool.PoolID, tip.Epoch)
}

// rematchObserved classifies a pool's previously-unclassifiable observed
// blocks now that its schedule for the epoch is ok. DB-only, cheap.
func (s *Service) rematchObserved(poolID string, epoch int) {
	blocks, err := s.store.UnclassifiedObserved(poolID, epoch)
	if err != nil {
		s.logger.Printf("warn: rematch query failed for %s epoch %d: %v", poolID, epoch, err)
		return
	}
	for _, b := range blocks {
		slot, found, err := s.store.FindScheduledSlot(poolID, epoch, b.Slot)
		if err != nil {
			continue
		}
		scheduled := found
		b.Scheduled = &scheduled
		if err := s.store.SaveObservedBlock(&b); err != nil {
			s.logger.Printf("warn: rematch save failed for block %s: %v", b.BlockHash, err)
			continue
		}
		if found && slot.Status == SlotPending {
			now := time.Now().UTC()
			slot.Status = SlotProduced
			slot.BlockHash = b.BlockHash
			slot.BlockHeight = b.Height
			slot.CheckedAt = &now
			if err := s.store.SaveSlot(&slot); err != nil {
				s.logger.Printf("warn: rematch slot update failed: %v", err)
			}
		}
	}
}

// finalizeSummaries closes out past epochs whose slots have all settled, and
// keeps the current epoch's summary row up to date. A past-epoch schedule
// still pending is unrecoverable (only the current epoch can be computed) and
// is transitioned to expired so the epoch can finalize instead of pinning
// reconcile work forever.
func (s *Service) finalizeSummaries(ctx context.Context, tip Tip) {
	epochs, err := s.store.EpochsNeedingReconcile(tip.Epoch)
	if err != nil {
		s.logger.Printf("warn: summary epoch scan failed: %v", err)
		return
	}
	for _, epoch := range epochs {
		schedules, err := s.store.PoolSchedulesForEpoch(epoch)
		if err != nil {
			continue
		}
		for _, ps := range schedules {
			if epoch < tip.Epoch && ps.Status == SchedPending {
				ps.Status = SchedExpired
				ps.Error = "epoch ended before the schedule could be computed"
				if err := s.store.SavePoolSchedule(&ps); err != nil {
					s.logger.Printf("warn: failed to expire schedule %s/%d: %v", ps.PoolID, epoch, err)
					continue
				}
				s.logger.Printf("schedule %s epoch %d: expired uncomputed", ps.Name, epoch)
			}

			counts, err := s.store.CountSlots(ps.PoolID, epoch)
			if err != nil {
				continue
			}
			es, _, err := s.store.GetEpochSummary(ps.PoolID, epoch)
			if err != nil {
				continue
			}
			es.PoolID = ps.PoolID
			es.Epoch = epoch

			if ps.Status == SchedOK {
				es.Scheduled = int(counts.Scheduled)
				es.Produced = int(counts.Produced)
				es.Missed = int(counts.Missed)
				es.ActualsOnly = false
				es.Finalized = epoch < tip.Epoch && counts.Pending == 0
			} else {
				// Schedule never became trustworthy: report real production
				// (blocks are ingested regardless) instead of a bogus 0/0.
				produced, err := s.store.CountObservedBlocks(ps.PoolID, epoch)
				if err != nil {
					continue
				}
				es.Scheduled = 0
				es.Missed = 0
				es.Produced = int(produced)
				es.ActualsOnly = true
				es.Finalized = epoch < tip.Epoch && ps.Status != SchedPending
			}
			// Only completed epochs appear in pool history; skip the lookup
			// for the current epoch (it would 0 every time and re-fetch).
			if es.ExpectedBlocks == 0 && epoch < tip.Epoch {
				es.ExpectedBlocks = s.expectedBlocks(ctx, ps.PoolID, epoch)
			}
			if err := s.store.SaveEpochSummary(&es); err != nil {
				s.logger.Printf("warn: failed to save summary %s/%d: %v", ps.PoolID, epoch, err)
			}
		}
	}
}

// expectedBlocks derives a pool's stake-based expectation for an epoch from
// Blockfrost pool history; 0 when unavailable (shown as n/a).
func (s *Service) expectedBlocks(ctx context.Context, poolID string, epoch int) float64 {
	cctx, cancel := callCtx(ctx)
	defer cancel()
	hist, err := s.api.PoolHistory(cctx, poolID, historyQuery())
	if err != nil {
		return 0
	}
	for _, h := range hist {
		if h.Epoch == epoch {
			return h.ActiveSize * float64(s.set.Net.EpochLength) * s.set.Net.ActiveSlotsCoeff
		}
	}
	return 0
}

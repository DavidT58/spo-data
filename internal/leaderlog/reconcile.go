package leaderlog

import (
	"context"
	"time"

	"github.com/blockfrost/blockfrost-go"
)

// reconcileTick settles scheduled slots against the chain and ingests every
// block our pools actually produced. Strictly sequential: one pool at a time,
// bounded per-batch timeouts, no concurrent Blockfrost fan-out.
func (s *Service) reconcileTick(ctx context.Context) {
	tip, err := s.cli.Tip(ctx)
	if err != nil {
		s.logger.Printf("warn: query tip failed, skipping reconcile tick: %v", err)
		return
	}
	setHealth(func(h *Health) {
		h.LastReconcileTick = time.Now().UTC()
		h.CurrentEpoch = tip.Epoch
		h.TipSlot = tip.Slot
		h.NodeSynced = tip.Synced()
	})

	epochs, err := s.store.EpochsNeedingReconcile(tip.Epoch)
	if err != nil {
		s.logger.Printf("warn: reconcile epoch scan failed: %v", err)
		return
	}

	for _, pool := range s.Pools() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		for _, epoch := range epochs {
			s.ingestPoolBlocks(ctx, pool.PoolID, epoch)
		}
	}

	s.settlePendingSlots(ctx, tip)
}

// ingestPoolBlocks pages a pool's blocks for an epoch (newest first, stopping
// at the first already-seen hash) and classifies each new one.
func (s *Service) ingestPoolBlocks(ctx context.Context, poolID string, epoch int) {
	page := 1
	for {
		cctx, cancel := callCtx(ctx)
		hashes, err := s.api.EpochBlockDistributionByPool(cctx, epoch, poolID, blockfrost.APIQueryParams{
			Count: 100, Page: page, Order: "desc",
		})
		cancel()
		if err != nil {
			if !isNotFound(err) {
				s.logger.Printf("warn: epoch %d blocks for %s failed: %v", epoch, poolID, err)
			}
			return
		}
		newInPage := 0
		for _, hash := range hashes {
			seen, err := s.store.HasObservedBlock(hash)
			if err != nil || seen {
				continue
			}
			if s.ingestBlock(ctx, poolID, epoch, hash) {
				newInPage++
			}
		}
		// Newest-first paging: a page with nothing new means everything older
		// is already ingested.
		if newInPage == 0 || len(hashes) < 100 {
			return
		}
		page++
	}
}

// ingestBlock fetches one block, stores it, and — when the pool's schedule for
// the epoch is trustworthy — classifies it as scheduled or anomalous.
func (s *Service) ingestBlock(ctx context.Context, poolID string, epoch int, hash string) bool {
	cctx, cancel := callCtx(ctx)
	block, err := s.api.Block(cctx, hash)
	cancel()
	if err != nil {
		s.logger.Printf("warn: block %s fetch failed: %v", hash, err)
		return false
	}

	obs := ObservedBlock{
		BlockHash: hash,
		PoolID:    poolID,
		Epoch:     epoch,
		Slot:      int64(block.Slot),
		Height:    int64(block.Height),
		Time:      time.Unix(int64(block.Time), 0).UTC(),
	}

	// Anomaly gating: only classify against a schedule that landed ok. Until
	// then Scheduled stays NULL and rematchObserved picks it up later.
	ps, found, err := s.store.GetPoolSchedule(poolID, epoch)
	if err == nil && found && ps.Status == SchedOK {
		slot, matched, err := s.store.FindScheduledSlot(poolID, epoch, obs.Slot)
		if err == nil {
			scheduled := matched
			obs.Scheduled = &scheduled
			if matched && slot.Status != SlotProduced {
				now := time.Now().UTC()
				slot.Status = SlotProduced
				slot.BlockHash = hash
				slot.BlockHeight = obs.Height
				slot.CheckedAt = &now
				if err := s.store.SaveSlot(&slot); err != nil {
					s.logger.Printf("warn: slot update failed for %s@%d: %v", poolID, obs.Slot, err)
				}
			}
		}
	}

	if err := s.store.SaveObservedBlock(&obs); err != nil {
		s.logger.Printf("warn: failed to save observed block %s: %v", hash, err)
		return false
	}
	return true
}

// settlePendingSlots resolves every pending slot old enough to be settled,
// across ALL epochs (the previous epoch's tail settles normally after
// rollover). Only a decoded 404 marks a miss — transient errors leave the slot
// pending for the next tick, so an outage can never fabricate misses. The
// settle cutoff is bounded by BOTH the node tip and Blockfrost's own indexed
// tip: a lagging/resyncing indexer 404s on slots it simply hasn't seen yet,
// and treating those as misses would fabricate permanent false negatives.
func (s *Service) settlePendingSlots(ctx context.Context, tip Tip) {
	cctx, cancel := callCtx(ctx)
	bfTip, err := s.api.BlockLatest(cctx)
	cancel()
	if err != nil {
		s.logger.Printf("warn: blockfrost tip unavailable, deferring slot settlement: %v", err)
		return
	}
	cutoff := tip.Slot - s.set.SettleMarginSlots
	if bf := int64(bfTip.Slot) - s.set.SettleMarginSlots; bf < cutoff {
		cutoff = bf
	}
	pending, err := s.store.PendingSlotsBefore(cutoff)
	if err != nil {
		s.logger.Printf("warn: pending slot query failed: %v", err)
		return
	}
	if len(pending) == 0 {
		return
	}
	settled := 0
	for i := range pending {
		select {
		case <-ctx.Done():
			return
		default:
		}
		sl := pending[i]
		cctx, cancel := callCtx(ctx)
		block, err := s.api.BlockBySlot(cctx, int(sl.Slot))
		cancel()

		now := time.Now().UTC()
		switch {
		case err == nil && block.SlotLeader == sl.PoolID:
			sl.Status = SlotProduced
			sl.BlockHash = block.Hash
			sl.BlockHeight = int64(block.Height)
		case err == nil:
			// The slot exists on-chain but belongs to another pool: lost battle
			// (or our block was orphaned — equivalent for rewards).
			sl.Status = SlotMissed
			sl.BattleLeader = block.SlotLeader
		case isNotFound(err):
			// No block at this slot at all: missed (or orphaned with no
			// competing block).
			sl.Status = SlotMissed
		default:
			// Transient Blockfrost failure: leave pending, retry next tick.
			s.logger.Printf("warn: slot %d lookup failed (will retry): %v", sl.Slot, err)
			continue
		}
		sl.CheckedAt = &now
		if err := s.store.SaveSlot(&sl); err != nil {
			s.logger.Printf("warn: failed to save settled slot %d: %v", sl.Slot, err)
			continue
		}
		settled++
		if sl.Status == SlotMissed {
			s.logger.Printf("missed slot: pool %s epoch %d slot %d (battle leader %q)",
				sl.PoolID, sl.Epoch, sl.Slot, sl.BattleLeader)
		}
	}
	if settled > 0 {
		s.logger.Printf("reconcile: settled %d/%d due slots (tip %d)", settled, len(pending), tip.Slot)
	}
}

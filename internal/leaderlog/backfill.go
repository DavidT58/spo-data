package leaderlog

import (
	"context"
	"strconv"

	"github.com/blockfrost/blockfrost-go"
)

// historyQuery covers the backfill window plus recent epochs in one call.
func historyQuery() blockfrost.APIQueryParams {
	return blockfrost.APIQueryParams{Count: 100, Order: "desc"}
}

// feeParams extracts a pool's registered margin and fixed cost (AP3X) from a
// Blockfrost pool lookup.
func feeParams(p blockfrost.Pool) (margin, fixedAp3x float64) {
	margin = p.MarginCost
	if f, err := strconv.ParseFloat(p.FixedCost, 64); err == nil && f > 0 {
		fixedAp3x = f / 1e6
	}
	return margin, fixedAp3x
}

// OperatorTake is the operator's share of a total epoch reward R given the
// pool's fee parameters: min(R, fixed) + margin × max(0, R − fixed).
func OperatorTake(r, margin, fixedAp3x float64) float64 {
	if r <= fixedAp3x {
		return r
	}
	return fixedAp3x + margin*(r-fixedAp3x)
}

// OwnerTake is the pool owner's part of a total epoch reward R: the operator
// fee (OperatorTake) plus the member rewards on the owned share of the pool's
// stake. The ledger pays members (R − fee) pro rata on the epoch's stake
// snapshot; registered owners get their pro-rata part inside the leader
// reward, so the same split covers both.
func OwnerTake(r, margin, fixedAp3x, ownedShare float64) float64 {
	fee := OperatorTake(r, margin, fixedAp3x)
	return fee + (r-fee)*ownedShare
}

// OwnedStakeShare is the fraction of a pool's epoch stake snapshot that its
// owner controls: the registered owner/reward accounts plus the operator's
// own_stake addresses. 0 for an empty snapshot.
func OwnedStakeShare(stakes []EpochStake, ownStake []string) float64 {
	own := make(map[string]bool, len(ownStake))
	for _, a := range ownStake {
		own[a] = true
	}
	var total, owned int64
	for _, st := range stakes {
		total += st.Lovelace
		if st.PoolOwner || own[st.StakeAddress] {
			owned += st.Lovelace
		}
	}
	if total <= 0 {
		return 0
	}
	return float64(owned) / float64(total)
}

// rewardPerBlockEst averages a pool's reward per minted block (in AP3X, i.e.
// the 1e-6 base-unit strings from Blockfrost divided out) over its most recent
// completed epochs that actually produced blocks. 0 when no usable history.
func rewardPerBlockEst(hist []blockfrost.PoolHistory, currentEpoch int) float64 {
	var rewards, blocks float64
	used := 0
	for _, h := range hist {
		if h.Epoch >= currentEpoch || h.Blocks == 0 {
			continue
		}
		r, err := strconv.ParseFloat(h.Rewards, 64)
		if err != nil || r <= 0 {
			continue
		}
		rewards += r
		blocks += float64(h.Blocks)
		if used++; used == 3 {
			break
		}
	}
	if blocks == 0 {
		return 0
	}
	return rewards / 1e6 / blocks
}

// ensureRewardEstimates fills RewardPerBlockEst for current-epoch schedule
// rows that lack it (pools computed before this feature existed, or whose
// history fetch failed). One Blockfrost call per missing pool per tick;
// no-ops once populated.
func (s *Service) ensureRewardEstimates(ctx context.Context, currentEpoch int) {
	for _, pool := range s.Pools() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		ps, found, err := s.store.GetPoolSchedule(pool.PoolID, currentEpoch)
		if err != nil || !found {
			continue
		}
		needRPB := ps.RewardPerBlockEst == 0
		needFees := ps.Margin == 0 && ps.FixedCostAp3x == 0
		if !needRPB && !needFees {
			continue
		}
		changed := false
		if needRPB {
			cctx, cancel := callCtx(ctx)
			hist, err := s.api.PoolHistory(cctx, pool.PoolID, historyQuery())
			cancel()
			if err == nil {
				if est := rewardPerBlockEst(hist, currentEpoch); est > 0 {
					ps.RewardPerBlockEst = est
					changed = true
				}
			}
		}
		if needFees {
			cctx, cancel := callCtx(ctx)
			poolInfo, err := s.api.Pool(cctx, pool.PoolID)
			cancel()
			if err == nil {
				ps.Margin, ps.FixedCostAp3x = feeParams(poolInfo)
				changed = ps.Margin > 0 || ps.FixedCostAp3x > 0 || changed
			}
		}
		if changed {
			if err := s.store.SavePoolSchedule(&ps); err != nil {
				s.logger.Printf("warn: failed to save reward estimate for %s: %v", pool.Name, err)
			}
		}
	}
}

// ensureStakeSnapshots stores each pool's active-stake snapshot for the epoch
// (per delegator, registered owner/reward accounts flagged) once. Two
// Blockfrost lookups per pool per epoch; failures retry on the next tick.
func (s *Service) ensureStakeSnapshots(ctx context.Context, epoch int) {
	for _, pool := range s.Pools() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if has, err := s.store.HasEpochStakes(pool.PoolID, epoch); err != nil || has {
			continue
		}
		cctx, cancel := callCtx(ctx)
		info, err := s.api.Pool(cctx, pool.PoolID)
		cancel()
		if err != nil {
			s.logger.Printf("warn: pool lookup for stake snapshot of %s failed: %v", pool.Name, err)
			continue
		}
		owners := map[string]bool{info.RewardAccount: true}
		for _, o := range info.Owners {
			owners[o] = true
		}
		var rows []EpochStake
		failed := false
		for page := 1; ; page++ {
			cctx, cancel := callCtx(ctx)
			chunk, err := s.api.EpochStakeDistributionByPool(cctx, epoch, pool.PoolID, blockfrost.APIQueryParams{Count: 100, Page: page})
			cancel()
			if err != nil {
				s.logger.Printf("warn: epoch %d stake snapshot for %s failed: %v", epoch, pool.Name, err)
				failed = true
				break
			}
			for _, c := range chunk {
				amount, err := strconv.ParseInt(c.Amount, 10, 64)
				if err != nil {
					s.logger.Printf("warn: bad stake amount %q for %s in %s", c.Amount, c.StakeAddress, pool.Name)
					failed = true
					break
				}
				rows = append(rows, EpochStake{
					PoolID: pool.PoolID, Epoch: epoch, StakeAddress: c.StakeAddress,
					Lovelace: amount, PoolOwner: owners[c.StakeAddress],
				})
			}
			if failed || len(chunk) < 100 {
				break
			}
		}
		if failed || len(rows) == 0 {
			continue
		}
		if err := s.store.ReplaceEpochStakes(pool.PoolID, epoch, rows); err != nil {
			s.logger.Printf("warn: failed to save stake snapshot for %s: %v", pool.Name, err)
		}
	}
}

// maybeBackfill seeds a first-seen pool's history with actuals-only summaries
// for pre-launch epochs (schedules cannot be computed retroactively, so these
// rows carry produced counts and stake-based expectations only).
func (s *Service) maybeBackfill(ctx context.Context, poolID string, currentEpoch int) {
	has, err := s.store.HasAnySummary(poolID)
	if err != nil || has {
		return
	}
	cctx, cancel := callCtx(ctx)
	hist, err := s.api.PoolHistory(cctx, poolID, historyQuery())
	cancel()
	if err != nil {
		s.logger.Printf("warn: backfill history failed for %s: %v", poolID, err)
		return
	}
	added := 0
	for _, h := range hist {
		if h.Epoch >= currentEpoch {
			continue
		}
		if added >= s.set.BackfillEpochs {
			break
		}
		// Never stomp an epoch the schedule pipeline tracks: actuals-only rows
		// are strictly for pre-launch epochs.
		if _, tracked, err := s.store.GetPoolSchedule(poolID, h.Epoch); err != nil || tracked {
			continue
		}
		es := EpochSummary{
			PoolID:         poolID,
			Epoch:          h.Epoch,
			Produced:       h.Blocks,
			ExpectedBlocks: h.ActiveSize * float64(s.set.Net.EpochLength) * s.set.Net.ActiveSlotsCoeff,
			ActualsOnly:    true,
			Finalized:      true,
		}
		if err := s.store.SaveEpochSummary(&es); err != nil {
			s.logger.Printf("warn: backfill save failed for %s/%d: %v", poolID, h.Epoch, err)
			continue
		}
		added++
	}
	if added > 0 {
		s.logger.Printf("backfilled %d actuals-only epochs for %s", added, poolID)
	}
}

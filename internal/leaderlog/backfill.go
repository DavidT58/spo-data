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

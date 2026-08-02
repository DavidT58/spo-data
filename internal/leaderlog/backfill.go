package leaderlog

import (
	"context"

	"github.com/blockfrost/blockfrost-go"
)

// historyQuery covers the backfill window plus recent epochs in one call.
func historyQuery() blockfrost.APIQueryParams {
	return blockfrost.APIQueryParams{Count: 100, Order: "desc"}
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

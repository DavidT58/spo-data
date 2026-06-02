package monitor

import (
	"context"
	"fmt"
	"time"

	"spo-data/configs"
)

// blockAssessment is the computed block-production health of a single pool.
type blockAssessment struct {
	pool        configs.PoolConfig
	hasBlock    bool
	lastBlock   time.Time
	dry         time.Duration
	activeSize  float64
	expBlocks   float64 // expected blocks per epoch
	expGap      time.Duration
	threshold   time.Duration
	monitorable bool // expBlocks >= 1/epoch, so block production is a usable signal
	inViolation bool
}

// assessBlock computes the adaptive dry-spell threshold for a pool and whether
// it is currently in violation. d is the current decentralisation parameter.
func (m *Monitor) assessBlock(ctx context.Context, pool configs.PoolConfig, d float64) (blockAssessment, error) {
	a := blockAssessment{pool: pool}

	block, ok, err := m.latestBlock(ctx, pool.PoolID)
	if err != nil {
		return a, err
	}
	a.hasBlock = ok
	if ok {
		a.lastBlock = time.Unix(int64(block.Time), 0)
		a.dry = time.Since(a.lastBlock)
	}

	poolInfo, err := m.client.Pool(ctx, pool.PoolID)
	if err != nil {
		return a, err
	}
	a.activeSize = poolInfo.ActiveSize

	// Expected blocks this pool produces per epoch = (network blocks/epoch) x
	// (pool's stake fraction) x (1 - decentralisation). Network blocks/epoch =
	// EpochLength * ActiveSlotsCoeff (e.g. 432000 * 0.05 = 21600 on Prime).
	a.expBlocks = float64(m.set.Net.EpochLength) * m.set.Net.ActiveSlotsCoeff * a.activeSize * (1 - d)
	a.monitorable = a.expBlocks >= m.set.MinBlocksEpoch

	floorSec := m.set.Floor.Seconds()
	capSec := m.set.Cap.Seconds()
	var thrSec float64
	if a.expBlocks <= 0 {
		thrSec = capSec
	} else {
		gapSec := m.set.Net.EpochSeconds() / a.expBlocks
		a.expGap = time.Duration(gapSec * float64(time.Second))
		thrSec = m.set.K * gapSec
	}
	if thrSec < floorSec {
		thrSec = floorSec
	}
	if thrSec > capSec {
		thrSec = capSec
	}
	a.threshold = time.Duration(thrSec * float64(time.Second))

	a.inViolation = a.hasBlock && a.dry > a.threshold
	return a, nil
}

// checkBlocks runs the adaptive block-production check across all pools.
func (m *Monitor) checkBlocks(ctx context.Context, firstPass bool) {
	cctx, cancel := callCtx(ctx)
	d := m.decentralisation(cctx)
	cancel()

	for _, pool := range m.cfg.Pools {
		select {
		case <-ctx.Done():
			return
		default:
		}

		cctx, cancel := callCtx(ctx)
		a, err := m.assessBlock(cctx, pool, d)
		cancel()
		if err != nil {
			m.logger.Printf("warn: block check failed for %s (%s): %v", pool.Name, pool.Operator, err)
			continue
		}

		if !a.hasBlock {
			if firstPass {
				m.logger.Printf("info: %s (%s) has never produced a block — skipping block monitoring", pool.Name, pool.Operator)
			}
			continue
		}
		// Pools below the monitorability floor (≈0 stake, retired) produce so
		// rarely that a dry-spell alert is noise, not signal. Skip them entirely
		// rather than firing at the cap.
		if !a.monitorable {
			if firstPass {
				m.logger.Printf("info: %s (%s) produces ~%.2f blocks/epoch (< %.2f floor) — block alerts disabled for this pool",
					pool.Name, pool.Operator, a.expBlocks, m.set.MinBlocksEpoch)
			}
			continue
		}

		alertMsg := fmt.Sprintf("⛔ *%s* (%s) — no block in %s (threshold %s, ~%.1f blk/epoch)",
			pool.Name, pool.Operator, fmtDuration(a.dry), fmtDuration(a.threshold), a.expBlocks)
		reminderMsg := fmt.Sprintf("⏰ *%s* (%s) — still no block: %s dry (threshold %s)",
			pool.Name, pool.Operator, fmtDuration(a.dry), fmtDuration(a.threshold))
		recoveryMsg := fmt.Sprintf("✅ *%s* (%s) — producing again (last block %s ago)",
			pool.Name, pool.Operator, fmtDuration(a.dry))

		m.evaluateWithReminder(pool.PoolID, pool.Operator, pool.Name, a.inViolation, alertMsg, reminderMsg, recoveryMsg)
	}
}

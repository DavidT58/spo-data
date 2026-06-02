package monitor

import (
	"context"
	"fmt"
	"time"

	"spo-data/configs"
	"spo-data/internal/database"
	"spo-data/internal/models"

	"github.com/blockfrost/blockfrost-go"
)

// maxKesScan bounds the bootstrap back-scan. ~4096 blocks comfortably exceeds
// one KES lifetime of block production even for very high-output pools, so the
// last rotation is virtually always within the window.
const maxKesScan = 4096

// kesAssessment is the computed KES-expiry health of a single pool.
type kesAssessment struct {
	pool       configs.PoolConfig
	counter    string
	rotation   time.Time
	confidence string
	expiry     time.Time
	daysLeft   float64
	level      int // number of crossed alert tiers (0 == healthy)
}

// assessKES computes a pool's KES expiry estimate. It returns the assessment
// plus the (possibly updated) tracking state the caller should persist. The
// pool has no usable opcert data (never produced / missing counter) when the
// returned assessment has an empty counter.
func (m *Monitor) assessKES(ctx context.Context, pool configs.PoolConfig) (kesAssessment, models.PoolKesState, error) {
	a := kesAssessment{pool: pool}

	block, ok, err := m.latestBlock(ctx, pool.PoolID)
	if err != nil {
		return a, models.PoolKesState{}, err
	}
	if !ok || block.OPCertCounter == nil {
		return a, models.PoolKesState{}, nil // nothing to assess
	}
	currentCounter := *block.OPCertCounter
	a.counter = currentCounter

	state, found, err := database.GetKesState(pool.PoolID)
	if err != nil {
		m.logger.Printf("warn: GetKesState(%s) failed: %v", pool.Name, err)
	}

	switch {
	case !found || state.OpCertCounter == "":
		// Bootstrap: locate the block where the current counter first appeared.
		rotation, conf := m.findRotation(ctx, pool.PoolID, currentCounter, block)
		state = models.PoolKesState{
			PoolID:            pool.PoolID,
			OpCertCounter:     currentCounter,
			RotationBlockTime: rotation,
			Confidence:        conf,
		}
	case state.OpCertCounter != currentCounter:
		// Rotation observed since last check: the opcert was refreshed.
		state.OpCertCounter = currentCounter
		state.RotationBlockTime = time.Unix(int64(block.Time), 0)
		state.Confidence = models.KesConfidenceOK
	}

	a.rotation = state.RotationBlockTime
	a.confidence = state.Confidence
	a.expiry = state.RotationBlockTime.Add(m.set.Net.KesLifetime())
	a.daysLeft = time.Until(a.expiry).Hours() / 24
	a.level = tierLevel(a.daysLeft, m.set.KesThresholds)
	return a, state, nil
}

// findRotation binary-searches a pool's block history for the oldest block still
// carrying currentCounter — the moment the current opcert took effect. Counters
// are monotonic going back in time, so the partition (==current vs older) is
// clean. Returns the rotation time and a confidence: "ok" if an older counter
// was actually observed (true boundary), "lowerbound" if the counter never
// changed within the scan window (rotation is at least this old).
func (m *Monitor) findRotation(ctx context.Context, poolID, currentCounter string, newest blockfrost.Block) (time.Time, string) {
	getAt := func(idx int) (counter string, t int, ok bool) {
		page := idx/100 + 1 // Blockfrost pagination is 1-based
		off := idx % 100
		cc, cancel := callCtx(ctx)
		defer cancel()
		hashes, err := m.client.PoolBlocks(cc, poolID, blockfrost.APIQueryParams{Count: 100, Page: page, Order: "desc"})
		if err != nil || off >= len(hashes) {
			return "", 0, false
		}
		b, err := m.client.Block(cc, hashes[off])
		if err != nil || b.OPCertCounter == nil {
			return "", 0, false
		}
		return *b.OPCertCounter, b.Time, true
	}

	lo, hi := 0, maxKesScan-1
	bestTime := newest.Time
	sawOlder := false
	for lo <= hi {
		mid := (lo + hi) / 2
		counter, t, ok := getAt(mid)
		if !ok {
			hi = mid - 1 // ran past available history; search newer
			continue
		}
		if counter == currentCounter {
			bestTime = t // oldest block confirmed to carry the current counter so far
			lo = mid + 1
		} else {
			sawOlder = true
			hi = mid - 1
		}
	}

	conf := models.KesConfidenceLowerBound
	if sawOlder {
		conf = models.KesConfidenceOK
	}
	return time.Unix(int64(bestTime), 0), conf
}

// tierLevel returns how many alert tiers daysLeft has crossed; a smaller
// daysLeft satisfies more thresholds and yields a higher (more severe) level.
func tierLevel(daysLeft float64, thresholds []int) int {
	level := 0
	for _, th := range thresholds {
		if daysLeft <= float64(th) {
			level++
		}
	}
	return level
}

// checkKES runs the KES expiry heuristic across all pools.
func (m *Monitor) checkKES(ctx context.Context, firstPass bool) {
	for _, pool := range m.cfg.Pools {
		select {
		case <-ctx.Done():
			return
		default:
		}

		cctx, cancel := callCtx(ctx)
		a, state, err := m.assessKES(cctx, pool)
		cancel()
		if err != nil {
			m.logger.Printf("warn: KES check failed for %s (%s): %v", pool.Name, pool.Operator, err)
			continue
		}
		if a.counter == "" {
			if firstPass {
				m.logger.Printf("info: %s (%s) has no opcert data yet — skipping KES monitoring", pool.Name, pool.Operator)
			}
			continue
		}

		if err := database.SaveKesState(&state); err != nil {
			m.logger.Printf("warn: SaveKesState(%s) failed: %v", pool.Name, err)
		}

		alertMsg, recoveryMsg := kesMessages(pool, a)
		m.evaluateTiered(pool.PoolID, pool.Operator, pool.Name, a.level, alertMsg, recoveryMsg)
	}
}

// kesMessages builds the alert and recovery text for a KES assessment.
func kesMessages(pool configs.PoolConfig, a kesAssessment) (alertMsg, recoveryMsg string) {
	expiryDate := a.expiry.Format("2006-01-02")
	confNote := ""
	if a.confidence == models.KesConfidenceLowerBound {
		confNote = " [estimate uncertain — rotation predates scan window]"
	}

	if a.daysLeft < 0 {
		alertMsg = fmt.Sprintf("🔑 *%s* (%s) — KES likely EXPIRED (est. %s)%s — rotate the opcert now",
			pool.Name, pool.Operator, expiryDate, confNote)
	} else {
		alertMsg = fmt.Sprintf("🔑 *%s* (%s) — KES expires in ~%.1fd (est. %s)%s — rotate the opcert",
			pool.Name, pool.Operator, a.daysLeft, expiryDate, confNote)
	}
	recoveryMsg = fmt.Sprintf("✅ *%s* (%s) — KES rotated, now expires in ~%.0fd (est. %s)",
		pool.Name, pool.Operator, a.daysLeft, expiryDate)
	return alertMsg, recoveryMsg
}

package monitor

import (
	"context"
	"fmt"
)

// DryRun prints the resolved network constants, the current decentralisation
// parameter, and the per-pool block + KES assessment for every configured pool,
// without sending any Telegram message. It is the primary verification path.
func (m *Monitor) DryRun(ctx context.Context) {
	n := m.set.Net
	fmt.Println("=== Network constants in use (verified Apex Fusion Prime, NOT from /genesis) ===")
	fmt.Printf("MaxKesEvolutions=%d  SlotsPerKesPeriod=%d  SlotLength=%d  EpochLength=%d  ActiveSlotsCoeff=%.3f\n",
		n.MaxKesEvolutions, n.SlotsPerKesPeriod, n.SlotLength, n.EpochLength, n.ActiveSlotsCoeff)
	fmt.Printf("KES lifetime (conservative): %.2f days\n", n.KesLifetime().Hours()/24)

	cctx, cancel := callCtx(ctx)
	d := m.decentralisation(cctx)
	cancel()
	fmt.Printf("decentralisation_param (current): %.3f\n", d)
	fmt.Printf("Settings: K=%.1f floor=%s cap=%s reminder=%s kesTiers=%v\n\n",
		m.set.K, m.set.Floor, m.set.Cap, m.set.ReminderInterval, m.set.KesThresholds)

	fmt.Printf("%-8s %-10s %-12s %-9s %-9s %-9s  %s\n",
		"TICKER", "OPERATOR", "blk/epoch", "dry", "threshold", "monitor?", "KES")
	fmt.Println("----------------------------------------------------------------------------------------")

	for _, pool := range m.cfg.Pools {
		cctx, cancel := callCtx(ctx)
		ba, berr := m.assessBlock(cctx, pool, d)
		ka, _, kerr := m.assessKES(cctx, pool)
		cancel()

		blkEpoch := fmt.Sprintf("%.2f", ba.expBlocks)
		dry := "n/a"
		thr := fmtDuration(ba.threshold)
		mon := "yes"
		if berr != nil {
			blkEpoch, thr, mon = "ERR", "ERR", "ERR"
		} else {
			if ba.hasBlock {
				dry = fmtDuration(ba.dry)
			} else {
				dry = "never"
			}
			if !ba.monitorable {
				mon = "off" // below floor: block alerts disabled
			}
		}

		kes := "n/a"
		if kerr != nil {
			kes = "ERR"
		} else if ka.counter != "" {
			conf := ""
			if ka.confidence == "lowerbound" {
				conf = "~"
			}
			kes = fmt.Sprintf("ctr %s, ~%.0fd left%s (est %s)", ka.counter, ka.daysLeft, conf, ka.expiry.Format("2006-01-02"))
		}

		flag := ""
		if berr == nil && ba.inViolation && ba.monitorable {
			flag = "  <-- BLOCK ALERT"
		}
		if kerr == nil && ka.level > 0 {
			flag += "  <-- KES ALERT"
		}

		fmt.Printf("%-8s %-10s %-12s %-9s %-9s %-9s  %s%s\n",
			pool.Name, pool.Operator, blkEpoch, dry, thr, mon, kes, flag)
	}
}

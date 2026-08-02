package leaderlog

import (
	"context"
	"fmt"
	"os"
)

// DryRun verifies the full pipeline's preconditions without writing anything:
// cli probe, node tip, per-pool key presence and on-chain VRF hash match, and
// Blockfrost reachability. Exit summary is printed to stdout.
func (s *Service) DryRun(ctx context.Context) {
	fmt.Printf("== spo-dashboard dry run ==\n")
	fmt.Printf("cli=%s socket=%s\n", s.set.CLIPath, s.set.SocketPath)
	fmt.Printf("genesis=%s network=%v\n", s.set.GenesisPath, s.set.NetworkFlag)
	fmt.Printf("keys=%s\n", s.set.KeysDir)

	prefix, err := s.cli.Probe(ctx)
	if err != nil {
		fmt.Printf("FAIL cli probe: %v\n", err)
		return
	}
	fmt.Printf("ok   cli probe (prefix %q)\n", prefix)

	tip, err := s.cli.Tip(ctx)
	if err != nil {
		fmt.Printf("FAIL query tip: %v\n", err)
		return
	}
	fmt.Printf("ok   tip: epoch %d, slot %d, sync %s%%\n", tip.Epoch, tip.Slot, tip.SyncProgress)
	if !tip.Synced() {
		fmt.Printf("WARN node is not fully synced — schedule computation would be skipped\n")
	}

	s.reloadConfig()
	pools := s.Pools()
	fmt.Printf("ok   config: %d pools\n", len(pools))

	okN, warnN := 0, 0
	for _, pool := range pools {
		skeyPath, vkeyPath := KeyPaths(s.set.KeysDir, pool.PoolID)
		if _, err := os.Stat(skeyPath); err != nil {
			fmt.Printf("WARN %-6s (%s): vrf.skey missing (%s) — schedule unavailable\n", pool.Name, pool.Operator, skeyPath)
			warnN++
			continue
		}
		localHash, err := s.cli.VRFKeyHash(ctx, vkeyPath)
		if err != nil {
			fmt.Printf("WARN %-6s (%s): cannot hash vrf.vkey: %v\n", pool.Name, pool.Operator, err)
			warnN++
			continue
		}
		cctx, cancel := callCtx(ctx)
		poolInfo, err := s.api.Pool(cctx, pool.PoolID)
		cancel()
		if err != nil {
			fmt.Printf("WARN %-6s (%s): blockfrost lookup failed: %v\n", pool.Name, pool.Operator, err)
			warnN++
			continue
		}
		if poolInfo.VrfKey != "" && poolInfo.VrfKey != localHash {
			fmt.Printf("WARN %-6s (%s): VRF KEY STALE local=%s chain=%s\n", pool.Name, pool.Operator, localHash, poolInfo.VrfKey)
			warnN++
			continue
		}
		fmt.Printf("ok   %-6s (%s): key verified (%s…)\n", pool.Name, pool.Operator, localHash[:16])
		okN++
	}
	fmt.Printf("== dry run done: %d ok, %d warnings ==\n", okN, warnN)
}

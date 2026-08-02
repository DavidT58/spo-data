package leaderlog

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Tip is the subset of `cardano-cli query tip` output the daemon uses. The
// local node is the authoritative epoch/slot source — Blockfrost is only used
// for chain data, never for rollover detection. SlotInEpoch/SlotsToEpochEnd
// give the epoch's absolute-slot window directly; deriving it as
// epoch × EpochLength would be wrong on Prime, whose slot numbering includes a
// 2-epoch Byron prefix.
type Tip struct {
	Epoch           int    `json:"epoch"`
	Slot            int64  `json:"slot"`
	SlotInEpoch     int64  `json:"slotInEpoch"`
	SlotsToEpochEnd int64  `json:"slotsToEpochEnd"`
	SyncProgress    string `json:"syncProgress"`
}

// Synced reports whether the node is at (or effectively at) chain tip.
func (t Tip) Synced() bool {
	f, err := strconv.ParseFloat(t.SyncProgress, 64)
	return err == nil && f >= 99.9
}

// CLI wraps the cardano-cli binary. The expensive leadership-schedule runs are
// bounded by a semaphore (DASHBOARD_CLI_PARALLEL; measured 93 MB / ~1.6 min per
// run, so 2 concurrent runs are safe on the 2-core host — the systemd unit's
// Nice/CPUWeight keeps cardano-node prioritized). Cheap queries (tip, key
// hashing) run unbounded. Probe must complete before concurrent use: it
// resolves the subcommand prefix, which is read without locking afterwards.
type CLI struct {
	Bin         string
	Socket      string
	Genesis     string
	NetworkFlag []string
	Timeout     time.Duration

	sem    chan struct{}
	mu     sync.Mutex
	prefix []string // "" or "latest": resolved by Probe for 10.x-era binaries
	probed bool
}

func NewCLI(set Settings) *CLI {
	parallel := set.CLIParallel
	if parallel < 1 {
		parallel = 1
	}
	return &CLI{
		Bin:         set.CLIPath,
		Socket:      set.SocketPath,
		Genesis:     set.GenesisPath,
		NetworkFlag: set.NetworkFlag,
		Timeout:     set.CLITimeout,
		sem:         make(chan struct{}, parallel),
	}
}

// run executes cardano-cli with the resolved subcommand prefix and a bounded
// context. Stderr is captured into the returned error.
func (c *CLI) run(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	full := append(append([]string{}, c.prefix...), args...)
	cmd := exec.CommandContext(ctx, c.Bin, full...)
	cmd.Env = append(os.Environ(), "CARDANO_NODE_SOCKET_PATH="+c.Socket)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 400 {
			msg = msg[:400]
		}
		return nil, fmt.Errorf("cardano-cli %s: %w (%s)", strings.Join(full, " "), err, msg)
	}
	return out, nil
}

// Probe resolves the working subcommand prefix: plain `query` (8.x/9.x) or
// `latest query` (10.x, where the era-less top-level commands were removed).
func (c *CLI) Probe(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.probed {
		return strings.Join(c.prefix, " "), nil
	}
	for _, prefix := range [][]string{{}, {"latest"}} {
		c.prefix = prefix
		if _, err := c.run(ctx, 30*time.Second, "query", "leadership-schedule", "--help"); err == nil {
			c.probed = true
			return strings.Join(prefix, " "), nil
		}
	}
	c.prefix = nil
	return "", fmt.Errorf("cardano-cli at %s supports neither 'query leadership-schedule' nor 'latest query leadership-schedule'", c.Bin)
}

// Tip queries the local node's chain tip. Cheap; not semaphore-bounded.
func (c *CLI) Tip(ctx context.Context) (Tip, error) {
	args := append([]string{"query", "tip", "--socket-path", c.Socket}, c.NetworkFlag...)
	out, err := c.run(ctx, 60*time.Second, args...)
	if err != nil {
		return Tip{}, err
	}
	var t Tip
	if err := json.Unmarshal(out, &t); err != nil {
		return Tip{}, fmt.Errorf("parse query tip output: %w", err)
	}
	return t, nil
}

// LeadershipSchedule runs the (expensive) leadership-schedule query for one
// pool and returns its assigned slots for the current epoch. At most
// cap(c.sem) runs execute concurrently.
func (c *CLI) LeadershipSchedule(ctx context.Context, poolID, vrfSkeyPath string) ([]ScheduleEntry, error) {
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	tmp, err := os.CreateTemp("", "leaderlog-*.json")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	args := append([]string{"query", "leadership-schedule", "--socket-path", c.Socket}, c.NetworkFlag...)
	args = append(args,
		"--genesis", c.Genesis,
		"--stake-pool-id", poolID,
		"--vrf-signing-key-file", vrfSkeyPath,
		"--current",
		"--output-json",
		"--out-file", tmpPath,
	)
	stdout, err := c.run(ctx, c.Timeout, args...)
	if err != nil {
		return nil, err
	}

	data, rerr := os.ReadFile(tmpPath)
	if rerr == nil && len(strings.TrimSpace(string(data))) > 0 {
		return ParseScheduleJSON(data)
	}
	// Fallback: some cli versions print the schedule as a table on stdout.
	return ParseScheduleText(stdout)
}

// VRFKeyHash returns the hex hash of a VRF verification key file, as
// registered on-chain in the pool's certificate. Cheap; not semaphore-bounded.
func (c *CLI) VRFKeyHash(ctx context.Context, vkeyPath string) (string, error) {
	out, err := c.runRaw(ctx, 30*time.Second, "node", "key-hash-VRF", "--verification-key-file", vkeyPath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// runRaw executes without the query prefix (node subcommands are not era-scoped).
func (c *CLI) runRaw(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	cmd.Env = append(os.Environ(), "CARDANO_NODE_SOCKET_PATH="+c.Socket)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 400 {
			msg = msg[:400]
		}
		return nil, fmt.Errorf("cardano-cli %s: %w (%s)", strings.Join(args, " "), err, msg)
	}
	return out, nil
}

// KeyPaths returns the expected on-disk key locations for a pool.
func KeyPaths(keysDir, poolID string) (skey, vkey string) {
	return filepath.Join(keysDir, poolID+".vrf.skey"), filepath.Join(keysDir, poolID+".vrf.vkey")
}

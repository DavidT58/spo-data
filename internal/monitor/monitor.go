// Package monitor implements a long-lived watcher for Apex Fusion Prime stake
// pools. It polls a self-hosted Blockfrost instance and raises Telegram alerts
// when a pool stops producing blocks (adaptive, stake-weighted threshold) or
// when its KES operational certificate is approaching expiry (estimated from
// on-chain op_cert_counter changes).
package monitor

import (
	"context"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"spo-data/configs"
	"spo-data/internal/telegram"

	"github.com/blockfrost/blockfrost-go"
)

// NetworkParams are the immutable Apex Fusion Prime constants. Defaults are the
// values verified directly from the on-chain genesis file
// (.../genesis/shelley/genesis.json). They are intentionally NOT read from the
// Blockfrost /genesis endpoint, which on this backend returns Cardano-mainnet
// values (max_kes_evolutions=62, system_start=2017) and would overstate KES
// lifetime by 3 days.
type NetworkParams struct {
	MaxKesEvolutions  int     // Prime: 60
	SlotsPerKesPeriod int     // Prime: 129600
	SlotLength        int     // seconds per slot; Prime: 1
	EpochLength       int     // slots per epoch; Prime: 432000
	ActiveSlotsCoeff  float64 // Prime: 0.05
}

// KesLifetime is the conservative usable lifetime of a freshly issued opcert:
// (MaxKesEvolutions-1) KES periods. The -1 period absorbs the lag between
// issuance and the first block that carries the new counter, keeping estimates
// conservative (alert early, never late).
func (n NetworkParams) KesLifetime() time.Duration {
	periods := n.MaxKesEvolutions - 1
	if periods < 1 {
		periods = n.MaxKesEvolutions
	}
	secs := int64(periods) * int64(n.SlotsPerKesPeriod) * int64(n.SlotLength)
	return time.Duration(secs) * time.Second
}

// EpochSeconds is the wall-clock length of one epoch.
func (n NetworkParams) EpochSeconds() float64 {
	return float64(n.EpochLength) * float64(n.SlotLength)
}

// Settings holds the tunable monitor configuration, sourced from env.
type Settings struct {
	BlockInterval    time.Duration
	KesInterval      time.Duration
	ReminderInterval time.Duration
	Floor            time.Duration // never alert faster than this on a dry spell
	Cap              time.Duration // always alert by this dry spell, regardless of stake
	K                float64       // adaptive multiplier on expected inter-block gap
	MinBlocksEpoch   float64       // pools below this expected blocks/epoch get no block alerts
	KesThresholds    []int         // alert tiers in days, e.g. [14,7,2]
	Net              NetworkParams
}

// Monitor coordinates the polling loops.
type Monitor struct {
	cfg    *configs.Config
	client blockfrost.APIClient
	tg     *telegram.Client // nil disables sending (alerts are logged only)
	set    Settings
	logger *log.Logger

	mu          sync.Mutex
	cachedEpoch int
	cachedD     float64
	dValid      bool
}

// New builds a Monitor. tg may be nil (e.g. dry-run), in which case alerts are
// logged but not sent.
func New(cfg *configs.Config, client blockfrost.APIClient, tg *telegram.Client, set Settings, logger *log.Logger) *Monitor {
	if logger == nil {
		logger = log.Default()
	}
	return &Monitor{cfg: cfg, client: client, tg: tg, set: set, logger: logger}
}

// Run starts the block and KES loops and blocks until ctx is cancelled.
func (m *Monitor) Run(ctx context.Context) {
	m.logger.Printf("monitor starting: %d pools, block interval %s, kes interval %s, KES lifetime %.1fd",
		len(m.cfg.Pools), m.set.BlockInterval, m.set.KesInterval, m.set.Net.KesLifetime().Hours()/24)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		m.loop(ctx, m.set.BlockInterval, true, m.checkBlocks)
	}()
	go func() {
		defer wg.Done()
		m.loop(ctx, m.set.KesInterval, true, m.checkKES)
	}()

	wg.Wait()
	m.logger.Printf("monitor stopped")
}

// loop runs fn immediately (if runNow) and then every interval until ctx ends.
// firstPass is passed to fn only on the initial invocation.
func (m *Monitor) loop(ctx context.Context, interval time.Duration, runNow bool, fn func(ctx context.Context, firstPass bool)) {
	if runNow {
		fn(ctx, true)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fn(ctx, false)
		}
	}
}

// callCtx derives a bounded context for a single batch of API calls.
func callCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 30*time.Second)
}

// latestBlock fetches a pool's most recent block. hasBlock is false when the
// pool has never produced a block.
func (m *Monitor) latestBlock(ctx context.Context, poolID string) (block blockfrost.Block, hasBlock bool, err error) {
	hashes, err := m.client.PoolBlocks(ctx, poolID, blockfrost.APIQueryParams{Count: 1, Order: "desc"})
	if err != nil {
		return blockfrost.Block{}, false, err
	}
	if len(hashes) == 0 {
		return blockfrost.Block{}, false, nil
	}
	block, err = m.client.Block(ctx, hashes[0])
	if err != nil {
		return blockfrost.Block{}, false, err
	}
	return block, true, nil
}

// decentralisation returns the current decentralisation_param, cached per epoch.
// On Prime this is currently 0 (fully decentralized); the value future-proofs
// the adaptive calc against a possible return to federated block production. On
// error it returns 0 (treat as fully decentralized).
func (m *Monitor) decentralisation(ctx context.Context) float64 {
	epoch, err := m.client.EpochLatest(ctx)
	if err != nil {
		m.logger.Printf("warn: EpochLatest failed, assuming d=0: %v", err)
		return 0
	}

	m.mu.Lock()
	if m.dValid && m.cachedEpoch == epoch.Epoch {
		d := m.cachedD
		m.mu.Unlock()
		return d
	}
	m.mu.Unlock()

	params, err := m.client.EpochParameters(ctx, epoch.Epoch)
	if err != nil {
		m.logger.Printf("warn: EpochParameters(%d) failed, assuming d=0: %v", epoch.Epoch, err)
		return 0
	}
	d := float64(params.DecentralisationParam)

	m.mu.Lock()
	m.cachedEpoch = epoch.Epoch
	m.cachedD = d
	m.dValid = true
	m.mu.Unlock()
	return d
}

// --- env-backed settings loader ---

// LoadSettings reads monitor settings from the environment, applying the
// verified Prime defaults where unset.
func LoadSettings() Settings {
	return Settings{
		BlockInterval:    envDuration("MONITOR_BLOCK_INTERVAL", 10*time.Minute),
		KesInterval:      envDuration("MONITOR_KES_INTERVAL", 12*time.Hour),
		ReminderInterval: time.Duration(envFloat("MONITOR_REMINDER_HOURS", 12) * float64(time.Hour)),
		Floor:            time.Duration(envFloat("MONITOR_FLOOR_HOURS", 24) * float64(time.Hour)),
		Cap:              time.Duration(envFloat("MONITOR_CAP_HOURS", 336) * float64(time.Hour)),
		K:                envFloat("MONITOR_K", 3),
		MinBlocksEpoch:   envFloat("MONITOR_MIN_BLOCKS_EPOCH", 1.0),
		KesThresholds:    envIntList("MONITOR_KES_THRESHOLDS", []int{14, 7, 2}),
		Net: NetworkParams{
			MaxKesEvolutions:  envInt("PRIME_MAX_KES_EVOLUTIONS", 60),
			SlotsPerKesPeriod: envInt("PRIME_SLOTS_PER_KES_PERIOD", 129600),
			SlotLength:        envInt("PRIME_SLOT_LENGTH", 1),
			EpochLength:       envInt("PRIME_EPOCH_LENGTH", 432000),
			ActiveSlotsCoeff:  envFloat("PRIME_ACTIVE_SLOTS_COEFF", 0.05),
		},
	}
}

func envDuration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("warn: invalid %s=%q, using default %s", key, v, def)
		return def
	}
	return d
}

func envFloat(key string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		log.Printf("warn: invalid %s=%q, using default %v", key, v, def)
		return def
	}
	return f
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("warn: invalid %s=%q, using default %d", key, v, def)
		return def
	}
	return i
}

func envIntList(key string, def []int) []int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	parts := strings.Split(v, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			log.Printf("warn: invalid entry %q in %s, using default %v", p, key, def)
			return def
		}
		out = append(out, n)
	}
	return out
}

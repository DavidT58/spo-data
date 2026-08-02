// Package leaderlog computes per-epoch leadership schedules for every
// configured pool (cardano-cli + the pool's VRF signing key) and reconciles
// each scheduled slot against the chain via Blockfrost. It is display-only:
// results land in its own SQLite database for internal/dashweb to serve, and
// no alerts are ever sent from here (spo-monitor owns alerting).
package leaderlog

import (
	"context"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"spo-data/configs"

	"github.com/blockfrost/blockfrost-go"
)

// chainAPI is the narrow slice of blockfrost.APIClient the daemon uses,
// extracted so tests can stub it.
type chainAPI interface {
	Pool(ctx context.Context, poolID string) (blockfrost.Pool, error)
	PoolHistory(ctx context.Context, poolID string, query blockfrost.APIQueryParams) ([]blockfrost.PoolHistory, error)
	EpochBlockDistributionByPool(ctx context.Context, epochNumber int, poolID string, query blockfrost.APIQueryParams) ([]string, error)
	Block(ctx context.Context, hashOrNumber string) (blockfrost.Block, error)
	BlockBySlot(ctx context.Context, slotNumber int) (blockfrost.Block, error)
	BlockLatest(ctx context.Context) (blockfrost.Block, error)
}

// cliAPI abstracts the cardano-cli wrapper for tests.
type cliAPI interface {
	Probe(ctx context.Context) (string, error)
	Tip(ctx context.Context) (Tip, error)
	LeadershipSchedule(ctx context.Context, poolID, vrfSkeyPath string) ([]ScheduleEntry, error)
	VRFKeyHash(ctx context.Context, vkeyPath string) (string, error)
}

// Service runs the scheduler and reconciler loops.
type Service struct {
	store  *Store
	api    chainAPI
	cli    cliAPI
	set    Settings
	logger *log.Logger

	configDir  string
	singlePool string // when set, restrict all work to this pool (staged rollout)

	// recheckedKeys flips after the first full catch-up pass: key_missing /
	// key_stale pools are re-verified once per daemon start, then left alone
	// until the next epoch.
	recheckedKeys atomic.Bool

	mu    sync.RWMutex
	pools []configs.PoolConfig
}

// New builds a Service.
func New(store *Store, api chainAPI, cli cliAPI, set Settings, configDir, singlePool string, logger *log.Logger) *Service {
	if logger == nil {
		logger = log.Default()
	}
	return &Service{
		store:      store,
		api:        api,
		cli:        cli,
		set:        set,
		logger:     logger,
		configDir:  configDir,
		singlePool: singlePool,
	}
}

// Pools returns the current pool list snapshot.
func (s *Service) Pools() []configs.PoolConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]configs.PoolConfig, len(s.pools))
	copy(out, s.pools)
	return out
}

// reloadConfig re-reads the config dir. Errors keep the previous pool list so
// a transiently unreadable config never empties the dashboard.
func (s *Service) reloadConfig() {
	cfg, skipped, err := configs.LoadAllConfigs(s.configDir)
	if err != nil {
		s.logger.Printf("warn: config reload failed, keeping %d pools: %v", len(s.Pools()), err)
		return
	}
	for _, sk := range skipped {
		s.logger.Printf("warn: skipped unparseable config %s/%s", s.configDir, sk)
	}
	pools := cfg.Pools
	if s.singlePool != "" {
		pools = nil
		for _, p := range cfg.Pools {
			if p.PoolID == s.singlePool {
				pools = append(pools, p)
			}
		}
	}
	s.mu.Lock()
	s.pools = pools
	s.mu.Unlock()
}

// Run starts both loops and blocks until ctx is cancelled. computeNow is a
// deprecated no-op kept for CLI compatibility: the first scheduler tick always
// computes any missing schedules for the current epoch.
func (s *Service) Run(ctx context.Context, computeNow bool) {
	prefix, err := s.cli.Probe(ctx)
	if err != nil {
		s.logger.Printf("error: %v", err)
		return
	}
	s.logger.Printf("leaderlog starting: cli prefix %q, reconcile every %s, epoch poll %s",
		prefix, s.set.ReconcileInterval, s.set.EpochPoll)

	s.reloadConfig()
	_ = computeNow // the first scheduler tick always runs the catch-up pass

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.loop(ctx, s.set.EpochPoll, s.schedulerTick)
	}()
	go func() {
		defer wg.Done()
		s.loop(ctx, s.set.ReconcileInterval, s.reconcileTick)
	}()
	wg.Wait()
	s.logger.Printf("leaderlog stopped")
}

func (s *Service) loop(ctx context.Context, interval time.Duration, fn func(ctx context.Context)) {
	fn(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fn(ctx)
		}
	}
}

// callCtx bounds one batch of Blockfrost calls, mirroring internal/monitor.
func callCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 30*time.Second)
}

func isNotFound(err error) bool {
	var apiErr *blockfrost.APIError
	if errors.As(err, &apiErr) {
		_, ok := apiErr.Response.(blockfrost.NotFound)
		return ok
	}
	return false
}

// Health is served by /api/health.
type Health struct {
	LastSchedulerTick time.Time `json:"last_scheduler_tick"`
	LastReconcileTick time.Time `json:"last_reconcile_tick"`
	CurrentEpoch      int       `json:"current_epoch"`
	TipSlot           int64     `json:"tip_slot"`
	NodeSynced        bool      `json:"node_synced"`
	PoolCount         int       `json:"pool_count"`
}

var (
	healthMu sync.RWMutex
	health   Health
)

func setHealth(fn func(h *Health)) {
	healthMu.Lock()
	defer healthMu.Unlock()
	fn(&health)
}

// GetHealth returns a snapshot of the daemon's liveness state.
func GetHealth() Health {
	healthMu.RLock()
	defer healthMu.RUnlock()
	return health
}

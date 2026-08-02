// spo-dashboard is the block-production dashboard daemon: it computes each
// configured pool's leadership schedule once per epoch (cardano-cli + the
// pool's VRF signing key), reconciles scheduled slots against the chain every
// few minutes, and serves the results over HTTP on localhost. Display-only —
// alerting stays in spo-monitor.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"spo-data/configs"
	"spo-data/internal/dashweb"
	"spo-data/internal/leaderlog"

	"github.com/blockfrost/blockfrost-go"
)

func main() {
	var (
		configDir  = flag.String("config-dir", envOr("DASHBOARD_CONFIG_DIR", "c"), "Directory of per-operator YAML config files")
		dbPath     = flag.String("db", envOr("DASHBOARD_DB", "dashboard.db"), "Path to the dashboard SQLite database")
		listen     = flag.String("listen", "", "Listen address (overrides DASHBOARD_LISTEN)")
		dryRun     = flag.Bool("dry-run", false, "Verify cli, node, keys and Blockfrost per pool, then exit (no writes)")
		singlePool = flag.String("pool", "", "Restrict all work to one pool (bech32 id) — staged rollout")
		computeNow = flag.Bool("compute-now", false, "Deprecated no-op: the first scheduler tick always computes missing schedules")
	)
	flag.Parse()

	logger := log.New(os.Stdout, "", log.LstdFlags|log.LUTC)
	set := leaderlog.LoadSettings()
	if *listen != "" {
		set.Listen = *listen
	}

	// The dashboard talks to exactly one Blockfrost instance:
	// DASHBOARD_BLOCKFROST_URL when set, else the merged config's address.
	cfg, skipped, err := configs.LoadAllConfigs(*configDir)
	if err != nil {
		logger.Fatalf("failed to load configs from %q: %v", *configDir, err)
	}
	for _, s := range skipped {
		logger.Printf("warn: skipped unparseable config %s/%s", *configDir, s)
	}
	bfURL := set.BlockfrostURL
	if bfURL == "" {
		bfURL = strings.TrimRight(cfg.BlockFrostAddress, "/")
	}
	logger.Printf("loaded %d pools from %q (blockfrost: %s)", len(cfg.Pools), *configDir, bfURL)

	client := blockfrost.NewAPIClient(blockfrost.APIClientOptions{Server: bfURL})
	cli := leaderlog.NewCLI(set)

	if *dryRun {
		// Dry run needs no database at all.
		svc := leaderlog.New(nil, client, cli, set, *configDir, *singlePool, logger)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		svc.DryRun(ctx)
		return
	}

	store, err := leaderlog.OpenStore(*dbPath)
	if err != nil {
		logger.Fatalf("failed to open database %q: %v", *dbPath, err)
	}

	svc := leaderlog.New(store, client, cli, set, *configDir, *singlePool, logger)

	server := dashweb.NewServer(store, svc.Pools, set, bfURL, logger)
	httpSrv := &http.Server{Addr: set.Listen, Handler: server.Handler()}
	go func() {
		logger.Printf("http listening on %s", set.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("http server failed: %v", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	svc.Run(ctx, *computeNow)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

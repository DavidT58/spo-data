package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"spo-data/configs"
	"spo-data/internal/database"
	"spo-data/internal/monitor"
	"spo-data/internal/telegram"

	"github.com/blockfrost/blockfrost-go"
)

func main() {
	var (
		configDir = flag.String("config-dir", envOr("MONITOR_CONFIG_DIR", "c"), "Directory of per-operator YAML config files")
		dbPath    = flag.String("db", envOr("MONITOR_DB", "data.db"), "Path to the SQLite state database")
		dryRun    = flag.Bool("dry-run", false, "Print per-pool assessment and exit (no alerts, no state writes)")
		testAlert = flag.Bool("test-alert", false, "Send one canned Telegram message to verify delivery, then exit")
	)
	flag.Parse()

	logger := log.New(os.Stdout, "", log.LstdFlags|log.LUTC)

	cfg, skipped, err := configs.LoadAllConfigs(*configDir)
	if err != nil {
		logger.Fatalf("failed to load configs from %q: %v", *configDir, err)
	}
	for _, s := range skipped {
		logger.Printf("warn: skipped unparseable config %s/%s", *configDir, s)
	}
	logger.Printf("loaded %d pools across operators from %q (blockfrost: %s)",
		len(cfg.Pools), *configDir, cfg.BlockFrostAddress)

	if err := database.Initialize(*dbPath); err != nil {
		logger.Fatalf("failed to initialize database %q: %v", *dbPath, err)
	}

	client := blockfrost.NewAPIClient(blockfrost.APIClientOptions{
		Server: cfg.BlockFrostAddress,
	})

	// Telegram is required for live monitoring and --test-alert, but optional
	// for --dry-run (which never sends).
	token := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	chatID := strings.TrimSpace(os.Getenv("TELEGRAM_CHAT_ID"))
	quietZone := envOr("MONITOR_QUIET_TIMEZONE", "Local")
	quietStart := strings.TrimSpace(os.Getenv("MONITOR_QUIET_START"))
	quietEnd := strings.TrimSpace(os.Getenv("MONITOR_QUIET_END"))
	quietHours, err := telegram.NewQuietHours(quietStart, quietEnd, quietZone)
	if err != nil {
		logger.Fatalf("invalid quiet-hours configuration: %v", err)
	}
	if quietHours != nil {
		logger.Printf("telegram quiet hours: %s-%s (%s)", quietStart, quietEnd, quietZone)
	}
	var tg *telegram.Client
	if token != "" && chatID != "" {
		tg = telegram.NewClient(token, chatID)
		tg.QuietHours = quietHours
	}

	if *testAlert {
		if tg == nil {
			logger.Fatal("TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID must be set for --test-alert")
		}
		if err := tg.Send("✅ *spo-monitor* test alert — Telegram delivery is working."); err != nil {
			logger.Fatalf("test alert failed: %v", err)
		}
		logger.Printf("test alert sent successfully")
		return
	}

	set := monitor.LoadSettings()
	m := monitor.New(&cfg, client, tg, set, logger)

	if *dryRun {
		m.DryRun(context.Background())
		return
	}

	if tg == nil {
		logger.Fatal("TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID must be set to run the monitor (use --dry-run to test without them)")
	}

	// Surface skipped configs to Telegram so a silent parse error in one
	// operator file is noticed instead of quietly dropping those pools.
	if len(skipped) > 0 {
		msg := fmt.Sprintf("⚠️ spo-monitor started but skipped %d unparseable config file(s):\n%s",
			len(skipped), strings.Join(skipped, "\n"))
		if err := tg.Send(msg); err != nil {
			logger.Printf("warn: failed to send skipped-config warning: %v", err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	m.Run(ctx)
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

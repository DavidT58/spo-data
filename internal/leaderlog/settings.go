package leaderlog

import (
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"spo-data/internal/monitor"
)

// Settings holds the dashboard daemon's tunables, sourced from DASHBOARD_* env
// vars with defaults matching the fr-0 deployment.
type Settings struct {
	Listen        string
	CLIPath       string
	SocketPath    string
	GenesisPath   string
	NetworkFlag   []string // e.g. ["--mainnet"] or ["--testnet-magic", "N"]
	KeysDir       string
	BlockfrostURL string // when set, overrides the config files' blockfrost_address

	EpochPoll         time.Duration // scheduler tick (epoch detection + catch-up)
	ReconcileInterval time.Duration
	CLITimeout        time.Duration // per leadership-schedule run
	CLIGap            time.Duration // pause between per-pool cli runs
	CLIParallel       int           // concurrent leadership-schedule runs (2-core host: 2)
	SettleMarginSlots int64         // slots behind tip before a pending slot settles
	MaxAttempts       int           // schedule computation attempts per pool per epoch
	BackfillEpochs    int           // pre-launch epochs of actuals to backfill

	Net monitor.NetworkParams
}

// LoadSettings reads DASHBOARD_* env vars. Network constants come from the
// monitor package (same PRIME_* overrides as spo-monitor).
func LoadSettings() Settings {
	networkFlag := strings.Fields(envOr("DASHBOARD_NETWORK_FLAG", "--mainnet"))
	return Settings{
		Listen:        envOr("DASHBOARD_LISTEN", "127.0.0.1:8090"),
		CLIPath:       envOr("DASHBOARD_CLI", "/opt/cardano/bin/cardano-cli"),
		SocketPath:    envOr("DASHBOARD_SOCKET", "/var/opt/apex/prime-mainnet/david-main-fr-0/node.sock"),
		GenesisPath:   envOr("DASHBOARD_GENESIS", "/etc/apex/prime-mainnet/nodes/configuration/genesis/shelley/genesis.json"),
		NetworkFlag:   networkFlag,
		KeysDir:       envOr("DASHBOARD_KEYS_DIR", "/opt/spo-data/keys"),
		BlockfrostURL: strings.TrimRight(envOr("DASHBOARD_BLOCKFROST_URL", ""), "/"),

		EpochPoll:         envDuration("DASHBOARD_EPOCH_POLL", 2*time.Minute),
		ReconcileInterval: envDuration("DASHBOARD_RECONCILE_INTERVAL", 15*time.Minute),
		CLITimeout:        envDuration("DASHBOARD_CLI_TIMEOUT", 15*time.Minute),
		CLIGap:            envDuration("DASHBOARD_CLI_GAP", 5*time.Second),
		CLIParallel:       envInt("DASHBOARD_CLI_PARALLEL", 2),
		SettleMarginSlots: int64(envInt("DASHBOARD_SETTLE_MARGIN_SLOTS", 600)),
		MaxAttempts:       envInt("DASHBOARD_MAX_ATTEMPTS", 3),
		BackfillEpochs:    envInt("DASHBOARD_BACKFILL_EPOCHS", 10),

		Net: monitor.LoadSettings().Net,
	}
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
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

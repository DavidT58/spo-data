# spo-data

Tooling for Apex Fusion Prime stake-pool operators. Reads per-operator pool
lists from the `c/` directory and queries a self-hosted Blockfrost instance.

- `cmd/spo-data` — one-shot CLI (balance, blocks, unclaimed-rewards, rewards-history).
- `cmd/spo-monitor` — long-lived monitor that alerts to Telegram (below).

## spo-monitor

A long-lived daemon that watches every pool across all operator files in `c/`
and sends a Telegram alert when:

- **Block production stalls** — adaptive, stake-weighted. Each pool's threshold
  scales with its expected block rate (`expectedBlocks/epoch =
  EpochLength × ActiveSlotsCoeff × activeStakeFraction × (1 − d)`); it alerts
  when the dry spell exceeds `max(MONITOR_FLOOR_HOURS, K × expectedGap)`, capped
  at `MONITOR_CAP_HOURS`. This avoids false alarms on small pools that
  legitimately go days between blocks.
- **KES expiry approaches** — estimated from on-chain `op_cert_counter` changes
  (Blockfrost-only heuristic, ~±1.5 days). When a pool's counter last changed,
  the current opcert is assumed freshly issued and valid for
  `(MaxKesEvolutions − 1) × SlotsPerKesPeriod × SlotLength` ≈ **88.5 days** on
  Prime. Alerts fire at 14 / 7 / 2 days before the estimate.

Alerts use a per-`(pool, type)` state machine in SQLite: one alert on entering
the condition, reminders (block) or tier escalations (KES) while it persists,
and a recovery message when it clears.

### Important: Prime network constants

The Blockfrost `/genesis` endpoint on this backend returns **Cardano-mainnet**
values (`max_kes_evolutions: 62`, `system_start: 2017`), which are wrong for
Prime. The monitor therefore uses constants verified from the on-chain genesis
file (`maxKESEvolutions: 60`, etc.), overridable via `PRIME_*` env vars. The
**dynamic** Blockfrost data (epoch, stake, params, blocks, op_cert) is accurate
and is used directly.

### Configuration

Copy `.env.example` to `.env` and set `TELEGRAM_BOT_TOKEN` + `TELEGRAM_CHAT_ID`.
All other settings have sensible defaults (see `.env.example`).

### Build & run

```bash
# Build (CGO disabled — the SQLite driver is pure Go)
CGO_ENABLED=0 go build -o spo-monitor ./cmd/spo-monitor

# Verify config + connectivity + per-pool math, no alerts sent:
./spo-monitor --dry-run -config-dir c

# Verify Telegram delivery (needs the env vars), sends one message:
./spo-monitor --test-alert

# Run the daemon (foreground):
./spo-monitor -config-dir c -db data.db
```

### Deploy (systemd)

See `deploy/spo-monitor.service` for a sample unit (`Restart=always`,
`EnvironmentFile=`, journald logging). Adjust the paths, then:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now spo-monitor
journalctl -u spo-monitor -f
```

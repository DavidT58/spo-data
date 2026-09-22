# spo-data

Tooling for Apex Fusion Prime stake-pool operators. Reads per-operator pool
lists from the `c/` directory and queries a self-hosted Blockfrost instance.

- `cmd/spo-data` — one-shot CLI (balance, blocks, unclaimed-rewards, rewards-history).
- `cmd/spo-monitor` — long-lived monitor that alerts to Telegram (below).
- `cmd/spo-dashboard` — block-production dashboard: per-epoch leadership
  schedules + slot-level produced/missed reconciliation (below).

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

The sample enables daily Telegram quiet hours from **00:00 to 08:00 in
Europe/Belgrade**, including daylight-saving changes. Set `MONITOR_QUIET_START`,
`MONITOR_QUIET_END` (both `HH:MM`), and `MONITOR_QUIET_TIMEZONE` to change this.
Leave both times unset to disable quiet hours. An unset time zone uses the
server's local zone, so set it explicitly when the server uses UTC.

No Telegram messages are sent during this window, including reminders,
recoveries, startup warnings, and `--test-alert`. Monitoring continues.
Deferred pool alerts do not advance their notification state. The next normal
poll outside quiet hours reassesses the condition; it does not replay stale
overnight incidents. This is not a scheduled 08:00 delivery: block checks run
every 10 minutes and KES checks every 12 hours by default.

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

## spo-dashboard

A display-only daemon (alerting stays in spo-monitor) that answers precisely:
*was this pool scheduled to produce a block, and did the block land on-chain?*

- **Once per epoch** it runs `cardano-cli query leadership-schedule --current`
  for every configured pool against the local node, using the pool's VRF
  signing key from `DASHBOARD_KEYS_DIR` (`<poolID>.vrf.skey`). Before each run
  the local `vrf.vkey` hash is verified against the pool's on-chain
  registration (Blockfrost `/pools/{id}`) — a stale or missing key shows as
  "schedule unavailable" instead of producing a silently wrong schedule.
- **Every 15 minutes** it reconciles scheduled slots that have passed:
  `produced` (block on-chain), or `missed` (empty slot or occupied by another
  pool — the battle leader is recorded). Blocks a pool produced *outside* its
  computed schedule are flagged as anomalies (self-check of the pipeline).
  Transient Blockfrost errors never settle a slot — only a decoded 404 does.
- **History** accumulates from launch; ~10 pre-launch epochs of actuals are
  backfilled per pool (marked "actuals only" — schedules cannot be computed
  retroactively).
- The epoch/rollover signal is the **local node tip** (`query tip`), never
  Blockfrost, and computation is gated on the node being synced.

The UI (embedded, vanilla JS) serves an overview grouped by operator file plus
a per-pool drill-down (slot list + epoch history with luck %). Each operator
header and the tile row show the OPERATOR's estimated current-epoch take:
the pool reward estimate (produced blocks × avg reward per block over the last
3 completed epochs) fee-adjusted through the pool's registered parameters —
`min(R, fixed) + margin × max(0, R − fixed)` — priced via the LBank
`ap3x_usdt` ticker (cached 5 min server-side). Pools at 100% margin therefore
show the full pool reward. The daemon
listens on localhost; nginx with basic auth + Cloudflare (Flexible SSL) sits in
front — see `deploy/nginx-spo-dashboard.conf` and `deploy/cf-ips.sh`.

```bash
CGO_ENABLED=0 go build -o spo-dashboard ./cmd/spo-dashboard

# Verify cli + node + per-pool keys + Blockfrost, no writes:
./spo-dashboard --dry-run -config-dir c

# Staged rollout on one pool:
./spo-dashboard -pool pool1... -config-dir c -db dashboard.db

# Full daemon (systemd unit: deploy/spo-dashboard.service):
./spo-dashboard -config-dir c -db dashboard.db
```

Timing note: 45 pools × ~1.6 min/run ≈ 75 min of sequential cli time at each
epoch boundary (measured: 1:35 wall, 93 MB peak RSS per run on fr-0). The
systemd unit nice/idle-fences the daemon so cardano-node is never starved.

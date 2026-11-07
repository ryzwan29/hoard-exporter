# hoard-exporter

## Setup

1. Create an admin device token through the Hoard web dashboard (`Users` -> create a new token under an admin account), or via the shell:

   ```bash
   hoard-admin token create <admin-user> --device exporter
   ```

2. Create a `.env` file alongside your Compose file:

   ```env
   HOARD_EXPORTER_TOKEN=hoard_v1_xxxxxxxx
   ```

3. Add the contents of `docker-compose.snippet.yml` to your existing Hoard Compose stack. Adjust the `server` service name if yours is different.

4. Start the exporter:

   ```bash
   docker compose up -d --build hoard-exporter
   ```

5. Add the following scrape configuration to your Prometheus/VictoriaMetrics configuration:

   ```yaml
   - job_name: hoard
     static_configs:
       - targets: ["hoard-exporter:9477"]   # or IP:9477 if running on a different host
   ```

## Configuration (Environment Variables)

| Variable | Required | Default | Description |
|---|---|---|---|
| `HOARD_URL` | Yes | - | Base URL of hoard-server, e.g. `http://server:12421` |
| `HOARD_TOKEN` | Yes | - | Admin device token (`hoard_v1_...`) |
| `LISTEN_ADDR` | No | `:9477` | Address the exporter listens on |
| `SCRAPE_INTERVAL` | No | `15s` | Polling interval for hoard-server (not the Prometheus scrape interval — the exporter caches results to avoid hitting the database on every Prometheus scrape) |
| `HOARD_TIMEOUT` | No | `10s` | Timeout for each request to hoard-server |
| `HOARD_MINT_USER_TOKENS` | No | - | `true` = mint a `hoard-exporter` device token per user via the admin API, for per-game metrics (see below) |
| `HOARD_STATE_FILE` | No | `/data/hoard-exporter-tokens.json` | Where minted tokens are stored (mount `/data` as a volume) |
| `HOARD_USER_TOKENS` | No | - | Manual alternative: `alice=hoard_v1_xxx,bob=hoard_v1_yyy` |

## Per-game metrics

`/v1/me/overview` and `/v1/playtime` are scoped to the calling token's own user, so an admin token can't read other users' games. Per-game metrics need one token per user: either `HOARD_USER_TOKENS`, or `HOARD_MINT_USER_TOKENS=true` (admin token mints them; they show up as device `hoard-exporter` in each user's device list and can be revoked from the dashboard). Neither set = no per-game metrics, everything else works as before.

## Exposed Metrics

### From `/v1/health` (always available, public, stable)

- `hoard_up` — 1/0
- `hoard_build_info{version}` — Always 1, with the version exposed as a label
- `hoard_uptime_seconds`

### From `/v1/admin/overview` (requires an admin token, **internal/unstable**)

- `hoard_scrape_success` — 1/0, indicates whether the metrics below are valid
- `hoard_users_total`, `hoard_admins_total`
- `hoard_saves_total`, `hoard_versions_total`, `hoard_versions_trashed_total`
- `hoard_logical_bytes`, `hoard_stored_bytes`, `hoard_trash_bytes`
- `hoard_orphan_objects_total`, `hoard_orphan_bytes` — **Worth alerting on**: if these values keep increasing, the hoard-server cleanup sweep may not be working properly
- `hoard_objects_total`, `hoard_db_bytes`, `hoard_client_logs_total`
- `hoard_oldest_snapshot_timestamp_seconds`
- Per-user metrics, with `{username, is_admin}` labels:
  - `hoard_user_used_bytes`
  - `hoard_user_quota_bytes`
  - `hoard_user_stored_bytes`
  - `hoard_user_saves_total`
  - `hoard_user_versions_total`
  - `hoard_user_devices_total`
  - `hoard_user_last_seen_timestamp_seconds`

### Per-game (needs per-user tokens, see above)

- `hoard_user_game_scrape_success{username}` — 1/0
- `hoard_user_playtime_seconds{username}`, `hoard_user_games_total{username}`
- Labels `{username, game, game_name}`:
  - `hoard_user_game_playtime_seconds`
  - `hoard_user_game_saves`, `hoard_user_game_versions`, `hoard_user_game_bytes`
  - `hoard_user_game_last_backup_timestamp_seconds`

```
# most played games (hours)
topk(10, sum by (game_name) (hoard_user_game_playtime_seconds)) / 3600
# players per game
count by (game_name) (hoard_user_game_playtime_seconds > 0)
# save data growth per game (24h)
topk(10, sum by (game_name) (delta(hoard_user_game_bytes[24h])))
```

Missing timestamps (`null` in JSON, e.g. for users who have never logged in) are represented as `-1` instead of `0`, so they cannot be confused with a real Unix epoch timestamp (January 1, 1970).

## Example PromQL Alerts

```promql
# Server down
up{job="hoard"} == 0 or hoard_up == 0

# Possible cleanup sweep failure
increase(hoard_orphan_objects_total[1h]) > 0 and hoard_orphan_objects_total > 100

# User approaching quota
hoard_user_quota_bytes > 0
  and (hoard_user_used_bytes / hoard_user_quota_bytes) > 0.9
```

## Development / Manual Build

```bash
go build -o hoard-exporter .
HOARD_URL=http://IP:12421 HOARD_TOKEN=hoard_v1_... ./hoard-exporter
```

There are no external dependencies (pure Go standard library), so a simple `go build` is all you need. No `go mod tidy` or internet access to download packages is required.

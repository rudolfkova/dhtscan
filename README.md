# dht-scan

*[Русская версия — README.ru.md](README.ru.md)*

A tiny, self-contained probe that tells you whether a host (any VPS) can talk to
TON storage providers over **DHT + ADNL/RLDP** — i.e. whether
`mytonstorage-backend`'s `RequestStorageInfo` notify path would work from here.

It replicates `tonutils-storage-provider/pkg/transport.Client.connect` exactly:

1. **DHT FindValue** `"storage-provider"` record for each provider pubkey
2. **DHT FindAddresses** to resolve the ADNL UDP address
3. **ADNL/RLDP** session + `GetStorageRates` query

Provider pubkeys are pulled from the public coordinator API
(`POST https://mytonprovider.org/api/v1/providers/search`) — no DB or auth needed.

## Build & run (on the VPS)

```bash
git clone <repo> && cd dht-scan
cp .env.example .env        # then edit .env
go build -o dhtscan .
./dhtscan
```

## Configuration (env / .env)

Config is env-first (Docker-friendly). Priority: **CLI flag > env var > `.env` file > default**.
Copy `.env.example` to `.env` and edit. Real environment variables override the
`.env` file.

| Env var | Default | Meaning |
|---------|---------|---------|
| `DHTSCAN_HOST` | `https://mytonprovider.org` | coordinator/API host for `/api/v1/providers/search` |
| `DHTSCAN_LIMIT` | `20` | number of providers to fetch/probe |
| `DHTSCAN_UPTIME` | `50` | `uptime_gt_percent` filter |
| `DHTSCAN_PUBKEYS` | _(empty)_ | comma-separated hex pubkeys; overrides the API |
| `DHTSCAN_TON_CONFIG` | TON global config URL | network must match your providers |
| `DHTSCAN_UDP_PORT` | `16167` | local UDP port (`0` = random; use it if the port is taken) |
| `DHTSCAN_CONCURRENCY` | `8` | parallel probes |
| `DHTSCAN_TIMEOUT` | `10s` | per-provider timeout |
| `DHTSCAN_RLDP` | `true` | run RLDP `GetStorageRates` after DHT resolve |

```bash
# point at a test coordinator while prod is down:
echo 'DHTSCAN_HOST=http://my-test-box:8080' >> .env && ./dhtscan

# one-off override via real env or flags:
DHTSCAN_LIMIT=30 ./dhtscan
./dhtscan --port 0 --rldp=false
```

Every env var has a matching flag (`--host --limit --uptime --pubkeys --config
--port --concurrency --timeout --rldp`) that wins if passed.

## Output

Per-provider line shows each layer (`findvalue` / `findaddr` / `rldp`) with latency
and the resolved `ip:port`, then a summary and a **verdict**.

| Verdict | Meaning | Exit |
|---------|---------|------|
| `DHT-READY` | DHT resolve + RLDP work; notify path is fine here | 0 |
| `DEGRADED (partial DHT resolution)` | flaky UDP / packet loss / strict NAT | 1 |
| `DEGRADED (DHT resolves, RLDP unreachable)` | UDP to provider ports filtered, or providers down | 2 |
| `DHT NOT READY` | 0 providers resolved — outbound UDP/DHT blocked | 2 |
| `ENVIRONMENT BROKEN` | can't load config / bind UDP / fetch providers | 3 |

The verdict block prints which helper script to run next.

> Note: a few individual providers failing is normal (offline / not announced in
> DHT). The verdict looks at the aggregate, not single rows.

## Helper scripts (`scripts/`)

| Script | Type | What it does |
|--------|------|--------------|
| `check-egress-ip.sh` | read-only | local vs public IP, NAT guess |
| `net-udp-check.sh` | read-only | UDP egress smoke test, kernel/conntrack/firewall inspection |
| `open-adnl-firewall.sh` | **fix** (root) | opens UDP port in ufw/nft/iptables, allows UDP return traffic |

Typical flow: run `dhtscan` → if the verdict isn't `DHT-READY`, run the recommended
`scripts/*.sh` it points to, fix, then re-run `dhtscan`.

Remember: cloud hosts (Hetzner/AWS/GCP/Proxmox) usually have an **external**
firewall / security group that `open-adnl-firewall.sh` cannot touch — open
UDP there too.

# dht-scan

A tiny, self-contained probe that tells you whether a host (any VPS) can talk to
TON storage providers over **DHT + ADNL/RLDP** — i.e. whether
`mytonstorage-backend`'s `RequestStorageInfo` notify path would work from here.

It replicates `tonutils-storage-provider/pkg/transport.Client.connect` exactly:

1. **DHT FindValue** `"storage-provider"` record for each provider pubkey
2. **DHT FindAddresses** to resolve the ADNL UDP address
3. **ADNL/RLDP** session + `GetStorageRates` query

Provider pubkeys are pulled from the public coordinator API
(`POST https://mytonprovider.org/api/v1/providers/search`) — no DB or auth needed.

## Build

```bash
go build -o dhtscan .

# static build for shipping to a VPS:
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o dhtscan .
```

Then `scp dhtscan` (and the `scripts/` folder) to the target host and run it there.

## Run

```bash
./dhtscan                      # 20 top-rated providers, default UDP 16167
./dhtscan --limit 30           # probe more providers
./dhtscan --port 0             # random local UDP port
./dhtscan --rldp=false         # DHT resolve only, skip RLDP layer
./dhtscan --pubkeys aabb..,ccdd..   # probe specific pubkeys, skip the API
```

Flags: `--host --limit --uptime --pubkeys --config --port --concurrency --timeout --rldp`.

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

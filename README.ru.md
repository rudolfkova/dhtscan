# dht-scan

*[English version — README.md](README.md)*

Крохотная самодостаточная утилита, которая проверяет, может ли хост (любой VPS)
общаться с TON storage-провайдерами по **DHT + ADNL/RLDP** — то есть будет ли
с этой машины работать notify-путь `RequestStorageInfo` из `mytonstorage-backend`.

Утилита **один-в-один** повторяет `tonutils-storage-provider/pkg/transport.Client.connect`:

1. **DHT FindValue** записи `"storage-provider"` по pubkey провайдера
2. **DHT FindAddresses** — резолв ADNL UDP-адреса
3. **ADNL/RLDP** сессия + запрос `GetStorageRates`

Pubkey'и провайдеров берутся из публичного API координатора
(`POST https://mytonprovider.org/api/v1/providers/search`) — без БД и авторизации.

## Сборка и запуск (на VPS)

```bash
git clone <repo> && cd dht-scan
cp .env.example .env        # затем отредактируй .env
go build -o dhtscan .
./dhtscan
```

Если Go на VPS нет:

```bash
sudo apt update && sudo apt install -y golang-go
# либо свежий Go вручную с https://go.dev/dl/
```

## Конфигурация (env / .env)

Конфиг в первую очередь через окружение (удобно для Docker).
Приоритет: **CLI-флаг > переменная окружения > файл `.env` > значение по умолчанию**.
Скопируй `.env.example` в `.env` и поправь. Реальные переменные окружения
перебивают то, что задано в файле `.env`.

| Переменная | По умолчанию | Назначение |
|------------|--------------|------------|
| `DHTSCAN_HOST` | `https://mytonprovider.org` | хост координатора/API для `/api/v1/providers/search` |
| `DHTSCAN_LIMIT` | `20` | сколько провайдеров запросить и проверить |
| `DHTSCAN_UPTIME` | `50` | фильтр `uptime_gt_percent` |
| `DHTSCAN_PUBKEYS` | _(пусто)_ | список hex-pubkey через запятую; перебивает API |
| `DHTSCAN_TON_CONFIG` | URL TON global config | сеть должна совпадать с сетью провайдеров |
| `DHTSCAN_UDP_PORT` | `16167` | локальный UDP-порт (`0` = случайный; если порт занят) |
| `DHTSCAN_CONCURRENCY` | `8` | параллельных проверок |
| `DHTSCAN_TIMEOUT` | `10s` | таймаут на одного провайдера |
| `DHTSCAN_RLDP` | `true` | делать RLDP `GetStorageRates` после DHT-резолва |

```bash
# направить на тестовый координатор, пока прода нет:
echo 'DHTSCAN_HOST=http://my-test-box:8080' >> .env && ./dhtscan

# разовое переопределение через реальный env или флаги:
DHTSCAN_LIMIT=30 ./dhtscan
./dhtscan --port 0 --rldp=false
```

У каждой переменной есть парный флаг (`--host --limit --uptime --pubkeys --config
--port --concurrency --timeout --rldp`), который имеет приоритет, если передан.

В Docker можно вообще без файла — просто `-e DHTSCAN_HOST=...` (реальный env
перебивает `.env`).

## Вывод

Строка по каждому провайдеру показывает все слои (`findvalue` / `findaddr` / `rldp`)
с латентностью и резолвнутым `ip:port`, затем сводку и **вердикт**.

| Вердикт | Что значит | Exit |
|---------|------------|------|
| `DHT-READY` | DHT-резолв + RLDP работают; notify-путь отсюда жив | 0 |
| `DEGRADED (partial DHT resolution)` | флапающий UDP / потери пакетов / строгий NAT | 1 |
| `DEGRADED (DHT resolves, RLDP unreachable)` | UDP до портов провайдеров режется, либо провайдеры лежат | 2 |
| `DHT NOT READY` | 0 провайдеров зарезолвилось — исходящий UDP/DHT заблокирован | 2 |
| `ENVIRONMENT BROKEN` | не удалось загрузить config / забиндить UDP / получить список | 3 |

Блок вердикта печатает, какой вспомогательный скрипт запускать дальше.

> Примечание: падение нескольких отдельных провайдеров — это норма (оффлайн /
> не анонсированы в DHT). Вердикт смотрит на агрегат, а не на отдельные строки.

## Вспомогательные скрипты (`scripts/`)

| Скрипт | Тип | Что делает |
|--------|-----|------------|
| `check-egress-ip.sh` | read-only | локальный vs публичный IP, догадка про NAT |
| `net-udp-check.sh` | read-only | смоук-тест UDP-egress, осмотр ядра/conntrack/firewall |
| `open-adnl-firewall.sh` | **фикс** (root) | открывает UDP-порт в ufw/nft/iptables, разрешает обратный UDP-трафик |

Типичный сценарий: запустил `dhtscan` → если вердикт не `DHT-READY`, запускаешь
рекомендованные `scripts/*.sh`, чинишь, перезапускаешь `dhtscan`.

Помни: у облачных хостов (Hetzner/AWS/GCP/Proxmox) обычно есть **внешний**
firewall / security group, до которого `open-adnl-firewall.sh` не дотянется —
открывай UDP и там.

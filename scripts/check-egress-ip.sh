#!/usr/bin/env bash
# check-egress-ip.sh — read-only: show this host's local vs public IP and
# guess whether it sits behind NAT. Handy when dht-scan says providers don't
# resolve: ADNL/DHT behind symmetric NAT or a wrong advertised IP is a common cause.
set -uo pipefail

echo "=== Local addresses ==="
if command -v ip >/dev/null 2>&1; then
    ip -br addr 2>/dev/null || ip addr
else
    ifconfig 2>/dev/null || echo "no ip/ifconfig available"
fi

echo
echo "=== Default route ==="
ip route get 1.1.1.1 2>/dev/null || route -n 2>/dev/null || true

echo
echo "=== Public egress IP ==="
PUB=""
for url in "https://api.ipify.org" "https://ifconfig.me/ip" "https://icanhazip.com"; do
    PUB="$(curl -fsS --max-time 8 "$url" 2>/dev/null | tr -d '[:space:]')"
    [ -n "$PUB" ] && { echo "  $PUB   (via $url)"; break; }
done
[ -z "$PUB" ] && echo "  could not determine public IP (outbound HTTPS blocked?)"

echo
echo "=== NAT guess ==="
if [ -n "$PUB" ]; then
    if ip -br addr 2>/dev/null | grep -qw "$PUB"; then
        echo "  Public IP is bound directly to an interface -> likely NO NAT."
    else
        echo "  Public IP is NOT on any local interface -> host is behind NAT."
        echo "  Outbound DHT/ADNL still works through conntrack, but a strict/symmetric"
        echo "  NAT can drop return UDP. If dht-scan resolves 0 providers, suspect this."
    fi
fi

echo
echo "Done. If you suspect NAT/firewall, run: sudo bash scripts/net-udp-check.sh"

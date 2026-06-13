#!/usr/bin/env bash
# net-udp-check.sh — read-only diagnostics for UDP/DHT egress.
# Run this when dht-scan reports "DHT NOT READY" or "DEGRADED".
# It does NOT change anything; it only inspects the host.
#
# Usage: sudo bash scripts/net-udp-check.sh [ADNL_UDP_PORT]   (default 16167)
set -uo pipefail

PORT="${1:-16167}"
echo "########################################################################"
echo "# UDP / DHT egress diagnostics (port ${PORT})"
echo "########################################################################"

# 1) Can we send UDP out at all? Probe DNS (UDP/53) as a generic egress test.
echo
echo "=== 1. Outbound UDP smoke test (DNS over UDP/53) ==="
udp_ok=0
if command -v dig >/dev/null 2>&1; then
    if dig +time=3 +tries=1 @8.8.8.8 ton.org A >/dev/null 2>&1; then
        echo "  OK   UDP/53 to 8.8.8.8 works (dig)"; udp_ok=1
    else
        echo "  FAIL UDP/53 to 8.8.8.8 failed (dig) -> outbound UDP likely blocked"
    fi
elif command -v nc >/dev/null 2>&1; then
    if printf '' | nc -u -w3 8.8.8.8 53 >/dev/null 2>&1; then
        echo "  OK   UDP/53 reachable (nc)"; udp_ok=1
    else
        echo "  FAIL UDP/53 unreachable (nc)"
    fi
else
    echo "  SKIP no dig/nc installed (apt-get install dnsutils netcat-openbsd)"
fi
[ "$udp_ok" = 0 ] && echo "  >>> If this fails, DHT cannot work. Fix outbound UDP egress first."

# 2) Kernel UDP / socket settings.
echo
echo "=== 2. Kernel networking settings ==="
echo "  ephemeral port range : $(sysctl -n net.ipv4.ip_local_port_range 2>/dev/null || echo '?')"
echo "  rmem_max             : $(sysctl -n net.core.rmem_max 2>/dev/null || echo '?')"
echo "  wmem_max             : $(sysctl -n net.core.wmem_max 2>/dev/null || echo '?')"
echo "  netdev_max_backlog   : $(sysctl -n net.core.netdev_max_backlog 2>/dev/null || echo '?')"

# 3) conntrack (UDP return traffic depends on it).
echo
echo "=== 3. conntrack ==="
if [ -f /proc/sys/net/netfilter/nf_conntrack_count ]; then
    echo "  conntrack count/max  : $(cat /proc/sys/net/netfilter/nf_conntrack_count 2>/dev/null)/$(cat /proc/sys/net/netfilter/nf_conntrack_max 2>/dev/null)"
    echo "  udp timeout (stream) : $(sysctl -n net.netfilter.nf_conntrack_udp_timeout_stream 2>/dev/null || echo '?')s"
else
    echo "  nf_conntrack not loaded (ok on some minimal hosts)"
fi

# 4) Who is bound to the ADNL port locally.
echo
echo "=== 4. Local socket on UDP/${PORT} ==="
if command -v ss >/dev/null 2>&1; then
    ss -lunp 2>/dev/null | awk -v p=":${PORT}" 'NR==1 || index($0,p)' || true
else
    netstat -lunp 2>/dev/null | grep -E "(:${PORT}\b|Proto)" || true
fi

# 5) Firewall state (needs root to read rules).
echo
echo "=== 5. Firewall rules (UDP-relevant) ==="
if [ "$(id -u)" -ne 0 ]; then
    echo "  (run with sudo to inspect iptables/nft rules)"
fi
if command -v ufw >/dev/null 2>&1; then
    echo "  -- ufw status --"
    ufw status verbose 2>/dev/null | sed 's/^/    /' || echo "    (need root)"
fi
if command -v nft >/dev/null 2>&1; then
    echo "  -- nftables (udp lines) --"
    nft list ruleset 2>/dev/null | grep -iE "udp|policy|chain" | sed 's/^/    /' | head -40 || echo "    (need root / empty)"
fi
if command -v iptables >/dev/null 2>&1; then
    echo "  -- iptables INPUT/OUTPUT policy --"
    iptables -S 2>/dev/null | grep -E "^-P (INPUT|OUTPUT)|udp|${PORT}" | sed 's/^/    /' | head -40 || echo "    (need root)"
fi

echo
echo "########################################################################"
echo "# Interpretation"
echo "########################################################################"
echo "  - Step 1 FAIL  -> outbound UDP blocked. Allow egress UDP (provider/host"
echo "                    firewall, security group). Then: sudo bash scripts/open-adnl-firewall.sh ${PORT}"
echo "  - Step 1 OK but dht-scan still 0/N -> return path blocked by NAT/firewall."
echo "                    Open the ADNL port: sudo bash scripts/open-adnl-firewall.sh ${PORT}"
echo "  - All OK but degraded -> packet loss / strict NAT; re-run dht-scan with --limit 30."

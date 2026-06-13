#!/usr/bin/env bash
# open-adnl-firewall.sh — FIX script: open the ADNL UDP port and make sure
# outbound UDP is allowed, so DHT/ADNL can work. Idempotent and non-destructive
# (it only ADDs allow rules; it never flushes existing ones).
#
# Usage: sudo bash scripts/open-adnl-firewall.sh [ADNL_UDP_PORT]   (default 16167)
set -euo pipefail

PORT="${1:-16167}"

if [ "$(id -u)" -ne 0 ]; then
    echo "This script changes firewall rules and must run as root: sudo bash $0 ${PORT}" >&2
    exit 1
fi

echo "Opening UDP/${PORT} for ADNL/DHT ..."

changed=0

# Prefer ufw if it manages the firewall.
if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -qi "Status: active"; then
    echo "  ufw detected (active) -> allowing ${PORT}/udp"
    ufw allow "${PORT}/udp" || true
    changed=1
elif command -v nft >/dev/null 2>&1 && nft list tables 2>/dev/null | grep -q .; then
    echo "  nftables detected -> ensuring inet filter input rule for udp dport ${PORT}"
    nft list table inet filter >/dev/null 2>&1 || nft add table inet filter
    nft list chain inet filter input >/dev/null 2>&1 || \
        nft 'add chain inet filter input { type filter hook input priority 0; }'
    if ! nft list chain inet filter input 2>/dev/null | grep -q "udp dport ${PORT} accept"; then
        nft add rule inet filter input udp dport "${PORT}" accept
    fi
    changed=1
elif command -v iptables >/dev/null 2>&1; then
    echo "  iptables detected -> appending ACCEPT rules"
    # Inbound on the ADNL port.
    iptables -C INPUT -p udp --dport "${PORT}" -j ACCEPT 2>/dev/null || \
        iptables -A INPUT -p udp --dport "${PORT}" -j ACCEPT
    # Allow established/related UDP return traffic (DHT replies).
    iptables -C INPUT -p udp -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT 2>/dev/null || \
        iptables -A INPUT -p udp -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
    changed=1
else
    echo "  No managed firewall (ufw/nft/iptables) found as active."
    echo "  If this host is behind a cloud security group, open inbound+outbound UDP/${PORT} there."
fi

if [ "$changed" = 1 ]; then
    echo
    echo "Done. Note: cloud providers (AWS/GCP/Hetzner/Proxmox host) often have an"
    echo "EXTERNAL firewall / security group too — open UDP/${PORT} (in & out) there as well."
fi

echo
echo "Re-test with:  ./dhtscan --port ${PORT} --limit 20"

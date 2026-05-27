#!/usr/bin/env bash
# client-init.sh — One-time client-side setup for paqet (Linux only)
# Usage: sudo bash client-init.sh <SERVER_IP> <SERVER_PORT>
# Example: sudo bash client-init.sh 203.0.113.10 9999
#
# WHY this script exists
# ──────────────────────
# paqet injects raw TCP packets via pcap, completely bypassing the OS TCP
# stack.  When the server's raw packets arrive at the client, the client's
# kernel sees TCP frames for a port it doesn't own — so it immediately sends
# RST back to the server.
#
# This RST is harmless to paqet itself (both sides ignore it), BUT stateful
# NAT devices and firewalls between client and server track TCP state.  When
# they see the RST they tear down the NAT entry, which blocks all subsequent
# server→client packets.  Result: connection appears "established" (KCP
# handshake), but responses never arrive.
#
# This script drops the kernel-generated RST before it leaves the machine,
# preventing NAT disruption without affecting any other traffic.

set -euo pipefail

SERVER_IP="${1:-}"
SERVER_PORT="${2:-}"

if [[ -z "$SERVER_IP" || -z "$SERVER_PORT" ]]; then
    echo "Usage: sudo $0 <server_ip> <server_port>"
    echo "Example: sudo $0 203.0.113.10 9999"
    exit 1
fi

if [[ "$EUID" -ne 0 ]]; then
    echo "Error: run as root (sudo)"
    exit 1
fi

echo "==> Configuring iptables for paqet client (server: $SERVER_IP:$SERVER_PORT) ..."

# Drop RST packets the client kernel sends when it receives raw TCP frames
# from the server that don't match any open socket.  Scope to the specific
# server IP and port so no other traffic is affected.
iptables -t mangle -C OUTPUT \
    -p tcp -d "$SERVER_IP" --dport "$SERVER_PORT" \
    --tcp-flags RST RST -j DROP 2>/dev/null || \
iptables -t mangle -A OUTPUT \
    -p tcp -d "$SERVER_IP" --dport "$SERVER_PORT" \
    --tcp-flags RST RST -j DROP

echo "==> RST-DROP rule applied for $SERVER_IP:$SERVER_PORT"

# Optional: also suppress connection-tracking for this server so the kernel
# does not attempt to manage the raw-socket TCP state.
iptables -t raw -C OUTPUT \
    -p tcp -d "$SERVER_IP" --dport "$SERVER_PORT" -j NOTRACK 2>/dev/null || \
iptables -t raw -A OUTPUT \
    -p tcp -d "$SERVER_IP" --dport "$SERVER_PORT" -j NOTRACK

iptables -t raw -C PREROUTING \
    -p tcp -s "$SERVER_IP" --sport "$SERVER_PORT" -j NOTRACK 2>/dev/null || \
iptables -t raw -A PREROUTING \
    -p tcp -s "$SERVER_IP" --sport "$SERVER_PORT" -j NOTRACK

echo "==> NOTRACK rules applied."

# Persist rules across reboots.
echo ""
echo "==> To make rules persistent across reboots:"
if command -v iptables-save &>/dev/null && [ -d /etc/iptables ]; then
    read -rp "    Save rules now? [y/N] " ans
    if [[ "${ans,,}" == "y" ]]; then
        iptables-save > /etc/iptables/rules.v4
        echo "    Saved to /etc/iptables/rules.v4"
    fi
else
    echo "    Debian/Ubuntu: iptables-save > /etc/iptables/rules.v4"
    echo "    RHEL/CentOS:   service iptables save"
fi

echo ""
echo "Done. Start the client with:"
echo "  sudo ./paqet run -c config.yaml"

#!/usr/bin/env bash
# server-init.sh — One-time server setup for paqet
# Usage: sudo bash server-init.sh <PORT>
# Example: sudo bash server-init.sh 9999

set -euo pipefail

PORT="${1:-}"
if [[ -z "$PORT" ]]; then
    echo "Usage: sudo $0 <port>"
    echo "Example: sudo $0 9999"
    exit 1
fi

if [[ "$EUID" -ne 0 ]]; then
    echo "Error: run as root (sudo)"
    exit 1
fi

echo "==> Configuring iptables for paqet on port $PORT ..."

# 1. Bypass kernel connection tracking for the paqet port.
#    Without this, the kernel sees raw TCP packets it didn't create and
#    sends RST storms that tear down the tunnel.
iptables -t raw -C PREROUTING -p tcp --dport "$PORT" -j NOTRACK 2>/dev/null || \
    iptables -t raw -A PREROUTING -p tcp --dport "$PORT" -j NOTRACK
iptables -t raw -C OUTPUT    -p tcp --sport "$PORT" -j NOTRACK 2>/dev/null || \
    iptables -t raw -A OUTPUT    -p tcp --sport "$PORT" -j NOTRACK

# 2. Drop RST packets the kernel generates (it can't see the session state).
iptables -t mangle -C OUTPUT -p tcp --sport "$PORT" --tcp-flags RST RST -j DROP 2>/dev/null || \
    iptables -t mangle -A OUTPUT -p tcp --sport "$PORT" --tcp-flags RST RST -j DROP

echo "==> iptables rules applied."

# 3. Optional: install libpcap if missing (needed only for self-compiled builds).
if ! ldconfig -p | grep -q libpcap; then
    echo "==> libpcap not found. Attempting install..."
    if command -v apt-get &>/dev/null; then
        apt-get install -y libpcap-dev
    elif command -v yum &>/dev/null; then
        yum install -y libpcap-devel
    elif command -v dnf &>/dev/null; then
        dnf install -y libpcap-devel
    else
        echo "WARNING: could not install libpcap automatically. Install it manually."
    fi
fi

# 4. Persist iptables rules across reboots.
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
echo "Done. Start the server with:"
echo "  sudo ./paqet run -c config.yaml"

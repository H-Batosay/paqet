# paqet - transport over raw packets

[![Go Version](https://img.shields.io/badge/go-1.25+-blue.svg)](https://golang.org)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

`paqet` is a bidirectional packet-level proxy built using raw sockets. It forwards traffic from a local client to a remote server, bypassing the host operating system's TCP/IP stack, using KCP for secure, reliable transport.

> **⚠️ Development Status Notice**
>
> This project is in **active development**. APIs, configuration formats, and interfaces may change without notice. Use with caution in production environments.

## How It Works

`paqet` captures packets using `pcap` and injects crafted TCP packets containing encrypted transport data. KCP provides reliable, encrypted communication optimized for high-loss networks using aggressive retransmission, forward error correction, and symmetric encryption.

```
[Your App] <------> [paqet Client] <===== Raw TCP Packet =====> [paqet Server] <------> [Target Server]
(e.g. curl)        (localhost:1080)        (Internet)          (Public IP:PORT)     (e.g. https://httpbin.org)
```

## Getting Started

### 1. Prerequisites

- **Linux** — No prerequisites. Pre-built binaries are statically linked with libpcap.
- **macOS** — libpcap is bundled with Xcode CLI tools: `xcode-select --install`
- **Windows** — Install [Npcap](https://npcap.com/)

### 2. Download

Download the pre-compiled binary for your OS from the [Releases page](https://github.com/hanselime/paqet/releases/latest).

```bash
chmod +x ./paqet_linux_amd64   # make it executable (Linux/macOS)
```

### 3. Server Setup (one-time)

```bash
# On your server, run the init script (requires root):
sudo bash scripts/server-init.sh 9999   # replace 9999 with your port
```

This script:
- Configures `iptables` to bypass kernel connection tracking on the paqet port (required)
- Optionally installs `libpcap` if missing (for self-compiled builds)
- Optionally persists the rules across reboots

### 4. Configure

Network settings (interface, IP, router MAC) are **auto-detected** from the routing table. You only need to set the essentials:

**Server `config.yaml`:**
```yaml
role: "server"
listen:
  addr: ":9999"
transport:
  protocol: "kcp"
  kcp:
    key: "your-secret-key"   # CHANGE ME
```

**Client `config.yaml`:**
```yaml
role: "client"
server:
  addr: "YOUR_SERVER_IP:9999"
socks5:
  - listen: "127.0.0.1:1080"
transport:
  protocol: "kcp"
  kcp:
    key: "your-secret-key"   # CHANGE ME — must match server
```

Generate a secure key with: `./paqet secret`

### 5. (Optional) Find the Best TCP Flags

Some networks block or modify certain TCP flag patterns. `paqet` handles this at two levels:

**Runtime auto-switching** (no config needed): if a flag combination stops working during a live session, the client automatically tries the next well-known combination after 3 consecutive failures. It cycles through up to 9 common profiles (PA/PA → A/PA → P/PA → FA/FA → …) and logs each switch:
```
auto flag switch: LF=PA RF=PA → LF=A RF=PA (after 3 consecutive failures)
```

**Probe tool** (optimal setup): run once to find the fastest combination for your specific network path before deployment:

```bash
# With server running, run on the client:
sudo ./paqet probe -c config.yaml
```

Output example:
```
  Flags (local→remote)    Latency  Status
  ─────────────────────   ───────  ──────
  PA / PA                 45ms     ✔ ok
  S / SA                  —        ✗ timeout
  FA / FA                 47ms     ✔ ok
  ...

Recommended config (lowest latency):
  network:
    tcp:
      local_flag:  ["PA"]
      remote_flag: ["PA"]
```

Add the recommended flags to your config for best performance. If no flags are set, `paqet` starts with `PA/PA` and auto-switches if needed.

### 6. Run

```bash
# Server
sudo ./paqet_linux_amd64 run -c config.yaml

# Client
sudo ./paqet_linux_amd64 run -c config.yaml
```

### 7. Test

```bash
curl -v https://httpbin.org/ip --proxy socks5h://127.0.0.1:1080
```

The response should show your server's public IP.

## Command Reference

```bash
sudo ./paqet <command> [flags]
```

| Command   | Description                                                        |
| :-------- | :----------------------------------------------------------------- |
| `run`     | Start the client or server proxy                                   |
| `probe`   | Test TCP flag combinations to find which pass through the network  |
| `secret`  | Generate a cryptographically secure secret key                     |
| `ping`    | Send a single test packet to verify raw connectivity               |
| `dump`    | Capture and decode raw paqet packets (like tcpdump)                |
| `iface`   | List available network interfaces and their addresses              |
| `version` | Print version information                                          |

## Configuration Reference

Full configuration options are documented in the example files:

- [`example/client.yaml.example`](example/client.yaml.example)
- [`example/server.yaml.example`](example/server.yaml.example)

### KCP Modes

| Mode     | Description                                          | Use case                    |
| :------- | :--------------------------------------------------- | :-------------------------- |
| `auto`   | Aggressive (same as fast3) — **recommended default** | General use                 |
| `normal` | Conservative, low CPU                                | Stable, low-loss networks   |
| `fast`   | Moderate aggressiveness                              | Most networks               |
| `fast2`  | More aggressive                                      | Moderate packet loss        |
| `fast3`  | Very aggressive                                      | High packet loss            |
| `turbo`  | fast3 + FEC (adds ~30% overhead)                     | Very lossy networks         |
| `manual` | Full manual control                                  | Expert tuning               |

### Encryption

The `transport.kcp.block` field selects the encryption cipher. Default: `aes`.

⚠️ `none` and `null` disable authentication — anyone with your server IP and port can connect.

### TCP Flag Cycling

`network.tcp.local_flag` and `network.tcp.remote_flag` set the TCP flags used when crafting raw packets.

- **Auto-switching**: if the active combination fails 3 times in a row, the client automatically rotates to the next well-known combo and logs the change. No restart required.
- **Probe tool**: run `paqet probe` to benchmark all combinations up-front and find the fastest one for your network path.

If neither field is set, paqet defaults to `PA/PA` and auto-switches as needed.

## Server Firewall (Manual Setup)

If you can't use `scripts/server-init.sh`, run these commands manually:

```bash
PORT=9999   # replace with your port

iptables -t raw    -A PREROUTING -p tcp --dport $PORT -j NOTRACK
iptables -t raw    -A OUTPUT     -p tcp --sport $PORT -j NOTRACK
iptables -t mangle -A OUTPUT     -p tcp --sport $PORT --tcp-flags RST RST -j DROP

# Optional — ensure port is accessible:
iptables -t filter -A INPUT  -p tcp --dport $PORT -j ACCEPT
iptables -t filter -A OUTPUT -p tcp --sport $PORT -j ACCEPT

# Make persistent (Debian/Ubuntu):
iptables-save > /etc/iptables/rules.v4
```

## Architecture & Security Model

### The `pcap` Approach and Firewall Bypass

`paqet` hooks in at a level below the OS TCP/IP stack, receiving a copy of every matching packet directly from the network driver before `netfilter` (ufw/firewalld) can act on it.

```
      +------------------------+
      |    paqet Application   |  ← Gets packet copy immediately via pcap
      +------------------------+
              ↑        ↘
 (pcap copy) /          ↘ (original continues up the stack)
            /            ↓
      +------------------------+
      |     OS TCP/IP Stack    |  ← Firewall may block original, but paqet
      |  (Connection Tracking) |    already has its copy
      +------------------------+
                  ↑
      +------------------------+
      |     Network Driver     |
      +------------------------+
```

## Troubleshooting

1. **Permission denied** — Run with `sudo` or `root`
2. **Connection times out**:
   - Did you run `scripts/server-init.sh` (or manual iptables rules) on the server?
   - Are the `key` values identical on client and server?
   - Is the server port open in your cloud provider's firewall/security group?
   - Run `paqet probe` to test connectivity with different flag combinations; the client will also auto-switch flags at runtime after 3 consecutive failures
   - Run `paqet dump -p <PORT>` on the server to verify packets are arriving
3. **`status=203/EXEC`** — Binary is not executable: `chmod +x ./paqet_*`
4. **High CPU at idle** — Check that iptables NOTRACK rules are applied on the server

## Acknowledgments

This project draws inspiration from [gfw_resist_tcp_proxy](https://github.com/GFW-knocker/gfw_resist_tcp_proxy).

- [pcap](https://github.com/the-tcpdump-group/libpcap) — packet capture and injection
- [gopacket](https://github.com/gopacket/gopacket) — raw packet crafting and decoding
- [kcp-go](https://github.com/xtaci/kcp-go) — reliable transport with encryption
- [smux](https://github.com/xtaci/smux) — connection multiplexing

## License

MIT License. See [LICENSE](LICENSE).

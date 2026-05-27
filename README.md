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
 SOCKS5 /          (localhost:1080              (Internet)        (Public IP:PORT)    (e.g. any host)
 TCP/UDP forward    or :2222 TCP, :5353 UDP …)
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

### 3b. Client Setup (Linux only, recommended)

```bash
# On your Linux client machine (requires root):
sudo bash scripts/client-init.sh YOUR_SERVER_IP 9999
```

**Why this matters:** paqet bypasses the OS TCP stack entirely using raw sockets. When the server sends packets back, the client's kernel sees TCP frames with no matching socket and immediately sends RST. Stateful NAT routers and firewalls see this RST and tear down the NAT entry, blocking all subsequent server→client traffic — the symptom is a tunnel that "connects" but never delivers responses. This script drops the RST before it leaves the machine.

Without this rule, paqet still works on many networks (home NATs often ignore RST for short-lived mappings), but it is the first fix to try if you see `responses not received`.

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

**Runtime auto-switching** (no config needed): at startup paqet tests every well-known flag combination with a short bidirectional ping and uses the first one that passes both ways. While running, a background health-check goroutine continuously pings the active connection every 30 seconds. If the ping fails, or if an established connection drops, the client automatically tries the next combination after 3 consecutive failures. It cycles through 19 built-in profiles covering ACK-based, SYN/SYN-ACK, FIN, ECN, and asymmetric patterns, and logs each switch:
```
bidirectional check failed (LF=PA RF=PA): server→client path may be blocked
auto flag switch: LF=PA RF=PA → LF=A RF=PA (after 3 consecutive failures)
health check failed (LF=A RF=PA): <error> — forcing reconnect
```

The retry count and health-check interval are configurable (see [Configuration Reference](#configuration-reference)).

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

### Auto-Detection vs Explicit Config

paqet has two configuration modes that coexist transparently:

**Minimal mode** (new users): omit any `network` fields and paqet auto-detects everything from the OS routing table at startup — interface name, local IP, router MAC, and (on Windows) the Npcap device GUID.

**Explicit mode** (pro users / locked-down setups): set any or all `network` fields in the YAML. Every field you provide is used verbatim; auto-detection is skipped **for that field only**. If you set all four fields, the OS is never probed at all.

The rule is simple: **any field present in the config wins; missing fields are filled in automatically.**

```yaml
# Pro / advanced client config — every network field set explicitly.
# Auto-detection is completely bypassed for all fields below.
role: "client"
server:
  addr: "203.0.113.10:9999"

network:
  interface: "eth0"
  # guid: "\Device\NPF_{XXXXXXXX-...}"  # Windows only
  ipv4:
    addr: "10.0.0.5:32100"         # fixed source IP and port
    router_mac: "aa:bb:cc:dd:ee:ff"
  ipv6:                             # optional second address family
    addr: "[2001:db8::1]:32100"
    router_mac: "aa:bb:cc:dd:ee:ff"
  tcp:
    local_flag:  ["PA", "A"]       # round-robins between PA and A
    remote_flag: ["PA"]
  pcap:
    sockbuf: 16777216               # 16 MB capture buffer

transport:
  protocol: "kcp"
  conn: 4                           # four parallel KCP sessions
  kcp:
    key: "your-secret-key"
    mode: "turbo"
    block: "aes-128"
    mtu: 1350
    sndwnd: 1024
    rcvwnd: 1024
    dshard: 10
    pshard: 3

socks5:
  - listen: "0.0.0.0:1080"
    username: "user"
    password: "pass"

log:
  level: "debug"
```

Configs written before the auto-detection feature was added continue to work without any changes.

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

### Port Forwarding

The `forward` block (client-side only) binds a local port and tunnels all traffic to a fixed remote target through paqet. Both TCP and UDP are supported. Multiple rules can be combined with SOCKS5 in the same config.

```yaml
forward:
  # TCP — SSH to the server's own port 22
  - listen:   "127.0.0.1:2222"
    target:   "127.0.0.1:22"    # address as seen FROM the server
    protocol: "tcp"

  # TCP — reach a database on a host behind the server
  - listen:   "127.0.0.1:5432"
    target:   "10.0.0.50:5432"
    protocol: "tcp"

  # UDP — use the server's DNS resolver
  - listen:   "127.0.0.1:5353"
    target:   "8.8.8.8:53"
    protocol: "udp"
```

Connect to the local port and traffic arrives at `target` as if it came from the server:

```bash
# After the config above, SSH through paqet:
ssh -p 2222 user@127.0.0.1

# DNS via paqet:
dig @127.0.0.1 -p 5353 example.com
```

`target` is resolved relative to the server — use `127.0.0.1` to reach the server itself, or any IP reachable from the server for onward routing.

### TCP Flag Cycling

`network.tcp.local_flag` and `network.tcp.remote_flag` set the TCP flags used when crafting raw packets.

- **Auto-switching**: when these fields are **not** set, paqet probes all 19 built-in flag combinations at startup with a 2-second bidirectional ping and picks the first one that works. After startup, a background goroutine pings the active connection every 30 seconds; if the ping fails the cycler records a failure. Once `max_failures` consecutive failures accumulate (default: 3), the client automatically rotates to the next combo and logs the change. No restart required.

- **Explicit flags**: when `local_flag` / `remote_flag` **are** set, paqet honours that choice unconditionally — the auto-cycler is disabled entirely. Use `paqet probe` to find the best combination first, then hard-code it.

- **Probe tool**: run `paqet probe` to benchmark all combinations up-front and find the fastest one for your network path.

#### Tuning the auto-switcher

```yaml
network:
  tcp:
    probe_timeout: 8     # total seconds per flag combo during startup probe (default: 8, min: 2)
                         # PPING is retried every ~1.5 s within this window — increase on
                         # high-latency paths where KCP takes longer to establish
    max_failures: 3      # failures before switching to next combo (default: 3, min: 1)
    health_interval: 30  # seconds between background pings (default: 30; -1 = disabled)
```

**`probe_timeout`** controls how long paqet spends testing each flag combination at startup. Within that window it retries PPING every ~1.5 s, so a single dropped packet (common during KCP session initialisation) does not cause a false failure. If all 19 probes still fail (you see "all N flag combos failed bidirectional check") but SOCKS/forwarding works immediately afterwards, the probe timeout is too short or `client-init.sh` needs to be applied.

Reduce `max_failures` to `1` for aggressive probing (switches immediately on any failure). Set `health_interval: -1` to disable the health-check goroutine entirely (relies on traffic-path failures alone to drive switching).

If neither flag field is set, paqet defaults to `PA/PA` and auto-switches as needed.

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

2. **Connected but no response / one-way traffic**
   The KCP session establishes but proxied requests never get a reply. Fix in order:
   - **Run `scripts/client-init.sh SERVER_IP SERVER_PORT`** (Linux) — the client kernel sends RST for every server→client raw packet, which disrupts NAT table entries on the router; the script drops those RSTs before they leave the machine.
   - **Wrong TCP flags** — run `paqet probe` to find a combo that passes both ways. The client's bidirectional ping check and auto-switcher detect this and try the next combo after 3 failures.
   - **Server iptables not applied** — re-run `scripts/server-init.sh`; without NOTRACK/RST-DROP the server kernel tears down the raw-socket session.

3. **Connection times out (no KCP handshake at all)**:
   - Did you run `scripts/server-init.sh` on the server?
   - Are the `key` values identical on both sides?
   - Is the server port open in your cloud firewall / security group?
   - Run `paqet dump -p <PORT>` on the server to verify packets are arriving

4. **`status=203/EXEC`** — Binary is not executable: `chmod +x ./paqet_*`

5. **High CPU at idle** — Ensure iptables NOTRACK rules are applied on the server (`scripts/server-init.sh`)

## Acknowledgments

This project draws inspiration from [gfw_resist_tcp_proxy](https://github.com/GFW-knocker/gfw_resist_tcp_proxy).

- [pcap](https://github.com/the-tcpdump-group/libpcap) — packet capture and injection
- [gopacket](https://github.com/gopacket/gopacket) — raw packet crafting and decoding
- [kcp-go](https://github.com/xtaci/kcp-go) — reliable transport with encryption
- [smux](https://github.com/xtaci/smux) — connection multiplexing

## License

MIT License. See [LICENSE](LICENSE).

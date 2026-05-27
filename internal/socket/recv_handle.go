package socket

import (
	"errors"
	"fmt"
	"net"
	"paqet/internal/conf"
	"runtime"
	"strings"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/gopacket/gopacket/pcap"
)

// RecvHandle reads raw packets from the network and extracts KCP payload.
// It uses a DecodingLayerParser so Ethernet/IPv4/IPv6/TCP layer objects are
// reused across calls — no per-packet heap allocation for layer structs.
// ZeroCopyReadPacketData is used so pcap does not allocate a new []byte for
// every captured frame; the caller is responsible for copying the payload
// before the next Read() call.
type RecvHandle struct {
	handle  *pcap.Handle
	parser  *gopacket.DecodingLayerParser
	decoded []gopacket.LayerType
	eth     layers.Ethernet
	ip4     layers.IPv4
	ip6     layers.IPv6
	tcp     layers.TCP
}

func NewRecvHandle(cfg *conf.Network) (*RecvHandle, error) {
	handle, err := newHandle(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to open pcap handle: %w", err)
	}

	// SetDirection is not fully supported on Windows Npcap, so skip it
	if runtime.GOOS != "windows" {
		if err := handle.SetDirection(pcap.DirectionIn); err != nil {
			return nil, fmt.Errorf("failed to set pcap direction in: %v", err)
		}
	}

	filter := buildBPFFilter(cfg)
	if err := handle.SetBPFFilter(filter); err != nil {
		return nil, fmt.Errorf("failed to set BPF filter: %w", err)
	}

	h := &RecvHandle{
		handle:  handle,
		decoded: make([]gopacket.LayerType, 0, 4),
	}
	h.parser = gopacket.NewDecodingLayerParser(
		layers.LayerTypeEthernet,
		&h.eth, &h.ip4, &h.ip6, &h.tcp,
	)
	// Don't error on VLAN tags, PPPoE, etc. — just skip unknown layers.
	h.parser.IgnoreUnsupported = true
	return h, nil
}

// buildBPFFilter constructs the most specific BPF expression possible.
// Adding "dst host" narrows the filter to our own IP(s) so the kernel BPF
// program drops all other traffic (broadcasts, other hosts on the same
// segment) before copying frames into user space.
func buildBPFFilter(cfg *conf.Network) string {
	var hosts []string
	if cfg.IPv4.Addr != nil {
		hosts = append(hosts, "dst host "+cfg.IPv4.Addr.IP.String())
	}
	if cfg.IPv6.Addr != nil {
		hosts = append(hosts, "dst host "+cfg.IPv6.Addr.IP.String())
	}

	base := fmt.Sprintf("tcp and dst port %d", cfg.Port)
	switch len(hosts) {
	case 0:
		return base
	case 1:
		return base + " and " + hosts[0]
	default:
		return base + " and (" + strings.Join(hosts, " or ") + ")"
	}
}

// Read returns the TCP payload from the next matching inbound packet along
// with the sender's address.  Returning (nil, nil, nil) signals a pcap idle
// timeout or a non-matching/truncated frame — the caller should loop.
//
// The returned payload slice points into pcap's internal ring buffer and is
// valid only until the next Read() call.  The caller must copy it before
// calling Read() again.
func (h *RecvHandle) Read() ([]byte, net.Addr, error) {
	// ZeroCopyReadPacketData returns a subslice of pcap's ring buffer — no
	// allocation, no copy of the full Ethernet frame.  The slice is only
	// valid until the next ZeroCopyReadPacketData call.
	data, _, err := h.handle.ZeroCopyReadPacketData()
	if err != nil {
		// Finite pcap timeout expired with no packet — not an error.
		if errors.Is(err, pcap.NextErrorTimeoutExpired) || err == pcap.NextErrorTimeoutExpired {
			return nil, nil, nil
		}
		return nil, nil, err
	}

	h.decoded = h.decoded[:0]
	if err := h.parser.DecodeLayers(data, &h.decoded); err != nil {
		// Non-matching or truncated frame — discard silently.
		return nil, nil, nil
	}

	var srcIP net.IP
	var srcPort int
	hasIP, hasTCP := false, false

	for _, lt := range h.decoded {
		switch lt {
		case layers.LayerTypeIPv4:
			srcIP = h.ip4.SrcIP
			hasIP = true
		case layers.LayerTypeIPv6:
			srcIP = h.ip6.SrcIP
			hasIP = true
		case layers.LayerTypeTCP:
			srcPort = int(h.tcp.SrcPort)
			hasTCP = true
		}
	}

	if !hasIP || !hasTCP || len(h.tcp.Payload) == 0 {
		return nil, nil, nil
	}

	// Copy SrcIP: it is a subslice of the ZeroCopy ring buffer and will be
	// overwritten by the next call.
	addr := &net.UDPAddr{
		IP:   append(net.IP(nil), srcIP...),
		Port: srcPort,
	}
	// tcp.Payload is also from the ZeroCopy ring buffer.
	// The caller (recvLoop) must copy it before calling Read() again.
	return h.tcp.Payload, addr, nil
}

func (h *RecvHandle) Close() {
	if h.handle != nil {
		h.handle.Close()
	}
}

package socket

import (
	"errors"
	"fmt"
	"net"
	"paqet/internal/conf"
	"runtime"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/gopacket/gopacket/pcap"
)

// RecvHandle reads raw packets from the network and extracts KCP payload.
// It uses a DecodingLayerParser so Ethernet/IPv4/IPv6/TCP layer objects are
// reused across calls — no per-packet heap allocation for layer structs.
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

	filter := fmt.Sprintf("tcp and dst port %d", cfg.Port)
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

func (h *RecvHandle) Read() ([]byte, net.Addr, error) {
	data, _, err := h.handle.ReadPacketData()
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

	// Copy SrcIP: the slice points into data which is only valid for this call.
	// tcp.Payload also points into data but kcp-go consumes it synchronously
	// before the next ReadPacketData call so no copy is needed there.
	addr := &net.UDPAddr{
		IP:   append(net.IP(nil), srcIP...),
		Port: srcPort,
	}
	return h.tcp.Payload, addr, nil
}

func (h *RecvHandle) Close() {
	if h.handle != nil {
		h.handle.Close()
	}
}

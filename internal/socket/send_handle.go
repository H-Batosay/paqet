package socket

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"paqet/internal/conf"
	"paqet/internal/pkg/hash"
	"paqet/internal/pkg/iterator"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/gopacket/gopacket/pcap"
)

// connState tracks per-destination TCP sequence state so every burst of
// packets to the same remote looks like a single, ongoing TCP stream rather
// than a series of isolated bursts with large, unrelated sequence numbers.
type connState struct {
	mu      sync.Mutex
	seq     uint32 // next local sequence number to emit
	ack     uint32 // running acknowledgement value (simulates remote side sending)
	peerSeq uint32 // latest SYN seq observed in packets FROM this peer
	hasPeer bool   // whether peerSeq has been set at least once
}

type TCPF struct {
	tcpF       iterator.Iterator[conf.TCPF]
	clientTCPF map[uint64]*iterator.Iterator[conf.TCPF]
	mu         sync.RWMutex
}

type SendHandle struct {
	handle      *pcap.Handle
	srcIPv4     net.IP
	srcIPv4RHWA net.HardwareAddr
	srcIPv6     net.IP
	srcIPv6RHWA net.HardwareAddr
	srcPort     uint16
	startMs     uint32        // epoch ms at handle creation — timestamp base
	tsCounter   atomic.Uint32 // monotonic ms counter (global across all dsts)
	tcpF        TCPF

	connStates   map[uint64]*connState
	connStatesMu sync.RWMutex

	ethPool  sync.Pool
	ipv4Pool sync.Pool
	ipv6Pool sync.Pool
	tcpPool  sync.Pool
	bufPool  sync.Pool
}

func NewSendHandle(cfg *conf.Network) (*SendHandle, error) {
	handle, err := newHandle(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to open pcap handle: %w", err)
	}

	// SetDirection is not fully supported on Windows Npcap, so skip it
	if runtime.GOOS != "windows" {
		if err := handle.SetDirection(pcap.DirectionOut); err != nil {
			return nil, fmt.Errorf("failed to set pcap direction out: %v", err)
		}
	}

	sh := &SendHandle{
		handle:  handle,
		srcPort: uint16(cfg.Port),
		startMs: uint32(time.Now().UnixNano() / int64(time.Millisecond)),
		tcpF: TCPF{
			tcpF:       iterator.Iterator[conf.TCPF]{Items: cfg.TCP.LF},
			clientTCPF: make(map[uint64]*iterator.Iterator[conf.TCPF]),
		},
		connStates: make(map[uint64]*connState),
		ethPool: sync.Pool{
			New: func() any {
				return &layers.Ethernet{SrcMAC: cfg.Interface.HardwareAddr}
			},
		},
		ipv4Pool: sync.Pool{New: func() any { return &layers.IPv4{} }},
		ipv6Pool: sync.Pool{New: func() any { return &layers.IPv6{} }},
		tcpPool:  sync.Pool{New: func() any { return &layers.TCP{} }},
		bufPool:  sync.Pool{New: func() any { return gopacket.NewSerializeBuffer() }},
	}
	if cfg.IPv4.Addr != nil {
		sh.srcIPv4 = cfg.IPv4.Addr.IP
		sh.srcIPv4RHWA = cfg.IPv4.Router
	}
	if cfg.IPv6.Addr != nil {
		sh.srcIPv6 = cfg.IPv6.Addr.IP
		sh.srcIPv6RHWA = cfg.IPv6.Router
	}
	return sh, nil
}

// getOrCreateConnState returns the per-destination TCP sequence tracker,
// creating one with a random ISN if this is the first packet to that host.
func (h *SendHandle) getOrCreateConnState(key uint64) *connState {
	h.connStatesMu.RLock()
	cs := h.connStates[key]
	h.connStatesMu.RUnlock()
	if cs != nil {
		return cs
	}

	cs = &connState{
		seq: rand.Uint32(),
		ack: rand.Uint32(),
	}
	h.connStatesMu.Lock()
	if existing := h.connStates[key]; existing != nil {
		h.connStatesMu.Unlock()
		return existing
	}
	h.connStates[key] = cs
	h.connStatesMu.Unlock()
	return cs
}

// ObservePeerSYN records the SYN sequence number from an incoming packet
// sent by addr.  When the next SA (SYN+ACK) is sent to that addr, the ack
// field will be set to peerSeq+1 — exactly what a real TCP SYN-ACK carries.
// Call this from recvLoop whenever an inbound SYN is decoded.
func (h *SendHandle) ObservePeerSYN(addr net.Addr, seq uint32) {
	a, ok := addr.(*net.UDPAddr)
	if !ok || a == nil {
		return
	}
	cs := h.getOrCreateConnState(hash.IPAddr(a.IP, uint16(a.Port)))
	cs.mu.Lock()
	cs.peerSeq = seq
	cs.hasPeer = true
	cs.mu.Unlock()
}

func (h *SendHandle) buildIPv4Header(dstIP net.IP) *layers.IPv4 {
	ip := h.ipv4Pool.Get().(*layers.IPv4)
	*ip = layers.IPv4{
		Version:  4,
		IHL:      5,
		TOS:      184,
		TTL:      64,
		Flags:    layers.IPv4DontFragment,
		Protocol: layers.IPProtocolTCP,
		SrcIP:    h.srcIPv4,
		DstIP:    dstIP,
	}
	return ip
}

func (h *SendHandle) buildIPv6Header(dstIP net.IP) *layers.IPv6 {
	ip := h.ipv6Pool.Get().(*layers.IPv6)
	*ip = layers.IPv6{
		Version:      6,
		TrafficClass: 184,
		HopLimit:     64,
		NextHeader:   layers.IPProtocolTCP,
		SrcIP:        h.srcIPv6,
		DstIP:        dstIP,
	}
	return ip
}

// buildTCPHeader constructs a TCP layer with realistic sequence/ack/timestamp
// fields.  seq and ack come from per-destination connState so they advance
// monotonically like a real stream.  tsVal is a global monotonic ms counter;
// tsEcr simulates a recent peer timestamp (tsVal minus a plausible RTT).
func (h *SendHandle) buildTCPHeader(dstPort uint16, f conf.TCPF, seq, ack, tsVal, tsEcr uint32) *layers.TCP {
	tcp := h.tcpPool.Get().(*layers.TCP)
	*tcp = layers.TCP{
		SrcPort: layers.TCPPort(h.srcPort),
		DstPort: layers.TCPPort(dstPort),
		FIN:     f.FIN, SYN: f.SYN, RST: f.RST, PSH: f.PSH, ACK: f.ACK,
		URG: f.URG, ECE: f.ECE, CWR: f.CWR, NS: f.NS,
		Window: 65535,
		Seq:    seq,
		Ack:    ack,
	}

	if f.SYN {
		// Full SYN options: MSS + SACK-permitted + timestamps + NOP + window-scale.
		// This matches what a Linux kernel sends and passes the most strict DPI.
		tcp.Options = []layers.TCPOption{
			{OptionType: layers.TCPOptionKindMSS, OptionLength: 4, OptionData: []byte{0x05, 0xb4}}, // MSS 1460
			{OptionType: layers.TCPOptionKindSACKPermitted, OptionLength: 2},
			{OptionType: layers.TCPOptionKindTimestamps, OptionLength: 10, OptionData: make([]byte, 8)},
			{OptionType: layers.TCPOptionKindNop},
			{OptionType: layers.TCPOptionKindWindowScale, OptionLength: 3, OptionData: []byte{7}},
		}
		binary.BigEndian.PutUint32(tcp.Options[2].OptionData[0:4], tsVal)
		ecr := tsEcr
		if !f.ACK {
			ecr = 0 // pure SYN carries no echo
		}
		binary.BigEndian.PutUint32(tcp.Options[2].OptionData[4:8], ecr)
		if !f.ACK {
			tcp.Ack = 0
		}
	} else {
		// Standard NOP+NOP+timestamps used by established connections.
		tcp.Options = []layers.TCPOption{
			{OptionType: layers.TCPOptionKindNop},
			{OptionType: layers.TCPOptionKindNop},
			{OptionType: layers.TCPOptionKindTimestamps, OptionLength: 10, OptionData: make([]byte, 8)},
		}
		binary.BigEndian.PutUint32(tcp.Options[2].OptionData[0:4], tsVal)
		binary.BigEndian.PutUint32(tcp.Options[2].OptionData[4:8], tsEcr)
	}
	return tcp
}

func (h *SendHandle) Write(payload []byte, addr *net.UDPAddr) error {
	buf := h.bufPool.Get().(gopacket.SerializeBuffer)
	ethLayer := h.ethPool.Get().(*layers.Ethernet)
	defer func() {
		buf.Clear()
		h.bufPool.Put(buf)
		h.ethPool.Put(ethLayer)
	}()

	dstIP := addr.IP
	dstPort := uint16(addr.Port)
	key := hash.IPAddr(dstIP, dstPort)

	f := h.getClientTCPF(dstIP, dstPort)
	cs := h.getOrCreateConnState(key)

	// Compute how many bytes this packet "consumes" in the sequence space.
	// SYN and FIN each consume 1; data packets consume len(payload).
	advance := uint32(len(payload))
	if advance == 0 || f.SYN || f.FIN {
		advance = 1
	}

	cs.mu.Lock()
	seq := cs.seq
	ack := cs.ack
	// SYN+ACK (SA): the ack must equal the peer's SYN seq + 1 so stateful
	// firewalls accept the handshake.  Use the seq we observed from the last
	// inbound SYN packet if available; fall back to the running estimate.
	if f.SYN && f.ACK && cs.hasPeer {
		ack = cs.peerSeq + 1
	}
	cs.seq += advance
	// Advance the fake remote side proportionally so ACK numbers look
	// realistic relative to the byte counts in both directions.
	cs.ack += advance/2 + 1
	cs.mu.Unlock()

	// Monotonic ms timestamp, globally unique across all destinations.
	tsVal := h.startMs + h.tsCounter.Add(1)
	// Echo a plausible peer timestamp: simulate ~100 ms RTT.
	tsEcr := tsVal - 100

	tcpLayer := h.buildTCPHeader(dstPort, f, seq, ack, tsVal, tsEcr)
	defer h.tcpPool.Put(tcpLayer)

	var ipLayer gopacket.SerializableLayer
	if dstIP.To4() != nil {
		ip := h.buildIPv4Header(dstIP)
		defer h.ipv4Pool.Put(ip)
		ipLayer = ip
		tcpLayer.SetNetworkLayerForChecksum(ip)
		ethLayer.DstMAC = h.srcIPv4RHWA
		ethLayer.EthernetType = layers.EthernetTypeIPv4
	} else {
		ip := h.buildIPv6Header(dstIP)
		defer h.ipv6Pool.Put(ip)
		ipLayer = ip
		tcpLayer.SetNetworkLayerForChecksum(ip)
		ethLayer.DstMAC = h.srcIPv6RHWA
		ethLayer.EthernetType = layers.EthernetTypeIPv6
	}

	opts := gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}
	if err := gopacket.SerializeLayers(buf, opts, ethLayer, ipLayer, tcpLayer, gopacket.Payload(payload)); err != nil {
		return err
	}
	return h.handle.WritePacketData(buf.Bytes())
}

func (h *SendHandle) getClientTCPF(dstIP net.IP, dstPort uint16) conf.TCPF {
	h.tcpF.mu.RLock()
	defer h.tcpF.mu.RUnlock()
	if ff := h.tcpF.clientTCPF[hash.IPAddr(dstIP, dstPort)]; ff != nil {
		return ff.Next()
	}
	return h.tcpF.tcpF.Next()
}

func (h *SendHandle) setClientTCPF(addr net.Addr, f []conf.TCPF) {
	a := *addr.(*net.UDPAddr)
	h.tcpF.mu.Lock()
	h.tcpF.clientTCPF[hash.IPAddr(a.IP, uint16(a.Port))] = &iterator.Iterator[conf.TCPF]{Items: f}
	h.tcpF.mu.Unlock()
}

func (h *SendHandle) clearClientTCPF(addr net.Addr) {
	a, ok := addr.(*net.UDPAddr)
	if !ok || a == nil {
		return
	}
	k := hash.IPAddr(a.IP, uint16(a.Port))
	h.tcpF.mu.Lock()
	delete(h.tcpF.clientTCPF, k)
	h.tcpF.mu.Unlock()
	// Also remove the per-destination connState so the map doesn't grow
	// indefinitely on the server (one entry per disconnected client).
	h.connStatesMu.Lock()
	delete(h.connStates, k)
	h.connStatesMu.Unlock()
}

func (h *SendHandle) Close() {
	if h.handle != nil {
		h.handle.Close()
	}
}

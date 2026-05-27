package socket

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"paqet/internal/conf"
	"sync/atomic"
	"time"
)

// recvPacket holds a decoded packet forwarded from recvLoop.
type recvPacket struct {
	data []byte
	addr net.Addr
}

type PacketConn struct {
	cfg           *conf.Network
	sendHandle    *SendHandle
	recvHandle    *RecvHandle
	readDeadline  atomic.Value
	writeDeadline atomic.Value
	ctx           context.Context
	cancel        context.CancelFunc
	recvCh        chan recvPacket // packets dispatched by recvLoop
}

func New(ctx context.Context, cfg *conf.Network) (*PacketConn, error) {
	if cfg.Port == 0 {
		cfg.Port = 32768 + rand.Intn(32768)
	}

	sendHandle, err := NewSendHandle(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create send handle on %s: %v", cfg.Interface.Name, err)
	}

	recvHandle, err := NewRecvHandle(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create receive handle on %s: %v", cfg.Interface.Name, err)
	}

	ctx, cancel := context.WithCancel(ctx)
	conn := &PacketConn{
		cfg:        cfg,
		sendHandle: sendHandle,
		recvHandle: recvHandle,
		ctx:        ctx,
		cancel:     cancel,
		recvCh:     make(chan recvPacket, 64), // absorbs short bursts
	}
	go conn.recvLoop()
	return conn, nil
}

// recvLoop runs in its own goroutine and owns all pcap I/O.
// It forwards matched packets to recvCh so that ReadFrom can block
// on a pure-Go select instead of a kernel/CGO call.  This means the
// kcp-go goroutine sits in Go's scheduler (zero CPU, zero syscalls)
// and is woken only when a real packet arrives or ctx is cancelled —
// no idle polling at all on the consumer side.
//
// ReadPacketData allocates a fresh []byte per call, so the payload
// subslice stays valid until the GC reclaims it.  No extra copy is needed.
func (c *PacketConn) recvLoop() {
	defer close(c.recvCh)
	for {
		payload, addr, err := c.recvHandle.Read()
		if err != nil {
			return // fatal pcap error; ReadFrom will observe io.EOF
		}
		if payload == nil {
			// pcap idle timeout — no matching packet arrived.
			// Check for shutdown before re-entering ReadPacketData.
			select {
			case <-c.ctx.Done():
				return
			default:
			}
			continue
		}
		select {
		case c.recvCh <- recvPacket{data: payload, addr: addr}:
		case <-c.ctx.Done():
			return
		}
	}
}

// ReadFrom blocks until a packet is available, the context is cancelled,
// or the read deadline expires.  It never polls; it waits in a single select.
func (c *PacketConn) ReadFrom(data []byte) (n int, addr net.Addr, err error) {
	// A nil channel is never selected, so deadlineCh == nil means
	// the deadline case is simply skipped — no special-casing needed.
	var deadlineCh <-chan time.Time
	if d, ok := c.readDeadline.Load().(time.Time); ok && !d.IsZero() {
		rem := time.Until(d)
		if rem <= 0 {
			return 0, nil, os.ErrDeadlineExceeded
		}
		t := time.NewTimer(rem)
		defer t.Stop()
		deadlineCh = t.C
	}

	select {
	case <-c.ctx.Done():
		return 0, nil, c.ctx.Err()
	case <-deadlineCh:
		return 0, nil, os.ErrDeadlineExceeded
	case pkt, ok := <-c.recvCh:
		if !ok {
			// recvLoop exited (fatal pcap error or shutdown).
			return 0, nil, io.EOF
		}
		n = copy(data, pkt.data)
		return n, pkt.addr, nil
	}
}

func (c *PacketConn) WriteTo(data []byte, addr net.Addr) (n int, err error) {
	var timer *time.Timer
	var deadline <-chan time.Time
	if d, ok := c.writeDeadline.Load().(time.Time); ok && !d.IsZero() {
		timer = time.NewTimer(time.Until(d))
		defer timer.Stop()
		deadline = timer.C
	}

	select {
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	case <-deadline:
		return 0, os.ErrDeadlineExceeded
	default:
	}

	daddr, ok := addr.(*net.UDPAddr)
	if !ok {
		return 0, net.InvalidAddrError("invalid address")
	}

	err = c.sendHandle.Write(data, daddr)
	if err != nil {
		return 0, err
	}

	return len(data), nil
}

func (c *PacketConn) Close() error {
	c.cancel()

	if c.sendHandle != nil {
		c.sendHandle.Close()
	}
	if c.recvHandle != nil {
		c.recvHandle.Close()
	}

	return nil
}

func (c *PacketConn) LocalAddr() net.Addr {
	return nil
}

func (c *PacketConn) SetDeadline(t time.Time) error {
	c.readDeadline.Store(t)
	c.writeDeadline.Store(t)
	return nil
}

func (c *PacketConn) SetReadDeadline(t time.Time) error {
	c.readDeadline.Store(t)
	return nil
}

func (c *PacketConn) SetWriteDeadline(t time.Time) error {
	c.writeDeadline.Store(t)
	return nil
}

func (c *PacketConn) SetDSCP(dscp int) error {
	return nil
}

func (c *PacketConn) SetClientTCPF(addr net.Addr, f []conf.TCPF) {
	c.sendHandle.setClientTCPF(addr, f)
}

func (c *PacketConn) ClearClientTCPF(addr net.Addr) {
	if c.sendHandle != nil {
		c.sendHandle.clearClientTCPF(addr)
	}
}

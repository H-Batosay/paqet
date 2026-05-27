package protocol

import (
	"encoding/binary"
	"fmt"
	"io"

	"paqet/internal/conf"
	"paqet/internal/tnet"
)

type PType = byte

const (
	PPING PType = 0x01
	PPONG PType = 0x02
	PTCPF PType = 0x03
	PTCP  PType = 0x04
	PUDP  PType = 0x05
)

// Proto is the control-plane message exchanged over a smux stream.
//
// Wire format (all fields big-endian):
//
//	PPING / PPONG : [type:1]
//	PTCP  / PUDP  : [type:1][port:2][host_len:1][host:N]
//	PTCPF         : [type:1][count:1]([flags:2] × count)
//
// The flags word packs conf.TCPF booleans into bits 0-8 (FIN…NS).
type Proto struct {
	Type PType
	Addr *tnet.Addr
	TCPF []conf.TCPF
}

func (p *Proto) Read(r io.Reader) error {
	var hdr [1]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	p.Type = hdr[0]

	switch p.Type {
	case PPING, PPONG:
		// no payload

	case PTCP, PUDP:
		// [port:2][host_len:1][host:N]
		var lead [3]byte
		if _, err := io.ReadFull(r, lead[:]); err != nil {
			return fmt.Errorf("read addr header: %w", err)
		}
		port := int(binary.BigEndian.Uint16(lead[0:2]))
		hostLen := int(lead[2])
		host := make([]byte, hostLen)
		if hostLen > 0 {
			if _, err := io.ReadFull(r, host); err != nil {
				return fmt.Errorf("read addr host: %w", err)
			}
		}
		p.Addr = &tnet.Addr{Host: string(host), Port: port}

	case PTCPF:
		// [count:1]([flags:2] × count)
		var cb [1]byte
		if _, err := io.ReadFull(r, cb[:]); err != nil {
			return fmt.Errorf("read TCPF count: %w", err)
		}
		n := int(cb[0])
		p.TCPF = make([]conf.TCPF, n)
		if n > 0 {
			raw := make([]byte, n*2)
			if _, err := io.ReadFull(r, raw); err != nil {
				return fmt.Errorf("read TCPF flags: %w", err)
			}
			for i := range n {
				v := binary.BigEndian.Uint16(raw[i*2 : i*2+2])
				p.TCPF[i] = conf.TCPF{
					FIN: v&(1<<0) != 0,
					SYN: v&(1<<1) != 0,
					RST: v&(1<<2) != 0,
					PSH: v&(1<<3) != 0,
					ACK: v&(1<<4) != 0,
					URG: v&(1<<5) != 0,
					ECE: v&(1<<6) != 0,
					CWR: v&(1<<7) != 0,
					NS:  v&(1<<8) != 0,
				}
			}
		}

	default:
		return fmt.Errorf("unknown protocol type: 0x%02x", p.Type)
	}
	return nil
}

func (p *Proto) Write(w io.Writer) error {
	switch p.Type {
	case PPING, PPONG:
		_, err := w.Write([]byte{p.Type})
		return err

	case PTCP, PUDP:
		host := p.Addr.Host
		if len(host) > 255 {
			return fmt.Errorf("host too long: %d bytes", len(host))
		}
		buf := make([]byte, 4+len(host))
		buf[0] = p.Type
		binary.BigEndian.PutUint16(buf[1:3], uint16(p.Addr.Port))
		buf[3] = byte(len(host))
		copy(buf[4:], host)
		_, err := w.Write(buf)
		return err

	case PTCPF:
		n := len(p.TCPF)
		buf := make([]byte, 2+n*2)
		buf[0] = p.Type
		buf[1] = byte(n)
		for i, f := range p.TCPF {
			var v uint16
			if f.FIN {
				v |= 1 << 0
			}
			if f.SYN {
				v |= 1 << 1
			}
			if f.RST {
				v |= 1 << 2
			}
			if f.PSH {
				v |= 1 << 3
			}
			if f.ACK {
				v |= 1 << 4
			}
			if f.URG {
				v |= 1 << 5
			}
			if f.ECE {
				v |= 1 << 6
			}
			if f.CWR {
				v |= 1 << 7
			}
			if f.NS {
				v |= 1 << 8
			}
			binary.BigEndian.PutUint16(buf[2+i*2:], v)
		}
		_, err := w.Write(buf)
		return err

	default:
		return fmt.Errorf("unknown protocol type: 0x%02x", p.Type)
	}
}

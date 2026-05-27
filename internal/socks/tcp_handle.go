package socks

import (
	"net"
	"sync"
	"time"

	"paqet/internal/flog"
	"paqet/internal/pkg/buffer"

	"github.com/txthinking/socks5"
)

func (h *Handler) TCPHandle(server *socks5.Server, conn *net.TCPConn, r *socks5.Request) error {
	if r.Cmd == socks5.CmdUDP {
		flog.Debugf("SOCKS5 UDP_ASSOCIATE from %s", conn.RemoteAddr())
		return h.handleUDPAssociate(conn)
	}
	if r.Cmd == socks5.CmdConnect {
		flog.Debugf("SOCKS5 CONNECT from %s to %s", conn.RemoteAddr(), r.Address())
		return h.handleTCPConnect(conn, r)
	}
	flog.Debugf("unsupported SOCKS5 command %d from %s", r.Cmd, conn.RemoteAddr())
	return nil
}

func (h *Handler) handleTCPConnect(conn *net.TCPConn, r *socks5.Request) error {
	// Build and send SOCKS5 success reply first so the client can start
	// buffering its request while we open the tunnel stream.
	buf := make([]byte, 0, 4+1+255+2)
	buf = append(buf, socks5.Ver)
	buf = append(buf, socks5.RepSuccess)
	buf = append(buf, 0x00) // reserved
	laddr := conn.LocalAddr().(*net.TCPAddr)
	if ip4 := laddr.IP.To4(); ip4 != nil {
		buf = append(buf, socks5.ATYPIPv4)
		buf = append(buf, ip4...)
	} else if ip6 := laddr.IP.To16(); ip6 != nil {
		buf = append(buf, socks5.ATYPIPv6)
		buf = append(buf, ip6...)
	} else {
		host := laddr.IP.String()
		buf = append(buf, socks5.ATYPDomain)
		buf = append(buf, byte(len(host)))
		buf = append(buf, host...)
	}
	buf = append(buf, byte(laddr.Port>>8), byte(laddr.Port&0xff))
	if _, err := conn.Write(buf); err != nil {
		return err
	}

	strm, err := h.client.TCP(r.Address())
	if err != nil {
		flog.Errorf("SOCKS5 failed to open tunnel for %s -> %s: %v", conn.RemoteAddr(), r.Address(), err)
		return err
	}
	flog.Infof("SOCKS5 TCP %s -> %s (stream %d)", conn.RemoteAddr(), r.Address(), strm.SID())

	// closeBoth ensures both ends are shut down exactly once regardless of
	// which goroutine or path triggers the close.
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			_ = strm.Close()
			_ = conn.Close()
		})
	}
	defer closeBoth()

	errCh := make(chan error, 2)
	// app → tunnel
	go func() { errCh <- buffer.CopyT(strm, conn) }()
	// tunnel → app
	go func() { errCh <- buffer.CopyT(conn, strm) }()

	select {
	case err := <-errCh:
		// One direction closed. Shut both ends so the other goroutine
		// unblocks, then wait for it to exit before returning — this
		// ensures any data already read from the stream is fully written
		// to the app connection before we tear down.
		closeBoth()
		select {
		case <-errCh:
		case <-time.After(2 * time.Second):
			flog.Debugf("SOCKS5 stream %d drain timeout for %s -> %s", strm.SID(), conn.RemoteAddr(), r.Address())
		}
		if err != nil {
			flog.Debugf("SOCKS5 stream %d for %s -> %s: %v", strm.SID(), conn.RemoteAddr(), r.Address(), err)
		}

	case <-h.ctx.Done():
		closeBoth()
		select {
		case <-errCh:
			select {
			case <-errCh:
			case <-time.After(2 * time.Second):
			}
		case <-time.After(2 * time.Second):
		}
	}

	flog.Debugf("SOCKS5 TCP %s -> %s closed (stream %d)", conn.RemoteAddr(), r.Address(), strm.SID())
	return nil
}

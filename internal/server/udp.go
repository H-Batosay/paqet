package server

import (
	"context"
	"net"
	"time"

	"paqet/internal/flog"
	"paqet/internal/pkg/buffer"
	"paqet/internal/protocol"
	"paqet/internal/tnet"
	"sync"
)

func (s *Server) handleUDPProtocol(ctx context.Context, strm tnet.Strm, p *protocol.Proto) error {
	flog.Infof("accepted UDP stream %d: %s -> %s", strm.SID(), strm.RemoteAddr(), p.Addr.String())
	return s.handleUDP(ctx, strm, p.Addr.String())
}

func (s *Server) handleUDP(ctx context.Context, strm tnet.Strm, addr string) error {
	dialer := &net.Dialer{Timeout: 8 * time.Second}
	conn, err := dialer.DialContext(ctx, "udp", addr)
	if err != nil {
		flog.Errorf("failed to establish UDP connection to %s for stream %d: %v", addr, strm.SID(), err)
		return err
	}
	flog.Debugf("UDP connection established to %s for stream %d", addr, strm.SID())

	errChan := make(chan error, 2)
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			_ = conn.Close()
			_ = strm.Close()
			flog.Debugf("closed UDP connection %s for stream %d", addr, strm.SID())
		})
	}
	go func() {
		err := buffer.CopyU(conn, strm)
		errChan <- err
	}()
	go func() {
		err := buffer.CopyU(strm, conn)
		errChan <- err
	}()

	select {
	case err := <-errChan:
		closeBoth()
		// Ensure the other copy goroutine exits too.
		select {
		case <-errChan:
		case <-time.After(2 * time.Second):
		}
		if err != nil {
			flog.Errorf("UDP stream %d to %s failed: %v", strm.SID(), addr, err)
			return err
		}
	case <-ctx.Done():
		closeBoth()
		select {
		case <-errChan:
			select {
			case <-errChan:
			case <-time.After(2 * time.Second):
			}
		case <-time.After(2 * time.Second):
		}
		return nil
	}

	return nil
}

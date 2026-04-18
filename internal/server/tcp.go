package server

import (
	"context"
	"net"
	"paqet/internal/flog"
	"paqet/internal/pkg/buffer"
	"paqet/internal/protocol"
	"paqet/internal/tnet"
	"sync"
	"time"
)

func (s *Server) handleTCPProtocol(ctx context.Context, strm tnet.Strm, p *protocol.Proto) error {
	flog.Infof("accepted TCP stream %d: %s -> %s", strm.SID(), strm.RemoteAddr(), p.Addr.String())
	return s.handleTCP(ctx, strm, p.Addr.String())
}

func (s *Server) handleTCP(ctx context.Context, strm tnet.Strm, addr string) error {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		flog.Errorf("failed to establish TCP connection to %s for stream %d: %v", addr, strm.SID(), err)
		return err
	}
	flog.Debugf("TCP connection established to %s for stream %d", addr, strm.SID())

	errChan := make(chan error, 2)
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			_ = conn.Close()
			_ = strm.Close()
			flog.Debugf("closed TCP connection %s for stream %d", addr, strm.SID())
		})
	}
	go func() {
		err := buffer.CopyT(conn, strm)
		errChan <- err
	}()
	go func() {
		err := buffer.CopyT(strm, conn)
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
			flog.Errorf("TCP stream %d to %s failed: %v", strm.SID(), addr, err)
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
	}
	return nil
}

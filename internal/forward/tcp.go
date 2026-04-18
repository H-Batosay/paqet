package forward

import (
	"context"
	"net"
	"paqet/internal/flog"
	"paqet/internal/pkg/buffer"
	"sync"
	"time"
)

func (f *Forward) listenTCP(ctx context.Context) error {
	listener, err := net.Listen("tcp", f.listenAddr)
	if err != nil {
		flog.Errorf("failed to bind TCP socket on %s: %v", f.listenAddr, err)
		return err
	}
	defer listener.Close()
	go func() {
		<-ctx.Done()
		listener.Close()
	}()
	flog.Infof("TCP forwarder listening on %s -> %s", f.listenAddr, f.targetAddr)

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				flog.Errorf("failed to accept TCP connection on %s: %v", f.listenAddr, err)
				time.Sleep(50 * time.Millisecond)
				continue
			}
		}

		f.wg.Go(func() {
			defer conn.Close()
			if err := f.handleTCPConn(ctx, conn); err != nil {
				flog.Errorf("TCP connection %s -> %s closed with error: %v", conn.RemoteAddr(), f.targetAddr, err)
			} else {
				flog.Debugf("TCP connection %s -> %s closed", conn.RemoteAddr(), f.targetAddr)
			}
		})
	}
}

func (f *Forward) handleTCPConn(ctx context.Context, conn net.Conn) error {
	strm, err := f.client.TCP(f.targetAddr)
	if err != nil {
		flog.Errorf("failed to establish stream for %s -> %s: %v", conn.RemoteAddr(), f.targetAddr, err)
		return err
	}
	flog.Infof("accepted TCP connection %s -> %s", conn.RemoteAddr(), f.targetAddr)

	errCh := make(chan error, 2)
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			_ = conn.Close()
			_ = strm.Close()
			flog.Debugf("TCP stream closed for %s -> %s", conn.RemoteAddr(), f.targetAddr)
		})
	}
	go func() {
		err := buffer.CopyT(conn, strm)
		errCh <- err
	}()
	go func() {
		err := buffer.CopyT(strm, conn)
		errCh <- err
	}()

	select {
	case err := <-errCh:
		closeBoth()
		// Ensure the other copy goroutine exits too.
		select {
		case <-errCh:
		case <-time.After(2 * time.Second):
		}
		if err != nil {
			flog.Errorf("TCP stream %d failed for %s -> %s: %v", strm.SID(), conn.RemoteAddr(), f.targetAddr, err)
			return err
		}
	case <-ctx.Done():
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

	return nil
}

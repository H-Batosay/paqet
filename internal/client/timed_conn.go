package client

import (
	"context"
	"fmt"
	"sync"

	"paqet/internal/conf"
	"paqet/internal/protocol"
	"paqet/internal/socket"
	"paqet/internal/tnet"
	"paqet/internal/tnet/kcp"
)

// timedConn wraps a single persistent KCP/smux connection.
// mu protects conn so that reconnect I/O doesn't hold the global client lock.
type timedConn struct {
	cfg    *conf.Conf
	conn   tnet.Conn
	ctx    context.Context
	mu     sync.Mutex
	cycler *flagCycler // auto flag-switching on repeated connection failures
}

func newTimedConn(ctx context.Context, cfg *conf.Conf) (*timedConn, error) {
	tc := timedConn{
		cfg:    cfg,
		ctx:    ctx,
		cycler: newFlagCycler(cfg.Network.TCP.LF, cfg.Network.TCP.RF),
	}
	var err error
	tc.conn, err = tc.createConn()
	if err != nil {
		return nil, err
	}
	return &tc, nil
}

func (tc *timedConn) createConn() (tnet.Conn, error) {
	// Use the cycler's active flags — these may differ from cfg defaults after
	// an automatic flag switch triggered by repeated connection failures.
	activeLF, activeRF := tc.cycler.Active()

	// Value-copy Network so we can override TCP flags without touching cfg.
	netCfg := tc.cfg.Network
	netCfg.TCP.LF = activeLF
	netCfg.TCP.RF = activeRF

	pConn, err := socket.New(tc.ctx, &netCfg)
	if err != nil {
		return nil, fmt.Errorf("could not create packet conn: %w", err)
	}

	conn, err := kcp.Dial(tc.cfg.Server.Addr, tc.cfg.Transport.KCP, pConn)
	if err != nil {
		pConn.Close() // Dial failed before taking ownership of pConn
		return nil, err
	}
	if err = tc.sendTCPF(conn, activeRF); err != nil {
		conn.Close() // conn.Close() also closes the underlying pConn
		return nil, err
	}
	return conn, nil
}

// sendTCPF opens a temporary stream and tells the server which TCP flags to
// use for packets it sends back to this client.
func (tc *timedConn) sendTCPF(conn tnet.Conn, rf []conf.TCPF) error {
	strm, err := conn.OpenStrm()
	if err != nil {
		return err
	}
	defer strm.Close()

	p := protocol.Proto{Type: protocol.PTCPF, TCPF: rf}
	return p.Write(strm)
}

func (tc *timedConn) close() {
	if tc.conn != nil {
		tc.conn.Close()
	}
}

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
	cfg  *conf.Conf
	conn tnet.Conn
	ctx  context.Context
	mu   sync.Mutex
}

func newTimedConn(ctx context.Context, cfg *conf.Conf) (*timedConn, error) {
	tc := timedConn{cfg: cfg, ctx: ctx}
	var err error
	tc.conn, err = tc.createConn()
	if err != nil {
		return nil, err
	}
	return &tc, nil
}

func (tc *timedConn) createConn() (tnet.Conn, error) {
	netCfg := tc.cfg.Network
	pConn, err := socket.New(tc.ctx, &netCfg)
	if err != nil {
		return nil, fmt.Errorf("could not create packet conn: %w", err)
	}

	conn, err := kcp.Dial(tc.cfg.Server.Addr, tc.cfg.Transport.KCP, pConn)
	if err != nil {
		return nil, err
	}
	if err = tc.sendTCPF(conn); err != nil {
		return nil, err
	}
	return conn, nil
}

func (tc *timedConn) sendTCPF(conn tnet.Conn) error {
	strm, err := conn.OpenStrm()
	if err != nil {
		return err
	}
	defer strm.Close()

	p := protocol.Proto{Type: protocol.PTCPF, TCPF: tc.cfg.Network.TCP.RF}
	return p.Write(strm)
}

func (tc *timedConn) close() {
	if tc.conn != nil {
		tc.conn.Close()
	}
}

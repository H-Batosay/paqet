package client

import (
	"context"
	"fmt"
	"sync"
	"time"

	"paqet/internal/conf"
	"paqet/internal/flog"
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
	lfStr, rfStr := tc.cycler.ActiveStrings()
	flog.Infof("dialing server %s (LF=%s RF=%s)", tc.cfg.Server.Addr, lfStr, rfStr)

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
		return nil, fmt.Errorf("KCP dial failed: %w", err)
	}

	if err = tc.sendTCPF(conn, activeRF); err != nil {
		conn.Close()
		return nil, fmt.Errorf("TCPF handshake failed: %w", err)
	}

	// Verify that data flows in BOTH directions before declaring success.
	// A ping sends a KCP frame TO the server and expects a PONG back.
	// If only client→server works (e.g. firewall drops reverse traffic),
	// the ping times out and createConn returns an error — the flagCycler
	// then advances to the next combo after maxFlagFailures attempts.
	if err = tc.verifyBidirectional(conn, lfStr, rfStr); err != nil {
		conn.Close()
		return nil, err
	}

	flog.Infof("connection established → %s (LF=%s RF=%s)", tc.cfg.Server.Addr, lfStr, rfStr)
	return conn, nil
}

// verifyBidirectional sends a ping and waits for the pong with a 5-second
// deadline.  A timeout means the server→client path is blocked.
func (tc *timedConn) verifyBidirectional(conn tnet.Conn, lfStr, rfStr string) error {
	const pingTimeout = 5 * time.Second
	flog.Debugf("verifying bidirectional connectivity (LF=%s RF=%s, timeout %s)", lfStr, rfStr, pingTimeout)

	if err := conn.SetDeadline(time.Now().Add(pingTimeout)); err != nil {
		return fmt.Errorf("could not set ping deadline: %w", err)
	}
	err := conn.Ping(true)
	// Always reset deadline regardless of outcome.
	_ = conn.SetDeadline(time.Time{})

	if err != nil {
		flog.Warnf("bidirectional check failed (LF=%s RF=%s): server→client path may be blocked — %v", lfStr, rfStr, err)
		return fmt.Errorf("ping timeout with LF=%s RF=%s (server→client blocked?): %w", lfStr, rfStr, err)
	}

	flog.Debugf("bidirectional OK (LF=%s RF=%s)", lfStr, rfStr)
	return nil
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

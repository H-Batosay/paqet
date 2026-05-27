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

// newTimedConn creates a timedConn and establishes the first KCP connection.
//
// Startup probe strategy:
//  1. Try every flag combo in the cycler with a short (2 s) bidirectional ping.
//     First combo that passes → use it and return.
//  2. If ALL combos fail the ping (server→client path blocked — usually the
//     client-init.sh RST-DROP rule is missing) → fall back to connecting
//     WITHOUT ping verification so the process stays alive.  A clear warning
//     is logged pointing to the fix.
//  3. Hard-fail only if the server is truly unreachable (KCP dial fails on
//     every combo).
func newTimedConn(ctx context.Context, cfg *conf.Conf) (*timedConn, error) {
	tc := &timedConn{
		cfg:    cfg,
		ctx:    ctx,
		cycler: newFlagCycler(cfg.Network.TCP.LF, cfg.Network.TCP.RF),
	}

	n := tc.cycler.Len()
	flog.Infof("startup probe: testing %d flag combo(s) against %s (2s each)", n, cfg.Server.Addr)

	// Phase 1 — find a combo that passes the bidirectional check.
	for i := 0; i < n; i++ {
		lfStr, rfStr := tc.cycler.ActiveStrings()
		flog.Debugf("  probe [%d/%d] LF=%s RF=%s", i+1, n, lfStr, rfStr)

		conn, err := tc.dialKCPOnly()
		if err != nil {
			flog.Debugf("  probe [%d/%d] KCP dial failed: %v", i+1, n, err)
			tc.cycler.ForceNext()
			continue
		}

		if err = tc.verifyBidirectional(conn, lfStr, rfStr, 2*time.Second); err == nil {
			flog.Infof("startup probe: working combo found — LF=%s RF=%s", lfStr, rfStr)
			tc.conn = conn
			return tc, nil
		}
		conn.Close()
		tc.cycler.ForceNext()
	}

	// Phase 2 — no combo passed ping.  Reset to combo 0 and start without
	// ping verification.  The connection will work once client-init.sh is run.
	tc.cycler.SetIdx(0)
	lfStr, rfStr := tc.cycler.ActiveStrings()

	flog.Warnf("startup probe: all %d flag combo(s) failed bidirectional check", n)
	flog.Warnf("  → server→client packets are likely blocked by your OS/NAT sending RST")
	flog.Warnf("  → FIX: sudo bash scripts/client-init.sh %s", cfg.Server.Addr)
	flog.Warnf("  → starting anyway with LF=%s RF=%s — will work once the rule is applied", lfStr, rfStr)

	conn, err := tc.dialKCPOnly()
	if err != nil {
		return nil, fmt.Errorf("server unreachable at %s: %w", cfg.Server.Addr, err)
	}
	tc.conn = conn
	return tc, nil
}

// createConn dials a fresh KCP session using the cycler's current active flags
// and verifies bidirectional connectivity before returning.
// Used for all reconnect attempts after startup.
func (tc *timedConn) createConn() (tnet.Conn, error) {
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
		pConn.Close()
		return nil, fmt.Errorf("KCP dial failed: %w", err)
	}

	if err = tc.sendTCPF(conn, activeRF); err != nil {
		conn.Close()
		return nil, fmt.Errorf("TCPF handshake failed: %w", err)
	}

	if err = tc.verifyBidirectional(conn, lfStr, rfStr, 5*time.Second); err != nil {
		conn.Close()
		return nil, err
	}

	flog.Infof("connection established → %s (LF=%s RF=%s)", tc.cfg.Server.Addr, lfStr, rfStr)
	return conn, nil
}

// dialKCPOnly establishes a KCP connection and sends the TCPF handshake but
// does NOT run the bidirectional ping check.  Used by the startup probe and
// as a last-resort fallback when ping verification fails on all combos.
func (tc *timedConn) dialKCPOnly() (tnet.Conn, error) {
	activeLF, activeRF := tc.cycler.Active()

	netCfg := tc.cfg.Network
	netCfg.TCP.LF = activeLF
	netCfg.TCP.RF = activeRF

	pConn, err := socket.New(tc.ctx, &netCfg)
	if err != nil {
		return nil, fmt.Errorf("could not create packet conn: %w", err)
	}

	conn, err := kcp.Dial(tc.cfg.Server.Addr, tc.cfg.Transport.KCP, pConn)
	if err != nil {
		pConn.Close()
		return nil, fmt.Errorf("KCP dial failed: %w", err)
	}

	if err = tc.sendTCPF(conn, activeRF); err != nil {
		conn.Close()
		return nil, fmt.Errorf("TCPF handshake failed: %w", err)
	}

	return conn, nil
}

// verifyBidirectional sends a ping and waits for the pong within timeout.
// A timeout indicates the server→client path is blocked.
func (tc *timedConn) verifyBidirectional(conn tnet.Conn, lfStr, rfStr string, timeout time.Duration) error {
	flog.Debugf("verifying bidirectional connectivity (LF=%s RF=%s, timeout %s)", lfStr, rfStr, timeout)

	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
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

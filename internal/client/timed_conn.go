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
//     First combo that passes both-way connectivity → use it and return.
//  2. If ALL combos fail the ping (server→client path blocked — usually the
//     client-init.sh RST-DROP rule is missing) → fall back to connecting
//     WITHOUT ping verification so the process stays alive.  A clear warning
//     is logged pointing to the fix.
//  3. Hard-fail only if the server is truly unreachable (KCP dial fails).
//
// After startup a background health-check goroutine continuously pings the
// connection at the interval configured in network.tcp.health_interval.
func newTimedConn(ctx context.Context, cfg *conf.Conf) (*timedConn, error) {
	tc := &timedConn{
		cfg: cfg,
		ctx: ctx,
		cycler: newFlagCycler(
			cfg.Network.TCP.LF,
			cfg.Network.TCP.RF,
			cfg.Network.TCP.ExplicitFlags,
			cfg.Network.TCP.MaxFailures,
		),
	}

	n := tc.cycler.Len()
	if cfg.Network.TCP.ExplicitFlags {
		lfStr, rfStr := tc.cycler.ActiveStrings()
		flog.Infof("startup: connecting with configured flags LF=%s RF=%s (no auto-switch)", lfStr, rfStr)
	} else {
		flog.Infof("startup probe: testing %d flag combo(s) against %s (5s each)", n, cfg.Server.Addr)
	}

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

		if err = tc.verifyBidirectional(conn, lfStr, rfStr, 5*time.Second); err == nil {
			flog.Infof("startup probe: working combo found — LF=%s RF=%s", lfStr, rfStr)
			tc.conn = conn
			tc.startHealthCheck()
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
	flog.Warnf("  → FIX: sudo bash scripts/client-init.sh %s %d", cfg.Server.Addr.IP, cfg.Server.Addr.Port)
	flog.Warnf("  → starting anyway with LF=%s RF=%s — will work once the rule is applied", lfStr, rfStr)

	conn, err := tc.dialKCPOnly()
	if err != nil {
		return nil, fmt.Errorf("server unreachable at %s: %w", cfg.Server.Addr, err)
	}
	tc.conn = conn
	tc.startHealthCheck()
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
//
// The deadline is set on the individual smux stream, not on the session —
// smux.Session.SetDeadline does NOT propagate to stream read/write, so
// setting it on the session would have no effect on Ping's read call.
func (tc *timedConn) verifyBidirectional(conn tnet.Conn, lfStr, rfStr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	flog.Debugf("verifying bidirectional connectivity (LF=%s RF=%s, timeout %s)", lfStr, rfStr, timeout)

	err := conn.Ping(true, deadline)
	if err != nil {
		flog.Warnf("bidirectional check failed (LF=%s RF=%s): server→client path may be blocked — %v", lfStr, rfStr, err)
		return fmt.Errorf("ping timeout with LF=%s RF=%s (server→client blocked?): %w", lfStr, rfStr, err)
	}

	flog.Debugf("bidirectional OK (LF=%s RF=%s)", lfStr, rfStr)
	return nil
}

// startHealthCheck launches a background goroutine that periodically pings
// the active connection.  If the ping fails the connection is closed and the
// flag cycler records a failure so the next reconnect tries a different combo.
// Does nothing when health_interval is 0 (disabled) or negative.
func (tc *timedConn) startHealthCheck() {
	interval := tc.cfg.Network.TCP.HealthInterval
	if interval <= 0 {
		return
	}
	go tc.healthCheckLoop(interval)
}

func (tc *timedConn) healthCheckLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-tc.ctx.Done():
			return
		case <-ticker.C:
			tc.mu.Lock()
			conn := tc.conn
			tc.mu.Unlock()

			if conn == nil || conn.IsClosed() {
				// Already dead — newConn() will reconnect when traffic arrives.
				continue
			}

			lfStr, rfStr := tc.cycler.ActiveStrings()
			deadline := time.Now().Add(5 * time.Second)
			if err := conn.Ping(true, deadline); err != nil {
				flog.Warnf("health check failed (LF=%s RF=%s): %v — forcing reconnect", lfStr, rfStr, err)
				tc.cycler.Fail()
				conn.Close() // marks conn as dead; next newConn() call reconnects
			} else {
				flog.Debugf("health check OK (LF=%s RF=%s)", lfStr, rfStr)
			}
		}
	}
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

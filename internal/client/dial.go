package client

import (
	"fmt"

	"paqet/internal/flog"
	"paqet/internal/tnet"
)

// newConn returns the next live connection.  If the smux session is closed,
// it reconnects without holding any global lock so that other goroutines using
// different connections are not blocked during I/O.
func (c *Client) newConn() (tnet.Conn, error) {
	tc := c.iter.Next()

	tc.mu.Lock()
	if !tc.conn.IsClosed() {
		conn := tc.conn
		tc.mu.Unlock()
		return conn, nil
	}
	tc.mu.Unlock()

	// Reconnect outside the lock so unrelated connections stay usable.
	flog.Infof("connection lost, reconnecting...")
	conn, err := tc.createConn()
	if err != nil {
		return nil, fmt.Errorf("reconnect failed: %w", err)
	}

	tc.mu.Lock()
	if tc.conn.IsClosed() {
		tc.conn = conn // we won the race
	} else {
		conn.Close() // another goroutine already reconnected
		conn = tc.conn
	}
	tc.mu.Unlock()
	return conn, nil
}

// newStrm opens a multiplexed stream, retrying up to maxRetries times.
func (c *Client) newStrm() (tnet.Strm, error) {
	const maxRetries = 3
	for i := range maxRetries {
		conn, err := c.newConn()
		if err != nil {
			flog.Debugf("session creation failed (attempt %d/%d): %v", i+1, maxRetries, err)
			continue
		}
		strm, err := conn.OpenStrm()
		if err != nil {
			flog.Debugf("failed to open stream (attempt %d/%d): %v", i+1, maxRetries, err)
			continue
		}
		return strm, nil
	}
	return nil, fmt.Errorf("failed to create stream after %d attempts", maxRetries)
}

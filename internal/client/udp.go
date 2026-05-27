package client

import (
	"paqet/internal/flog"
	"paqet/internal/pkg/hash"
	"paqet/internal/protocol"
	"paqet/internal/tnet"
)

// UDP returns a stream multiplexed over an existing KCP connection for the
// given local→target address pair.  It is safe for concurrent use.
//
// If the pair already has an active stream, new=false and the caller must NOT
// start a reader goroutine for it (one is already running).
// If new=true the caller is responsible for draining the stream.
func (c *Client) UDP(lAddr, tAddr string) (tnet.Strm, bool, uint64, error) {
	key := hash.AddrPair(lAddr, tAddr)

	// Fast path: stream already exists.
	c.udpPool.mu.RLock()
	strm, exists := c.udpPool.strms[key]
	c.udpPool.mu.RUnlock()
	if exists {
		flog.Debugf("reusing UDP stream %d for %s -> %s", strm.SID(), lAddr, tAddr)
		return strm, false, key, nil
	}

	// Slow path: create a new stream outside any lock so I/O doesn't stall
	// unrelated goroutines.
	newStrm, err := c.newStrm()
	if err != nil {
		flog.Debugf("failed to create stream for UDP %s -> %s: %v", lAddr, tAddr, err)
		return nil, false, 0, err
	}

	taddr, err := tnet.NewAddr(tAddr)
	if err != nil {
		flog.Debugf("invalid UDP address %s: %v", tAddr, err)
		newStrm.Close()
		return nil, false, 0, err
	}
	p := protocol.Proto{Type: protocol.PUDP, Addr: taddr}
	if err := p.Write(newStrm); err != nil {
		flog.Debugf("failed to write UDP protocol header for %s -> %s on stream %d: %v", lAddr, tAddr, newStrm.SID(), err)
		newStrm.Close()
		return nil, false, 0, err
	}

	// Double-checked locking: another goroutine may have inserted the same key
	// while we were doing I/O above.  If so, discard our stream and use theirs.
	c.udpPool.mu.Lock()
	if existing, exists := c.udpPool.strms[key]; exists {
		c.udpPool.mu.Unlock()
		newStrm.Close()
		flog.Debugf("reusing UDP stream %d for %s -> %s (race resolved)", existing.SID(), lAddr, tAddr)
		return existing, false, key, nil
	}
	c.udpPool.strms[key] = newStrm
	c.udpPool.mu.Unlock()

	flog.Debugf("UDP stream %d created for %s -> %s", newStrm.SID(), lAddr, tAddr)
	return newStrm, true, key, nil
}

func (c *Client) CloseUDP(key uint64) error {
	return c.udpPool.delete(key)
}

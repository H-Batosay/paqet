package buffer

import "sync"

var (
	TPool int
	UPool int
	tBuf  sync.Pool
	uBuf  sync.Pool
)

func Initialize(tcp, udp int) {
	TPool = tcp
	UPool = udp
	tBuf = sync.Pool{New: func() any { return make([]byte, TPool) }}
	uBuf = sync.Pool{New: func() any { return make([]byte, UPool) }}
}

// GetTBuf returns a TCP-sized buffer from the pool.
func GetTBuf() []byte { return tBuf.Get().([]byte) }

// PutTBuf returns a TCP-sized buffer to the pool.
func PutTBuf(b []byte) { tBuf.Put(b) }

// GetUBuf returns a UDP-sized buffer from the pool.
func GetUBuf() []byte { return uBuf.Get().([]byte) }

// PutUBuf returns a UDP-sized buffer to the pool.
func PutUBuf(b []byte) { uBuf.Put(b) }

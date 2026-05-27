package client

import (
	"sync"

	"paqet/internal/conf"
	"paqet/internal/flog"
)

// maxFlagFailures is the number of consecutive connection failures with the
// current TCP flag combination before the cycler advances to the next one.
const maxFlagFailures = 3

type flagPair struct{ lf, rf string }

// fallbackCombos is the ordered list of well-known flag pairs tried when the
// active combo fails.  Covers the most common firewall/NAT profiles.
// Entries that duplicate the user-configured combo are skipped at init time.
var fallbackCombos = []flagPair{
	{"PA", "PA"},
	{"A", "PA"},
	{"P", "PA"},
	{"FA", "FA"},
	{"FA", "PA"},
	{"S", "SA"},
	{"SA", "PA"},
	{"EA", "PA"},
	{"CA", "PA"},
}

// flagCycler manages automatic TCP flag switching for a single timedConn.
// It starts with the user-configured combo and falls back to fallbackCombos
// after maxFlagFailures consecutive connection failures.
// All methods are safe for concurrent use.
type flagCycler struct {
	mu       sync.Mutex
	combos   []flagPair // ordered list; combos[0] is always the user config
	idx      int        // currently active combo index
	failures int        // consecutive failures with the active combo
}

// newFlagCycler returns a cycler whose first entry is the user-configured
// combo (lf/rf).  All fallbackCombos follow, skipping any duplicate.
func newFlagCycler(lf, rf []conf.TCPF) *flagCycler {
	userLF := tcpfToStr(lf)
	userRF := tcpfToStr(rf)

	combos := []flagPair{{userLF, userRF}}
	for _, c := range fallbackCombos {
		if c.lf == userLF && c.rf == userRF {
			continue // already covered by the user combo
		}
		combos = append(combos, c)
	}
	return &flagCycler{combos: combos}
}

// Active returns the currently active local and remote flag slices.
func (fc *flagCycler) Active() (lf, rf []conf.TCPF) {
	fc.mu.Lock()
	p := fc.combos[fc.idx]
	fc.mu.Unlock()
	return parseFlagStr(p.lf), parseFlagStr(p.rf)
}

// ActiveStrings returns the currently active LF/RF as human-readable strings.
func (fc *flagCycler) ActiveStrings() (lf, rf string) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	p := fc.combos[fc.idx]
	return p.lf, p.rf
}

// Fail records one connection failure.  Once maxFlagFailures is reached the
// cycler advances to the next combo and logs the switch.
func (fc *flagCycler) Fail() {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	fc.failures++
	if fc.failures < maxFlagFailures {
		return
	}
	old := fc.combos[fc.idx]
	fc.idx = (fc.idx + 1) % len(fc.combos)
	fc.failures = 0
	next := fc.combos[fc.idx]
	flog.Infof("auto flag switch: LF=%s RF=%s → LF=%s RF=%s (after %d consecutive failures)",
		old.lf, old.rf, next.lf, next.rf, maxFlagFailures)
}

// Succeed resets the consecutive failure counter without changing the active
// combo.  Call this after a successful createConn.
func (fc *flagCycler) Succeed() {
	fc.mu.Lock()
	fc.failures = 0
	fc.mu.Unlock()
}

// parseFlagStr converts a string like "PA" into a single-element []conf.TCPF.
func parseFlagStr(s string) []conf.TCPF {
	var f conf.TCPF
	for _, ch := range s {
		switch ch {
		case 'F':
			f.FIN = true
		case 'S':
			f.SYN = true
		case 'R':
			f.RST = true
		case 'P':
			f.PSH = true
		case 'A':
			f.ACK = true
		case 'U':
			f.URG = true
		case 'E':
			f.ECE = true
		case 'C':
			f.CWR = true
		case 'N':
			f.NS = true
		}
	}
	return []conf.TCPF{f}
}

// tcpfToStr converts the first element of a []conf.TCPF to a flag string like
// "PA".  Returns "PA" for an empty slice.
func tcpfToStr(flags []conf.TCPF) string {
	if len(flags) == 0 {
		return "PA"
	}
	f := flags[0]
	s := ""
	if f.FIN {
		s += "F"
	}
	if f.SYN {
		s += "S"
	}
	if f.RST {
		s += "R"
	}
	if f.PSH {
		s += "P"
	}
	if f.ACK {
		s += "A"
	}
	if f.URG {
		s += "U"
	}
	if f.ECE {
		s += "E"
	}
	if f.CWR {
		s += "C"
	}
	if f.NS {
		s += "N"
	}
	return s
}

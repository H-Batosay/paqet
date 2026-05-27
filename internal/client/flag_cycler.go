package client

import (
	"sync"

	"paqet/internal/conf"
	"paqet/internal/flog"
)

type flagPair struct{ lf, rf string }

// fallbackCombos is the ordered list of well-known flag pairs tried when the
// active combo fails.  It covers a wide range of firewall and DPI profiles:
//
//   - Established-session patterns (PSH+ACK, ACK): most permissive firewalls
//   - SYN/SYN-ACK: deep-inspection systems that track connection state
//   - FIN/FIN-ACK: close-sequence mimicry
//   - ECN flags (ECE, CWR): some networks allow ECN-flagged traffic selectively
//   - Mixed asymmetric combos: bypass stateful asymmetric DPI rules
//
// Entries that duplicate the user-configured combo are skipped at init time.
var fallbackCombos = []flagPair{
	// ── Established-session (ACK-based) ──────────────────────────────────────
	{"PA", "PA"},   // PSH+ACK / PSH+ACK     — most common data pattern
	{"A", "PA"},    // ACK     / PSH+ACK     — minimal client, data server
	{"PA", "A"},    // PSH+ACK / ACK         — data client, minimal server
	{"A", "A"},     // ACK     / ACK         — keepalive-like, very lightweight
	{"P", "PA"},    // PSH     / PSH+ACK     — no-ACK push

	// ── SYN / SYN-ACK handshake patterns ────────────────────────────────────
	{"S", "SA"},    // SYN / SYN-ACK         — clean handshake (stateful-friendly)
	{"SA", "PA"},   // SYN+ACK / PSH+ACK    — asymmetric: server-first look
	{"SA", "SA"},   // SYN+ACK / SYN+ACK    — both appear to be accepting

	// ── FIN / close-sequence patterns ────────────────────────────────────────
	{"FA", "FA"},   // FIN+ACK / FIN+ACK    — graceful close mimicry
	{"FA", "PA"},   // FIN+ACK / PSH+ACK    — client closing, server still sending
	{"FA", "A"},    // FIN+ACK / ACK        — client finishing, server ACKing
	{"PA", "FA"},   // PSH+ACK / FIN+ACK    — data client, server closing
	{"FPA", "PA"},  // FIN+PSH+ACK / PSH+ACK — unusual but valid combination

	// ── ECN / congestion-control flags ───────────────────────────────────────
	{"EA", "PA"},   // ECE+ACK / PSH+ACK    — ECN-aware client
	{"CA", "PA"},   // CWR+ACK / PSH+ACK    — congestion-window reduced
	{"PA", "EA"},   // PSH+ACK / ECE+ACK    — ECN-aware server
	{"PA", "CA"},   // PSH+ACK / CWR+ACK
	{"EA", "EA"},   // ECE+ACK / ECE+ACK    — both ECN

	// ── PSH+ACK / SYN+ACK asymmetric ─────────────────────────────────────────
	{"PA", "SA"},   // PSH+ACK / SYN+ACK    — looks like data to accepting server
}

// flagCycler manages automatic TCP flag switching for a single timedConn.
// It starts with the user-configured combo and falls back to fallbackCombos
// after maxFailures consecutive connection failures.
// All methods are safe for concurrent use.
type flagCycler struct {
	mu          sync.Mutex
	combos      []flagPair // ordered list; combos[0] is always the user config
	idx         int        // currently active combo index
	failures    int        // consecutive failures with the active combo
	maxFailures int        // failures needed to advance to the next combo
}

// newFlagCycler returns a cycler whose first entry is the user-configured
// combo (lf/rf).
//
// explicit=true (user set local_flag/remote_flag in config): only that combo
// is ever used — no fallback cycling.  The user has made an intentional
// choice; paqet honours it and never switches to another combination.
//
// explicit=false (no flags in config, defaults applied): all fallbackCombos
// are appended after the default so paqet can probe and auto-switch.
//
// maxFailures is the consecutive failure count before advancing to the next
// combo (configured via network.tcp.max_failures, default 3).
func newFlagCycler(lf, rf []conf.TCPF, explicit bool, maxFailures int) *flagCycler {
	userLF := tcpfToStr(lf)
	userRF := tcpfToStr(rf)

	combos := []flagPair{{userLF, userRF}}
	if !explicit {
		for _, c := range fallbackCombos {
			if c.lf == userLF && c.rf == userRF {
				continue // already covered by the user combo
			}
			combos = append(combos, c)
		}
	}
	return &flagCycler{combos: combos, maxFailures: maxFailures}
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

// Fail records one connection failure.  Once maxFailures is reached the
// cycler advances to the next combo and logs the switch.
func (fc *flagCycler) Fail() {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	fc.failures++
	if fc.failures < fc.maxFailures {
		return
	}
	old := fc.combos[fc.idx]
	fc.idx = (fc.idx + 1) % len(fc.combos)
	fc.failures = 0
	next := fc.combos[fc.idx]
	flog.Infof("auto flag switch: LF=%s RF=%s → LF=%s RF=%s (after %d consecutive failures)",
		old.lf, old.rf, next.lf, next.rf, fc.maxFailures)
}

// Succeed resets the consecutive failure counter without changing the active
// combo.  Call this after a successful createConn.
func (fc *flagCycler) Succeed() {
	fc.mu.Lock()
	fc.failures = 0
	fc.mu.Unlock()
}

// Failures returns the current consecutive failure count for the active combo.
func (fc *flagCycler) Failures() int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.failures
}

// MaxFailures returns the configured failure threshold before auto-switching.
func (fc *flagCycler) MaxFailures() int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.maxFailures
}

// Len returns the total number of flag combinations in the rotation.
func (fc *flagCycler) Len() int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return len(fc.combos)
}

// ForceNext advances to the next combo immediately, without incrementing the
// failure counter.  Used by the startup probe to sweep all combos quickly.
func (fc *flagCycler) ForceNext() {
	fc.mu.Lock()
	fc.idx = (fc.idx + 1) % len(fc.combos)
	fc.failures = 0
	fc.mu.Unlock()
}

// SetIdx sets the active combo by index and resets the failure counter.
// Used by the startup probe to restore the best combo found.
func (fc *flagCycler) SetIdx(i int) {
	fc.mu.Lock()
	if i >= 0 && i < len(fc.combos) {
		fc.idx = i
		fc.failures = 0
	}
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

package conf

import (
	"fmt"
	"time"
)

type TCP struct {
	LF_ []string `yaml:"local_flag"`
	RF_ []string `yaml:"remote_flag"`
	LF  []TCPF   `yaml:"-"`
	RF  []TCPF   `yaml:"-"`
	// ExplicitFlags is true when the user set local_flag or remote_flag in the
	// config.  When false, paqet probes all well-known flag combos at startup
	// and auto-switches on repeated failures.  When true it respects the user's
	// choice and never switches to a different combination.
	ExplicitFlags bool `yaml:"-"`

	// MaxFailures is the number of consecutive connection failures with the
	// active flag combination before the cycler switches to the next one.
	// Default: 3.  Minimum: 1.
	MaxFailures int `yaml:"max_failures"`

	// HealthInterval is how often (seconds) a background goroutine sends a
	// bidirectional ping to verify the tunnel is still alive.  If the ping
	// fails the connection is marked dead and the cycler records a failure.
	// Default: 30.  Set to -1 to disable health checks.
	HealthInterval_  int           `yaml:"health_interval"`
	HealthInterval   time.Duration `yaml:"-"`
}

type TCPF struct {
	FIN, SYN, RST, PSH, ACK, URG, ECE, CWR, NS bool
}

func (t *TCP) setDefaults() {
	// Record whether the user provided flags before we fill in defaults.
	t.ExplicitFlags = len(t.LF_) > 0 || len(t.RF_) > 0
	if len(t.LF_) == 0 {
		t.LF_ = []string{"PA"}
	}
	if len(t.RF_) == 0 {
		t.RF_ = []string{"PA"}
	}
	if t.MaxFailures == 0 {
		t.MaxFailures = 3
	}
	if t.HealthInterval_ == 0 {
		t.HealthInterval_ = 30 // default: check every 30 s
	}
}

func (t *TCP) validate() []error {
	var errors []error

	if len(t.LF_) != 0 {
		t.LF = make([]TCPF, len(t.LF_))
		for i, fStr := range t.LF_ {
			f, err := strTCPF(fStr)
			if err != nil {
				errors = append(errors, err)
			}
			t.LF[i] = f
		}
	}
	if len(t.RF_) != 0 {
		t.RF = make([]TCPF, len(t.RF_))
		for i, fStr := range t.RF_ {
			f, err := strTCPF(fStr)
			if err != nil {
				errors = append(errors, err)
			}
			t.RF[i] = f
		}
	}

	if len(t.LF) == 0 || len(t.RF) == 0 {
		errors = append(errors, fmt.Errorf("at least one TCP flag combination required"))
	}

	if t.MaxFailures < 1 {
		errors = append(errors, fmt.Errorf("max_failures must be >= 1"))
	}

	// HealthInterval_ < 0 → disabled (HealthInterval stays 0).
	// HealthInterval_ > 0 → convert to Duration.
	if t.HealthInterval_ > 0 {
		t.HealthInterval = time.Duration(t.HealthInterval_) * time.Second
	}

	return errors
}

func strTCPF(fStr string) (TCPF, error) {
	var f TCPF
	for _, ch := range fStr {
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
		default:
			return f, fmt.Errorf("invalid TCP flag '%c' in combination", ch)
		}
	}
	return f, nil
}

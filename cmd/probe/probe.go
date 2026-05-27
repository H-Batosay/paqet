package probe

import (
	"context"
	"fmt"
	"log"
	"paqet/internal/conf"
	"paqet/internal/protocol"
	"paqet/internal/socket"
	"paqet/internal/tnet/kcp"
	"time"

	"github.com/spf13/cobra"
)

var confPath string

func init() {
	Cmd.Flags().StringVarP(&confPath, "config", "c", "config.yaml", "Path to client configuration file.")
}

var Cmd = &cobra.Command{
	Use:   "probe",
	Short: "Tests TCP flag combinations to find which pass through the network.",
	Long: `probe connects to the configured server using different TCP flag combinations
and measures which ones work and their round-trip latency.

The results help you choose optimal local_flag / remote_flag values for your network.

Requires the server to be running.`,
	Run: func(cmd *cobra.Command, args []string) {
		runProbe()
	},
}

// flagCombo describes a flag pair to test.
type flagCombo struct {
	local  string // client → server TCP flags
	remote string // server → client TCP flags
	desc   string
}

// allCombos lists the flag combinations to probe.
var allCombos = []flagCombo{
	{"PA", "PA", "PSH+ACK / PSH+ACK  (default)"},
	{"A", "PA", "ACK → PSH+ACK"},
	{"P", "PA", "PSH → PSH+ACK"},
	{"FA", "FA", "FIN+ACK / FIN+ACK"},
	{"FA", "PA", "FIN+ACK → PSH+ACK"},
	{"S", "SA", "SYN → SYN+ACK  (handshake style)"},
	{"SA", "PA", "SYN+ACK → PSH+ACK"},
	{"EA", "PA", "ECE+ACK → PSH+ACK"},
	{"CA", "PA", "CWR+ACK → PSH+ACK"},
	{"FSPA", "FSPA", "All flags (obfuscated)"},
}

type result struct {
	combo   flagCombo
	ok      bool
	latency time.Duration
	errMsg  string
}

func runProbe() {
	cfg, err := conf.LoadFromFile(confPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	if cfg.Role != "client" {
		log.Fatalf("probe requires a client configuration")
	}

	fmt.Printf("\npaqet probe — testing %d flag combinations against %s\n\n",
		len(allCombos), cfg.Server.Addr)
	fmt.Printf("  %-22s  %-8s  %s\n", "Flags (local→remote)", "Latency", "Status")
	fmt.Printf("  %-22s  %-8s  %s\n", "─────────────────────", "───────", "──────")

	var best *result
	for _, combo := range allCombos {
		r := testCombo(cfg, combo)
		status := "✗ failed"
		latStr := "—"
		if r.ok {
			status = "✔ ok"
			latStr = r.latency.Round(time.Millisecond).String()
			if best == nil || r.latency < best.latency {
				best = &r
			}
		} else {
			status = fmt.Sprintf("✗ %s", r.errMsg)
		}
		fmt.Printf("  %-22s  %-8s  %s\n",
			fmt.Sprintf("%s / %s", combo.local, combo.remote), latStr, status)
	}

	fmt.Println()
	if best != nil {
		fmt.Printf("Recommended config (lowest latency):\n")
		fmt.Printf("  network:\n")
		fmt.Printf("    tcp:\n")
		fmt.Printf("      local_flag:  [\"%s\"]\n", best.combo.local)
		fmt.Printf("      remote_flag: [\"%s\"]\n", best.combo.remote)
	} else {
		fmt.Println("No flag combination succeeded. Check server connectivity and iptables rules.")
	}
	fmt.Println()
}

// testCombo attempts a full KCP connection + protocol ping with the given flags.
func testCombo(baseCfg *conf.Conf, combo flagCombo) result {
	r := result{combo: combo}

	lf, err := parseTCPF(combo.local)
	if err != nil {
		r.errMsg = err.Error()
		return r
	}
	rf, err := parseTCPF(combo.remote)
	if err != nil {
		r.errMsg = err.Error()
		return r
	}

	// Deep-copy the config and override flags.
	testCfg := *baseCfg
	testNet := baseCfg.Network
	testNet.TCP.LF = []conf.TCPF{lf}
	testNet.TCP.RF = []conf.TCPF{rf}
	testCfg.Network = testNet

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pConn, err := socket.New(ctx, &testCfg.Network)
	if err != nil {
		r.errMsg = fmt.Sprintf("socket: %v", err)
		return r
	}
	defer pConn.Close()

	conn, err := kcp.Dial(testCfg.Server.Addr, testCfg.Transport.KCP, pConn)
	if err != nil {
		r.errMsg = fmt.Sprintf("dial: %v", err)
		return r
	}
	defer conn.Close()

	// Tell server which flags to use when responding.
	strm, err := conn.OpenStrm()
	if err != nil {
		r.errMsg = fmt.Sprintf("open stream: %v", err)
		return r
	}
	p := protocol.Proto{Type: protocol.PTCPF, TCPF: testNet.TCP.RF}
	if err := p.Write(strm); err != nil {
		strm.Close()
		r.errMsg = fmt.Sprintf("send flags: %v", err)
		return r
	}
	strm.Close()

	start := time.Now()
	if err := conn.Ping(true); err != nil {
		r.errMsg = fmt.Sprintf("ping: %v", err)
		return r
	}
	r.ok = true
	r.latency = time.Since(start)
	return r
}

// parseTCPF converts a flag string ("PA", "S", etc.) to conf.TCPF.
func parseTCPF(s string) (conf.TCPF, error) {
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
		default:
			return f, fmt.Errorf("invalid TCP flag %q", ch)
		}
	}
	return f, nil
}

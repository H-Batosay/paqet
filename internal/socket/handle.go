package socket

import (
	"fmt"
	"paqet/internal/conf"
	"runtime"
	"time"

	"github.com/gopacket/gopacket/pcap"
)

func newHandle(cfg *conf.Network) (*pcap.Handle, error) {
	// On Windows, use the GUID field to construct the NPF device name
	// On other platforms, use the interface name directly
	ifaceName := cfg.Interface.Name
	if runtime.GOOS == "windows" {
		ifaceName = cfg.GUID
	}

	inactive, err := pcap.NewInactiveHandle(ifaceName)
	if err != nil {
		return nil, fmt.Errorf("failed to create inactive pcap handle for %s: %v", cfg.Interface.Name, err)
	}
	defer inactive.CleanUp()

	if err = inactive.SetBufferSize(cfg.PCAP.Sockbuf); err != nil {
		return nil, fmt.Errorf("failed to set pcap buffer size to %d: %v", cfg.PCAP.Sockbuf, err)
	}

	if err = inactive.SetSnapLen(65536); err != nil {
		return nil, fmt.Errorf("failed to set pcap snap length: %v", err)
	}
	// Promiscuous mode is NOT needed: we only want packets addressed to this
	// host.  Enabling it forces the NIC driver to deliver every frame on the
	// wire through the BPF filter (all hosts' traffic), which burns CPU even
	// when our port is completely idle.  Normal unicast traffic destined for
	// our IP always arrives with our MAC as the Ethernet destination, so
	// promisc=false captures everything we care about.
	if err = inactive.SetPromisc(false); err != nil {
		return nil, fmt.Errorf("failed to set promiscuous mode: %v", err)
	}
	// IMPORTANT: Do not block forever. A finite timeout lets ReadPacketData wake up
	// so callers can observe ctx cancellation / deadlines and exit cleanly.
	// ImmediateMode(true) is set below, so real packets are delivered instantly
	// regardless of this value — it only controls the idle wakeup frequency.
	// 1 s → 1 goroutine wake-up/sec per handle at idle (vs. 10/sec at 100 ms).
	if err = inactive.SetTimeout(1000 * time.Millisecond); err != nil {
		return nil, fmt.Errorf("failed to set pcap timeout: %v", err)
	}
	if err = inactive.SetImmediateMode(true); err != nil {
		return nil, fmt.Errorf("failed to enable immediate mode: %v", err)
	}

	handle, err := inactive.Activate()
	if err != nil {
		return nil, fmt.Errorf("failed to activate pcap handle on %s: %v", cfg.Interface.Name, err)
	}

	return handle, nil
}

package autoconf

import (
	"fmt"
	"net"
)

// Info holds auto-detected network parameters for the default-route interface.
// Fields that could not be determined are nil/empty — callers should check.
type Info struct {
	Interface     string           // OS interface name (e.g. "eth0", "en0")
	GUID          string           // Windows Npcap device name ("\Device\NPF_{...}")
	IPv4          net.IP           // local IPv4 address
	IPv4Gateway   net.IP           // IPv4 default gateway
	IPv4RouterMAC net.HardwareAddr // gateway MAC address
}

// interfaceByIPv4 returns the network interface that owns ip.
// Shared by all platform implementations.
func interfaceByIPv4(ip net.IP) (*net.Interface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for i := range ifaces {
		addrs, err := ifaces[i].Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ifIP net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ifIP = v.IP.To4()
			case *net.IPAddr:
				ifIP = v.IP.To4()
			}
			if ifIP != nil && ifIP.Equal(ip) {
				return &ifaces[i], nil
			}
		}
	}
	return nil, fmt.Errorf("no interface found for IPv4 %s", ip)
}

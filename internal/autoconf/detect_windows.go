//go:build windows

package autoconf

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"

	"github.com/gopacket/gopacket/pcap"
)

func Detect(destination string) (*Info, error) {
	if destination == "" {
		destination = "8.8.8.8:53"
	}

	conn, err := net.DialTimeout("udp4", destination, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("route probe failed: %w", err)
	}
	localIP := conn.LocalAddr().(*net.UDPAddr).IP.To4()
	conn.Close()
	if localIP == nil {
		return nil, fmt.Errorf("could not determine local IPv4 address")
	}

	iface, err := interfaceByIPv4(localIP)
	if err != nil {
		return nil, fmt.Errorf("interface lookup: %w", err)
	}

	// Find the Npcap device GUID by matching the local IP in pcap's device list.
	guid, _ := windowsFindGUID(localIP)

	gw, err := windowsDefaultGateway()
	if err != nil {
		return &Info{Interface: iface.Name, GUID: guid, IPv4: localIP}, nil
	}

	mac, err := windowsARPLookup(gw.String())
	if err != nil {
		windowsTriggerARP(gw)
		time.Sleep(300 * time.Millisecond)
		mac, _ = windowsARPLookup(gw.String())
	}

	return &Info{
		Interface:     iface.Name,
		GUID:          guid,
		IPv4:          localIP,
		IPv4Gateway:   gw,
		IPv4RouterMAC: mac,
	}, nil
}

func windowsFindGUID(ip net.IP) (string, error) {
	devices, err := pcap.FindAllDevs()
	if err != nil {
		return "", err
	}
	for _, dev := range devices {
		for _, addr := range dev.Addresses {
			if addr.IP != nil && addr.IP.Equal(ip) {
				return dev.Name, nil
			}
		}
	}
	return "", fmt.Errorf("Npcap device not found for IP %s", ip)
}

func windowsDefaultGateway() (net.IP, error) {
	out, err := exec.Command("route", "print", "0.0.0.0").Output()
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		// IPv4 Route Table line: 0.0.0.0  0.0.0.0  <gateway>  <iface>  <metric>
		if len(fields) >= 3 && fields[0] == "0.0.0.0" && fields[1] == "0.0.0.0" {
			gw := net.ParseIP(fields[2])
			if gw != nil {
				if ip4 := gw.To4(); ip4 != nil {
					return ip4, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("default gateway not found in route output")
}

func windowsARPLookup(ip string) (net.HardwareAddr, error) {
	out, err := exec.Command("arp", "-a", ip).Output()
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, ip) {
			continue
		}
		for _, field := range strings.Fields(line) {
			// Windows uses hyphens: aa-bb-cc-dd-ee-ff
			normalized := strings.ReplaceAll(field, "-", ":")
			if mac, err := net.ParseMAC(normalized); err == nil {
				return mac, nil
			}
		}
	}
	return nil, fmt.Errorf("ARP entry not found for %s", ip)
}

func windowsTriggerARP(gw net.IP) {
	conn, err := net.DialTimeout("udp4", gw.String()+":1", 200*time.Millisecond)
	if err == nil {
		conn.Write([]byte{0}) //nolint:errcheck
		conn.Close()
	}
}

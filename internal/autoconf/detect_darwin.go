//go:build darwin

package autoconf

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"
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

	gw, err := darwinDefaultGateway()
	if err != nil {
		return &Info{Interface: iface.Name, IPv4: localIP}, nil
	}

	mac, err := darwinARPLookup(gw.String())
	if err != nil {
		darwinTriggerARP(gw)
		time.Sleep(200 * time.Millisecond)
		mac, _ = darwinARPLookup(gw.String())
	}

	return &Info{
		Interface:     iface.Name,
		IPv4:          localIP,
		IPv4Gateway:   gw,
		IPv4RouterMAC: mac,
	}, nil
}

func darwinDefaultGateway() (net.IP, error) {
	out, err := exec.Command("route", "-n", "get", "default").Output()
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "gateway:") {
			gwStr := strings.TrimSpace(strings.TrimPrefix(line, "gateway:"))
			if ip := net.ParseIP(gwStr); ip != nil {
				if ip4 := ip.To4(); ip4 != nil {
					return ip4, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("gateway not found in route output")
}

func darwinARPLookup(ip string) (net.HardwareAddr, error) {
	out, err := exec.Command("arp", "-n", ip).Output()
	if err != nil {
		return nil, err
	}
	// Output: "? (192.168.1.1) at aa:bb:cc:dd:ee:ff on en0 ..."
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, " at ") {
			continue
		}
		parts := strings.SplitN(line, " at ", 2)
		if len(parts) != 2 {
			continue
		}
		macStr := strings.Fields(parts[1])[0]
		if macStr == "incomplete" || macStr == "(incomplete)" {
			continue
		}
		mac, err := net.ParseMAC(macStr)
		if err == nil {
			return mac, nil
		}
	}
	return nil, fmt.Errorf("ARP entry not found for %s", ip)
}

func darwinTriggerARP(gw net.IP) {
	conn, err := net.DialTimeout("udp4", gw.String()+":1", 200*time.Millisecond)
	if err == nil {
		conn.Write([]byte{0}) //nolint:errcheck
		conn.Close()
	}
}

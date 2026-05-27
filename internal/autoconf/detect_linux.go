//go:build linux

package autoconf

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// Detect discovers network parameters for the interface used to reach destination.
// destination must be "host:port" (e.g. "1.2.3.4:9999" or "8.8.8.8:53").
func Detect(destination string) (*Info, error) {
	if destination == "" {
		destination = "8.8.8.8:53"
	}

	// Route probe: use UDP dial to discover the outgoing local IP without
	// actually sending a packet.
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

	gw, err := linuxDefaultGateway(iface.Name)
	if err != nil {
		// Return what we have — MAC will be missing.
		return &Info{Interface: iface.Name, IPv4: localIP}, nil
	}

	mac, err := linuxARPLookup(gw.String(), iface.Name)
	if err != nil {
		// ARP entry not cached yet: send a packet to trigger ARP, then retry.
		linuxTriggerARP(gw)
		for i := 0; i < 5; i++ {
			time.Sleep(100 * time.Millisecond)
			mac, err = linuxARPLookup(gw.String(), iface.Name)
			if err == nil {
				break
			}
		}
	}

	return &Info{
		Interface:     iface.Name,
		IPv4:          localIP,
		IPv4Gateway:   gw,
		IPv4RouterMAC: mac, // may be nil if ARP resolution failed
	}, nil
}

// linuxDefaultGateway reads the IPv4 default gateway from /proc/net/route.
// /proc/net/route format (hex, little-endian):
//
//	Iface  Destination  Gateway  Flags  RefCnt  Use  Metric  Mask  MTU  Window  IRTT
func linuxDefaultGateway(iface string) (net.IP, error) {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Scan() // skip header
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			continue
		}
		// Default route: Destination == "00000000"
		if fields[1] != "00000000" {
			continue
		}
		b, err := hex.DecodeString(fields[2])
		if err != nil || len(b) != 4 {
			continue
		}
		// Little-endian bytes → big-endian IPv4
		gw := net.IP{b[3], b[2], b[1], b[0]}
		if !gw.IsUnspecified() {
			return gw, nil
		}
	}
	return nil, fmt.Errorf("default gateway not found in /proc/net/route")
}

// linuxARPLookup reads a completed ARP entry from /proc/net/arp.
func linuxARPLookup(ip, iface string) (net.HardwareAddr, error) {
	f, err := os.Open("/proc/net/arp")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Scan() // skip header
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 6 {
			continue
		}
		// fields: IP  HWtype  Flags  HWaddr  Mask  Device
		if fields[0] != ip {
			continue
		}
		if iface != "" && fields[5] != iface {
			continue
		}
		if fields[2] == "0x0" {
			continue // incomplete entry
		}
		mac, err := net.ParseMAC(fields[3])
		if err != nil {
			continue
		}
		return mac, nil
	}
	return nil, fmt.Errorf("ARP entry not found for %s on %s", ip, iface)
}

// linuxTriggerARP sends a single UDP byte to gw to trigger kernel ARP resolution.
func linuxTriggerARP(gw net.IP) {
	conn, err := net.DialTimeout("udp4", gw.String()+":1", 200*time.Millisecond)
	if err == nil {
		conn.Write([]byte{0}) //nolint:errcheck
		conn.Close()
	}
}

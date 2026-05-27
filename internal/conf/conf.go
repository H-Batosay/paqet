package conf

import (
	"fmt"
	"net"
	"os"
	"paqet/internal/autoconf"
	"paqet/internal/flog"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"
)

type Conf struct {
	Role      string    `yaml:"role"`
	Log       Log       `yaml:"log"`
	Listen    Server    `yaml:"listen"`
	SOCKS5    []SOCKS5  `yaml:"socks5"`
	Forward   []Forward `yaml:"forward"`
	Network   Network   `yaml:"network"`
	Server    Server    `yaml:"server"`
	Transport Transport `yaml:"transport"`
}

func LoadFromFile(path string) (*Conf, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var conf Conf

	if err := yaml.Unmarshal(data, &conf); err != nil {
		return &conf, err
	}

	validRoles := []string{"client", "server"}
	if !slices.Contains(validRoles, conf.Role) {
		return nil, fmt.Errorf("role must be 'client' or 'server'")
	}

	conf.setDefaults()
	if err := conf.validate(); err != nil {
		return &conf, err
	}

	return &conf, nil
}

func (c *Conf) setDefaults() {
	c.Log.setDefaults()
	c.Listen.setDefaults()
	for i := range c.SOCKS5 {
		c.SOCKS5[i].setDefaults()
	}
	for i := range c.Forward {
		c.Forward[i].setDefaults()
	}
	// Auto-detect network before Network.setDefaults so that detected values
	// are available for the rest of the config initialisation.
	c.autoDetectNetwork()
	c.Network.setDefaults(c.Role)
	c.Server.setDefaults()
	c.Transport.setDefaults(c.Role)
}

// autoDetectNetwork fills in missing Network fields by probing the OS.
// It only touches fields that are empty in the YAML config so explicit
// user values are never overridden.
func (c *Conf) autoDetectNetwork() {
	needInterface := c.Network.Interface_ == ""
	needIPv4 := c.Network.IPv4.Addr_ == ""
	needMac := c.Network.IPv4.RouterMac_ == ""
	needGUID := runtime.GOOS == "windows" && c.Network.GUID == ""

	if !needInterface && !needIPv4 && !needMac && !needGUID {
		return
	}

	// Use server address as routing hint on the client so we probe the
	// correct outgoing interface.  Fall back to a public address for the
	// server role.
	hint := "8.8.8.8:53"
	if c.Role == "client" && c.Server.Addr_ != "" {
		hint = c.Server.Addr_
	}

	info, err := autoconf.Detect(hint)
	if err != nil {
		flog.Debugf("network auto-detect skipped: %v", err)
		return
	}

	if needInterface && info.Interface != "" {
		flog.Infof("auto-detect: interface = %s", info.Interface)
		c.Network.Interface_ = info.Interface
	}
	if needGUID && info.GUID != "" {
		flog.Infof("auto-detect: guid = %s", info.GUID)
		c.Network.GUID = info.GUID
	}
	if needIPv4 && info.IPv4 != nil {
		port := 0 // client: random ephemeral port
		if c.Role == "server" && c.Listen.Addr_ != "" {
			// Extract port from listen address string (e.g. ":9999" → 9999)
			if _, portStr, err := net.SplitHostPort(c.Listen.Addr_); err == nil {
				if p, err := strconv.Atoi(portStr); err == nil {
					port = p
				}
			}
		}
		c.Network.IPv4.Addr_ = fmt.Sprintf("%s:%d", info.IPv4, port)
		flog.Infof("auto-detect: ipv4 = %s", c.Network.IPv4.Addr_)
	}
	if needMac && info.IPv4RouterMAC != nil {
		flog.Infof("auto-detect: router_mac = %s", info.IPv4RouterMAC)
		c.Network.IPv4.RouterMac_ = info.IPv4RouterMAC.String()
	}
}

func (c *Conf) validate() error {
	var allErrors []error

	allErrors = append(allErrors, c.Log.validate()...)
	if c.Role == "client" && len(c.SOCKS5) == 0 && len(c.Forward) == 0 {
		flog.Warnf("warning: client mode enabled but no SOCKS5 or forward configurations found")
	}
	for i := range c.SOCKS5 {
		errs := c.SOCKS5[i].validate()
		for _, err := range errs {
			allErrors = append(allErrors, fmt.Errorf("socks5[%d] %v", i, err))
		}
	}

	for i := range c.Forward {
		errs := c.Forward[i].validate()
		for _, err := range errs {
			allErrors = append(allErrors, fmt.Errorf("forward[%d] %v", i, err))
		}
	}

	allErrors = append(allErrors, c.Network.validate()...)
	allErrors = append(allErrors, c.Transport.validate()...)
	if c.Role == "server" {
		allErrors = append(allErrors, c.Listen.validate()...)
	} else {
		allErrors = append(allErrors, c.Server.validate()...)
		if c.Server.Addr.IP.To4() != nil && c.Network.IPv4.Addr == nil {
			allErrors = append(allErrors, fmt.Errorf("server address is IPv4, but the IPv4 interface is not configured"))
		}
		if c.Server.Addr.IP.To4() == nil && c.Network.IPv6.Addr == nil {
			allErrors = append(allErrors, fmt.Errorf("server address is IPv6, but the IPv6 interface is not configured"))
		}
		if c.Transport.Conn > 1 && c.Network.Port != 0 {
			allErrors = append(allErrors, fmt.Errorf("only one connection is allowed when a client port is explicitly set"))
		}
	}
	return writeErr(allErrors)
}

func writeErr(allErrors []error) error {
	if len(allErrors) > 0 {
		var messages []string
		for _, err := range allErrors {
			messages = append(messages, err.Error())
		}
		return fmt.Errorf("validation failed:\n  - %s", strings.Join(messages, "\n  - "))
	}
	return nil
}

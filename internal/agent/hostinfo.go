package agent

import (
	"net"
	"os"
	"strings"
	"time"
)

// HostInfo holds static host metadata included in every agent report.
type HostInfo struct {
	Hostname string
	IP       string
	Timezone string
}

// HostInfoConfig makes host info collection injectable for tests.
type HostInfoConfig struct {
	// TimezonePath is read for the timezone name (e.g. /etc/timezone).
	TimezonePath string
	// HostnameFunc returns the host name; defaults to os.Hostname.
	HostnameFunc func() (string, error)
	// InterfacesFunc lists network interfaces; defaults to net.Interfaces.
	InterfacesFunc func() ([]net.Interface, error)
}

// DefaultHostInfoConfig returns a HostInfoConfig backed by the real system.
func DefaultHostInfoConfig() HostInfoConfig {
	return HostInfoConfig{
		TimezonePath:   "/etc/timezone",
		HostnameFunc:   os.Hostname,
		InterfacesFunc: net.Interfaces,
	}
}

// CollectHostInfo gathers hostname, first non-loopback IPv4 and timezone.
func CollectHostInfo(cfg HostInfoConfig) HostInfo {
	hi := HostInfo{Timezone: detectTimezone(cfg.TimezonePath)}
	if cfg.HostnameFunc != nil {
		if h, err := cfg.HostnameFunc(); err == nil {
			hi.Hostname = h
		}
	}
	if cfg.InterfacesFunc != nil {
		hi.IP = firstIPv4(cfg.InterfacesFunc)
	}
	return hi
}

// firstIPv4 returns the first non-loopback IPv4 address of the host.
func firstIPv4(list func() ([]net.Interface, error)) string {
	ifaces, err := list()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip4 := ip.To4(); ip4 != nil && !ip4.IsLoopback() {
				return ip4.String()
			}
		}
	}
	return ""
}

// detectTimezone reads the timezone file; falls back to the local zone name,
// then to "UTC".
func detectTimezone(path string) string {
	if path != "" {
		if content, err := os.ReadFile(path); err == nil {
			if tz := strings.TrimSpace(string(content)); tz != "" {
				return tz
			}
		}
	}
	if name, _ := time.Now().In(time.Local).Zone(); name != "" {
		return name
	}
	return "UTC"
}

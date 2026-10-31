package main

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Server struct {
		Bind    string `toml:"bind"`
		WAN     string `toml:"wan"`
		TLSCert string `toml:"tls_cert"`
		TLSKey  string `toml:"tls_key"`
	} `toml:"server"`
	Share struct {
		DefaultExpiry       string `toml:"default_expiry"`
		DefaultMaxDownloads *int   `toml:"default_max_downloads"`
	} `toml:"share"`
}

const defaultBind = "0.0.0.0:8443"

const configTemplate = `# quickshare configuration

[server]
# Address the daemon listens on (inside this machine).
bind = "0.0.0.0:8443"

# Public address put in links: your public IP or hostname plus the port
# the outside world connects to. Required unless this machine has a
# public IP on one of its interfaces (then it is detected).
# wan = "203.0.113.5:8443"

# Optional HTTPS. Both must be set.
# tls_cert = "/etc/letsencrypt/live/example.com/fullchain.pem"
# tls_key  = "/etc/letsencrypt/live/example.com/privkey.pem"

[share]
default_expiry        = "24h"
default_max_downloads = 1
`

// loadConfig reads the config file. A missing file is not an error: a
// commented template is written so the user has something to edit.
func loadConfig(p Paths) (Config, error) {
	var c Config
	data, err := os.ReadFile(p.Config)
	if errors.Is(err, os.ErrNotExist) {
		if p.ensure() == nil {
			os.WriteFile(p.Config, []byte(configTemplate), 0o600)
		}
		data = []byte(configTemplate)
	} else if err != nil {
		return c, err
	}
	if _, err := toml.Decode(string(data), &c); err != nil {
		return c, fmt.Errorf("%s: %w", p.Config, err)
	}
	if c.Server.Bind == "" {
		c.Server.Bind = defaultBind
	}
	if c.Share.DefaultExpiry == "" {
		c.Share.DefaultExpiry = "24h"
	}
	if c.Share.DefaultMaxDownloads == nil {
		one := 1
		c.Share.DefaultMaxDownloads = &one
	}
	return c, nil
}

// parseDuration accepts Go durations ("90m", "1h30m") plus whole days ("7d").
func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "0" {
		return 0, nil
	}
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (examples: 30m, 24h, 7d)", s)
	}
	if d < 0 {
		return 0, fmt.Errorf("duration %q is negative", s)
	}
	return d, nil
}

// validateLimits enforces that a share always ends somehow, unless the
// user explicitly asks for a share that never ends.
func validateLimits(maxDownloads int, expires time.Duration, allowForever bool) error {
	if maxDownloads < 0 {
		return errors.New("--max-downloads cannot be negative")
	}
	if expires < 0 {
		return errors.New("--expires cannot be negative")
	}
	if maxDownloads == 0 && expires == 0 && !allowForever {
		return errors.New("--max-downloads 0 with --expires 0 would share forever; add --allow-forever if you mean it")
	}
	if expires > 0 && expires < time.Second {
		return errors.New("--expires must be at least 1s")
	}
	return nil
}

// checkAddresses returns warnings about a bind/wan combination that is
// unlikely to work. It never blocks the share.
func checkAddresses(bind, wan string) []string {
	var warns []string
	bHost, bPort, err := net.SplitHostPort(bind)
	if err != nil {
		return []string{fmt.Sprintf("bind %q is not host:port", bind)}
	}
	wHost, wPort, err := net.SplitHostPort(wan)
	if err != nil {
		return []string{fmt.Sprintf("wan %q is not host:port", wan)}
	}

	if bPort != wPort {
		warns = append(warns, fmt.Sprintf("bind port %s differs from wan port %s: make sure your router/NAT forwards %s → %s", bPort, wPort, wPort, bPort))
	}

	bAddr, bErr := netip.ParseAddr(bHost)
	wAddr, wErr := netip.ParseAddr(wHost)
	bindAll := bHost == "" || (bErr == nil && bAddr.IsUnspecified())

	if wErr == nil {
		switch {
		case wAddr.IsUnspecified():
			warns = append(warns, fmt.Sprintf("wan %s is not a real address; set it to your public IP or hostname", wHost))
		case wAddr.IsLoopback():
			warns = append(warns, fmt.Sprintf("wan %s is a loopback address: the link only works on this machine", wHost))
		case wAddr.IsPrivate() || wAddr.IsLinkLocalUnicast() || isCGNAT(wAddr):
			warns = append(warns, fmt.Sprintf("wan %s is a private address: the link only works on your local network", wHost))
		}
	}

	if bErr == nil && !bindAll {
		switch {
		case bAddr.IsLoopback() && !(wErr == nil && wAddr.IsLoopback()):
			warns = append(warns, fmt.Sprintf("bind %s only accepts connections from this machine; remote downloads will fail unless a proxy forwards to it", bHost))
		case !bAddr.IsLoopback() && !bAddr.IsPrivate() && wErr == nil && !wAddr.IsLoopback() && !wAddr.IsPrivate() && bAddr != wAddr:
			warns = append(warns, fmt.Sprintf("bind %s is a public address that differs from wan %s", bHost, wHost))
		}
	}
	return warns
}

// isCGNAT reports addresses in 100.64.0.0/10 (carrier-grade NAT).
func isCGNAT(a netip.Addr) bool {
	return netip.MustParsePrefix("100.64.0.0/10").Contains(a)
}

// detectPublicIP looks for exactly one public IPv4 address on this
// machine's interfaces. No external service is contacted.
func detectPublicIP() (string, bool) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", false
	}
	var found []string
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(ipn.IP)
		if !ok {
			continue
		}
		ip = ip.Unmap()
		if ip.Is4() && ip.IsGlobalUnicast() && !ip.IsPrivate() && !isCGNAT(ip) {
			found = append(found, ip.String())
		}
	}
	if len(found) == 1 {
		return found[0], true
	}
	return "", false
}

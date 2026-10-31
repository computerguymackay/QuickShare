package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestValidateLimits(t *testing.T) {
	cases := []struct {
		max     int
		exp     time.Duration
		forever bool
		ok      bool
	}{
		{1, 24 * time.Hour, false, true},
		{0, time.Hour, false, true}, // unlimited downloads, expires
		{3, 0, false, true},         // no expiry, limited downloads
		{0, 0, false, false},        // forever refused
		{0, 0, true, true},          // ...unless explicitly allowed
		{-1, time.Hour, false, false},
	}
	for _, c := range cases {
		err := validateLimits(c.max, c.exp, c.forever)
		if (err == nil) != c.ok {
			t.Errorf("validateLimits(%d, %s, %v) = %v, want ok=%v", c.max, c.exp, c.forever, err, c.ok)
		}
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"0": 0, "30s": 30 * time.Second, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour} {
		got, err := parseDuration(in)
		if err != nil || got != want {
			t.Errorf("parseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "-5m", "xd"} {
		if _, err := parseDuration(bad); err == nil {
			t.Errorf("parseDuration(%q) accepted", bad)
		}
	}
}

func TestCheckAddresses(t *testing.T) {
	cases := []struct {
		bind, wan string
		want      []string // substrings, one per expected warning
	}{
		{"0.0.0.0:8443", "203.0.113.5:8443", nil},
		{"0.0.0.0:8443", "files.example.com:8443", nil},
		{"0.0.0.0:8443", "203.0.113.5:443", []string{"differs from wan port"}},
		{"0.0.0.0:8443", "192.168.1.10:8443", []string{"private address"}},
		{"0.0.0.0:8443", "10.0.0.2:8443", []string{"private address"}},
		{"0.0.0.0:8443", "100.64.1.1:8443", []string{"private address"}},
		{"0.0.0.0:8443", "127.0.0.1:8443", []string{"loopback"}},
		{"127.0.0.1:8443", "203.0.113.5:8443", []string{"only accepts connections from this machine"}},
		{"127.0.0.1:8443", "127.0.0.1:8443", []string{"loopback"}},
		{"198.51.100.7:8443", "203.0.113.5:8443", []string{"public address that differs"}},
		{"198.51.100.7:8443", "203.0.113.5:9000", []string{"differs from wan port", "public address that differs"}},
		{"0.0.0.0:8443", "0.0.0.0:8443", []string{"not a real address"}},
		{"nonsense", "203.0.113.5:8443", []string{"not host:port"}},
	}
	for _, c := range cases {
		got := checkAddresses(c.bind, c.wan)
		if len(got) != len(c.want) {
			t.Errorf("checkAddresses(%s, %s) = %q, want %d warning(s)", c.bind, c.wan, got, len(c.want))
			continue
		}
		for i, w := range c.want {
			if !strings.Contains(got[i], w) {
				t.Errorf("checkAddresses(%s, %s)[%d] = %q, want it to mention %q", c.bind, c.wan, i, got[i], w)
			}
		}
	}
}

func TestLoadConfigWritesTemplate(t *testing.T) {
	t.Setenv("QUICKSHARE_HOME", t.TempDir())
	p, _ := resolvePaths()
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Bind != defaultBind || cfg.Server.WAN != "" || *cfg.Share.DefaultMaxDownloads != 1 || cfg.Share.DefaultExpiry != "24h" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if !fileExists(p.Config) {
		t.Fatal("config template not written")
	}

	os.WriteFile(p.Config, []byte("[server]\nwan = \"203.0.113.5:9000\"\n[share]\ndefault_max_downloads = 0\n"), 0o600)
	cfg, err = loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.WAN != "203.0.113.5:9000" || *cfg.Share.DefaultMaxDownloads != 0 {
		t.Fatalf("config not read: %+v", cfg)
	}
}

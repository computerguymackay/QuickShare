package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type DaemonOptions struct {
	Bind    string
	TLSCert string
	TLSKey  string
	Grace   time.Duration // wait this long with no active shares before exiting
	Tick    time.Duration // how often to check for expiry
}

func defaultDaemonOptions() DaemonOptions {
	return DaemonOptions{
		Bind:  defaultBind,
		Grace: envDuration("QUICKSHARE_GRACE", 45*time.Second),
		Tick:  envDuration("QUICKSHARE_TICK", 10*time.Second),
	}
}

func envDuration(name string, def time.Duration) time.Duration {
	if v := os.Getenv(name); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

var errAlreadyRunning = errors.New("another daemon is already running")

type Daemon struct {
	p   Paths
	opt DaemonOptions
	log *log.Logger

	store    *Store
	fs       *FileServer
	ctrlKey  string
	httpAddr string

	mu         sync.Mutex
	graceTimer *time.Timer

	stopOnce sync.Once
	stop     chan struct{}
}

func newDaemon(p Paths, opt DaemonOptions, logger *log.Logger) *Daemon {
	return &Daemon{p: p, opt: opt, log: logger, stop: make(chan struct{})}
}

// Run serves until ctx is cancelled, a stop command arrives, or no
// shares have been active for the grace period.
func (d *Daemon) Run(ctx context.Context) error {
	if err := d.p.ensure(); err != nil {
		return err
	}
	release, err := lockFile(d.p.DaemonLock, false)
	if errors.Is(err, errLocked) {
		return errAlreadyRunning
	}
	if err != nil {
		return fmt.Errorf("daemon lock: %w", err)
	}
	defer release()

	d.store, err = openStore(d.p.DB)
	if err != nil {
		return err
	}
	defer d.store.Close()

	// Restore state: drop anything that can no longer be served.
	now := time.Now()
	removed, err := d.store.Prune(now)
	if err != nil {
		return err
	}
	for _, sh := range removed {
		d.log.Printf("dropped %s on startup (%s)", shortToken(sh.Token), deadReason(sh, now))
	}
	if n, _ := d.store.CountActive(now); n > 0 {
		d.log.Printf("restored %d share(s)", n)
	}

	d.fs = &FileServer{store: d.store, log: d.log, now: time.Now, onChange: d.evaluate}
	srv := &http.Server{
		Handler:           d.fs,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          d.log,
	}

	hl, err := net.Listen("tcp", d.opt.Bind)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", d.opt.Bind, err)
	}
	scheme := "http"
	if d.opt.TLSCert != "" {
		cert, err := tls.LoadX509KeyPair(d.opt.TLSCert, d.opt.TLSKey)
		if err != nil {
			hl.Close()
			return fmt.Errorf("load TLS certificate: %w", err)
		}
		hl = tls.NewListener(hl, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
		scheme = "https"
	}
	d.httpAddr = hl.Addr().String()

	cl, key, err := listenControl(d.p)
	if err != nil {
		hl.Close()
		return fmt.Errorf("control socket: %w", err)
	}
	d.ctrlKey = key
	defer cleanupControl(d.p)

	pid := strconv.Itoa(os.Getpid())
	os.WriteFile(d.p.PID, []byte(pid+"\n"), 0o600)
	defer func() {
		if b, _ := os.ReadFile(d.p.PID); strings.TrimSpace(string(b)) == pid {
			os.Remove(d.p.PID)
		}
	}()

	d.log.Printf("daemon started pid=%s serving %s://%s", pid, scheme, d.httpAddr)

	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.Serve(hl) }()
	go d.serveControl(cl)

	ticker := time.NewTicker(d.opt.Tick)
	defer ticker.Stop()
	d.evaluate()

	var exitErr error
loop:
	for {
		select {
		case <-ctx.Done():
			d.log.Printf("signal received")
			break loop
		case <-d.stop:
			break loop
		case err := <-srvErr:
			exitErr = fmt.Errorf("http server: %w", err)
			break loop
		case <-ticker.C:
			d.prune()
			d.evaluate()
		}
	}

	cl.Close()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		srv.Close()
	}
	d.log.Printf("daemon stopped")
	return exitErr
}

func (d *Daemon) shutdown(reason string) {
	d.stopOnce.Do(func() {
		d.log.Printf("shutting down: %s", reason)
		close(d.stop)
	})
}

func (d *Daemon) prune() {
	now := time.Now()
	removed, err := d.store.Prune(now)
	if err != nil {
		d.log.Printf("prune: %v", err)
	}
	for _, sh := range removed {
		d.log.Printf("share %s ended (%s)", shortToken(sh.Token), deadReason(sh, now))
	}
}

func deadReason(sh Share, now time.Time) string {
	switch {
	case !fileExists(sh.Path):
		return "file missing"
	case sh.ExpiresAt != 0 && now.Unix() >= sh.ExpiresAt:
		return "expired"
	case sh.MaxDownloads != 0 && sh.Count >= sh.MaxDownloads:
		return "download limit reached"
	}
	return "inactive"
}

// idle is true when no share is active and no request is being served.
// Callers hold d.mu.
func (d *Daemon) idle() bool {
	n, err := d.store.CountActive(time.Now())
	if err != nil {
		d.log.Printf("count active: %v", err)
		return false
	}
	return n == 0 && d.fs.InFlight() == 0
}

// evaluate starts the grace timer when idle and cancels it otherwise.
// It runs after every request, every control change and every tick.
func (d *Daemon) evaluate() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.idle() {
		if d.graceTimer != nil {
			d.graceTimer.Stop()
			d.graceTimer = nil
			d.log.Printf("shutdown cancelled: shares are active again")
		}
		return
	}
	if d.graceTimer != nil {
		return
	}
	d.log.Printf("no active shares; exiting in %s unless a new share arrives", d.opt.Grace)
	var t *time.Timer
	t = time.AfterFunc(d.opt.Grace, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.graceTimer != t {
			return
		}
		d.graceTimer = nil
		if d.idle() {
			d.shutdown("no active shares")
		}
	})
	d.graceTimer = t
}

func (d *Daemon) serveControl(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			c.SetDeadline(time.Now().Add(10 * time.Second))
			var req ctrlRequest
			if err := json.NewDecoder(c).Decode(&req); err != nil {
				return
			}
			resp := d.handleControl(req)
			json.NewEncoder(c).Encode(resp)
			if req.Op == "stop" && resp.OK {
				d.shutdown("stop requested")
			}
		}(c)
	}
}

func (d *Daemon) handleControl(req ctrlRequest) ctrlResponse {
	fail := func(format string, a ...any) ctrlResponse {
		return ctrlResponse{Error: fmt.Sprintf(format, a...)}
	}
	if d.ctrlKey != "" && req.Key != d.ctrlKey {
		return fail("unauthorized")
	}
	switch req.Op {
	case "status":
		n, _ := d.store.CountActive(time.Now())
		return ctrlResponse{OK: true, Status: &DaemonStatus{
			PID: os.Getpid(), Bind: d.opt.Bind, Addr: d.httpAddr,
			TLS: d.opt.TLSCert != "", Active: n, Version: version,
		}}

	case "add-share":
		if !filepath.IsAbs(req.Path) || !fileExists(req.Path) {
			return fail("not a readable file: %s", req.Path)
		}
		if req.MaxDownloads < 0 || req.ExpiresAt < 0 {
			return fail("invalid limits")
		}
		tok, err := newToken()
		if err != nil {
			return fail("token: %v", err)
		}
		sh := Share{Token: tok, Path: req.Path, Created: time.Now().Unix(),
			ExpiresAt: req.ExpiresAt, MaxDownloads: req.MaxDownloads}
		if err := d.store.Add(sh); err != nil {
			return fail("save share: %v", err)
		}
		d.log.Printf("share %s added: %s (%s)", shortToken(tok), filepath.Base(sh.Path), describeLimits(sh))
		d.evaluate()
		return ctrlResponse{OK: true, Share: &sh}

	case "list":
		all, err := d.store.List()
		if err != nil {
			return fail("%v", err)
		}
		now := time.Now()
		var live []Share
		for _, sh := range all {
			if sh.Active(now) {
				live = append(live, sh)
			}
		}
		return ctrlResponse{OK: true, Shares: live}

	case "revoke":
		all, err := d.store.List()
		if err != nil {
			return fail("%v", err)
		}
		tok, err := resolveToken(all, req.Token)
		if err != nil {
			return fail("%v", err)
		}
		d.store.Delete(tok)
		d.log.Printf("share %s revoked", shortToken(tok))
		d.evaluate()
		return ctrlResponse{OK: true, Share: &Share{Token: tok}}

	case "stop":
		return ctrlResponse{OK: true}
	}
	return fail("unknown op %q", req.Op)
}

func describeLimits(sh Share) string {
	var parts []string
	if sh.MaxDownloads == 0 {
		parts = append(parts, "unlimited downloads")
	} else if sh.MaxDownloads == 1 {
		parts = append(parts, "1 download")
	} else {
		parts = append(parts, itoa(sh.MaxDownloads)+" downloads")
	}
	if sh.ExpiresAt == 0 {
		parts = append(parts, "no expiry")
	} else {
		parts = append(parts, "expires "+time.Unix(sh.ExpiresAt, 0).Format("2006-01-02 15:04 MST"))
	}
	return strings.Join(parts, ", ")
}

func itoa(n int) string { return strconv.Itoa(n) }

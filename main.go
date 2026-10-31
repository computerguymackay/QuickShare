// Command quickshare shares a file over HTTP from a machine with a
// reachable public IP. See README.md.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

const version = "0.1.0"

const usage = `quickshare — share a file over HTTP with a one-time link

Usage:
  quickshare <file> [flags]     share a file and print its link
  quickshare list [--json]      show active shares
  quickshare revoke <token>     cancel a share (a unique prefix is enough)
  quickshare stop               stop the background daemon

Share flags:
  --max-downloads N   downloads allowed (default 1; 0 = unlimited, needs --expires)
  --expires DURATION  e.g. 30m, 24h, 7d (default 24h; 0 = never, needs --max-downloads)
  --wan host:port     public address used in the link (overrides config)
  --bind host:port    address the daemon listens on (overrides config)
  --interactive       prompt for options
  --qr                show the link as a QR code (on stderr)
  --qr-out PATH       save the QR code as a PNG
  --json              print {url, token, expires_at, max_downloads}
  --watch             follow the daemon log after sharing
  --tls cert,key      serve over HTTPS with this certificate
  --allow-forever     allow --max-downloads 0 together with --expires 0

The link is always the last line on stdout, so link=$(quickshare f.zip) works.
Config: ~/.quickshare/config.toml (or $QUICKSHARE_HOME/config.toml)
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "list":
		return cmdList(args[1:], stdout, stderr)
	case "revoke":
		return cmdRevoke(args[1:], stdout, stderr)
	case "stop":
		return cmdStop(stdout, stderr)
	case "__daemon":
		return cmdDaemon(args[1:])
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	case "version", "--version":
		fmt.Fprintln(stdout, "quickshare", version)
		return 0
	case "--":
		args = args[1:]
	}
	return cmdShare(args, stdin, stdout, stderr)
}

func errorf(w io.Writer, format string, a ...any) int {
	fmt.Fprintf(w, "quickshare: "+format+"\n", a...)
	return 1
}

func warnf(w io.Writer, format string, a ...any) {
	fmt.Fprintf(w, "warning: "+format+"\n", a...)
}

// parseInterspersed lets flags appear before or after the file name.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		if args[0] == "--" {
			return append(pos, args[1:]...), nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func cmdShare(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("quickshare", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	maxFlag := fs.Int("max-downloads", 1, "")
	expFlag := fs.String("expires", "", "")
	wanFlag := fs.String("wan", "", "")
	bindFlag := fs.String("bind", "", "")
	interactive := fs.Bool("interactive", false, "")
	showQR := fs.Bool("qr", false, "")
	qrOut := fs.String("qr-out", "", "")
	jsonOut := fs.Bool("json", false, "")
	watch := fs.Bool("watch", false, "")
	tlsFlag := fs.String("tls", "", "")
	allowForever := fs.Bool("allow-forever", false, "")

	pos, err := parseInterspersed(fs, args)
	if err != nil {
		fmt.Fprintf(stderr, "quickshare: %v\n\n%s", err, usage)
		return 2
	}
	if len(pos) != 1 {
		fmt.Fprintf(stderr, "quickshare: expected exactly one file\n\n%s", usage)
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	p, err := resolvePaths()
	if err != nil {
		return errorf(stderr, "%v", err)
	}
	cfg, err := loadConfig(p)
	if err != nil {
		return errorf(stderr, "config: %v", err)
	}

	// The file.
	path, err := filepath.Abs(pos[0])
	if err != nil {
		return errorf(stderr, "%v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return errorf(stderr, "%v", err)
	}
	if !fi.Mode().IsRegular() {
		return errorf(stderr, "%s is not a regular file (directories: zip or tar them first)", pos[0])
	}
	if f, err := os.Open(path); err != nil {
		return errorf(stderr, "%v", err)
	} else {
		f.Close()
	}

	// Limits.
	maxDl := *cfg.Share.DefaultMaxDownloads
	if set["max-downloads"] {
		maxDl = *maxFlag
	}
	expStr := cfg.Share.DefaultExpiry
	if set["expires"] {
		expStr = *expFlag
	}
	expires, err := parseDuration(expStr)
	if err != nil {
		return errorf(stderr, "--expires: %v", err)
	}
	if *interactive {
		maxDl, expires, *showQR, err = promptOptions(stdin, stderr, maxDl, expires, *showQR)
		if err != nil {
			return errorf(stderr, "%v", err)
		}
	}
	if err := validateLimits(maxDl, expires, *allowForever); err != nil {
		return errorf(stderr, "%v", err)
	}

	// Addresses.
	bind := cfg.Server.Bind
	if *bindFlag != "" {
		bind = *bindFlag
	}
	wan := cfg.Server.WAN
	if *wanFlag != "" {
		wan = *wanFlag
	}
	if wan == "" {
		_, port, err := net.SplitHostPort(bind)
		ip, ok := detectPublicIP()
		if err != nil || !ok {
			return errorf(stderr, "no public address configured.\nSet wan in %s or pass --wan host:port (your public IP or hostname and port).", p.Config)
		}
		wan = net.JoinHostPort(ip, port)
		fmt.Fprintf(stderr, "using detected public address %s (set wan in config to change)\n", wan)
	}
	for _, w := range checkAddresses(bind, wan) {
		warnf(stderr, "%s", w)
	}

	// TLS.
	opt := defaultDaemonOptions()
	opt.Bind = bind
	opt.TLSCert, opt.TLSKey = cfg.Server.TLSCert, cfg.Server.TLSKey
	if *tlsFlag != "" {
		c, k, ok := strings.Cut(*tlsFlag, ",")
		if !ok || c == "" || k == "" {
			return errorf(stderr, "--tls expects cert,key")
		}
		opt.TLSCert, opt.TLSKey = c, k
	}
	if (opt.TLSCert == "") != (opt.TLSKey == "") {
		return errorf(stderr, "TLS needs both a certificate and a key")
	}
	if opt.TLSCert != "" {
		for _, f := range []*string{&opt.TLSCert, &opt.TLSKey} {
			if *f, err = filepath.Abs(*f); err != nil || !fileExists(*f) {
				return errorf(stderr, "TLS file not found: %s", *f)
			}
		}
		if _, err := tls.LoadX509KeyPair(opt.TLSCert, opt.TLSKey); err != nil {
			return errorf(stderr, "TLS: %v", err)
		}
	}

	// Daemon.
	logOffset := fileSize(p.Log)
	st, spawned, err := ensureDaemon(p, opt)
	if err != nil {
		return errorf(stderr, "%v", err)
	}
	if !spawned {
		if st.Bind != opt.Bind {
			warnf(stderr, "daemon is already running on %s; bind %s applies after `quickshare stop`", st.Bind, opt.Bind)
		}
		if opt.TLSCert != "" && !st.TLS {
			warnf(stderr, "daemon is already running without TLS; run `quickshare stop` first to switch to HTTPS")
		}
	}
	scheme := "http"
	if st.TLS {
		scheme = "https"
	}

	var expiresAt int64
	if expires > 0 {
		expiresAt = time.Now().Add(expires).Unix()
	}
	resp, err := ctrlCall(p, ctrlRequest{Op: "add-share", Path: path, ExpiresAt: expiresAt, MaxDownloads: maxDl})
	if err != nil {
		return errorf(stderr, "add share: %v", err)
	}
	sh := *resp.Share
	link := fmt.Sprintf("%s://%s/%s/%s", scheme, wan, sh.Token, url.PathEscape(filepath.Base(path)))

	if !st.TLS {
		warnf(stderr, "plain HTTP: anyone on the network path can read this file. Encrypt sensitive files first (e.g. age, gpg).")
	}
	if spawned {
		if err := selfCheck(scheme, wan); err != nil {
			warnf(stderr, "could not reach %s from this machine (%v).\n  If downloads fail, check your firewall and NAT port forward (see README).\n  Many routers can't loop back to their own public IP, so this may be a false alarm.", wan, err)
		}
	}

	if *jsonOut {
		out := struct {
			URL          string  `json:"url"`
			Token        string  `json:"token"`
			ExpiresAt    *string `json:"expires_at"`
			MaxDownloads int     `json:"max_downloads"`
		}{URL: link, Token: sh.Token, MaxDownloads: sh.MaxDownloads}
		if sh.ExpiresAt != 0 {
			s := time.Unix(sh.ExpiresAt, 0).UTC().Format(time.RFC3339)
			out.ExpiresAt = &s
		}
		enc := json.NewEncoder(stdout)
		enc.Encode(out)
	} else {
		fmt.Fprintf(stderr, "sharing %s (%s) · %s\n", filepath.Base(path), humanSize(fi.Size()), describeLimits(sh))
		fmt.Fprintln(stdout, link)
	}

	if *showQR {
		fmt.Fprintln(stderr)
		color := false
		if f, ok := stderr.(*os.File); ok {
			color = isTerminal(f)
		}
		if err := renderQR(stderr, link, color); err != nil {
			warnf(stderr, "QR: %v", err)
		}
	}
	if *qrOut != "" {
		if err := writeQRPNG(*qrOut, link); err != nil {
			return errorf(stderr, "write QR: %v", err)
		}
		fmt.Fprintf(stderr, "QR code saved to %s\n", *qrOut)
	}

	if *watch {
		watchLog(p, logOffset, stderr)
	}
	return 0
}

func promptOptions(stdin io.Reader, w io.Writer, maxDl int, exp time.Duration, qr bool) (int, time.Duration, bool, error) {
	r := bufio.NewReader(stdin)
	ask := func(q, def string) string {
		fmt.Fprintf(w, "%s [%s]: ", q, def)
		line, _ := r.ReadString('\n')
		if line = strings.TrimSpace(line); line == "" {
			return def
		}
		return line
	}
	for {
		v := ask("Max downloads (0 = unlimited)", itoa(maxDl))
		var n int
		if _, err := fmt.Sscan(v, &n); err == nil && n >= 0 {
			maxDl = n
			break
		}
		fmt.Fprintln(w, "  enter a whole number")
	}
	for {
		def := "0"
		if exp > 0 {
			def = formatDuration(exp)
		}
		d, err := parseDuration(ask("Expires after (e.g. 30m, 24h, 7d; 0 = never)", def))
		if err == nil {
			exp = d
			break
		}
		fmt.Fprintln(w, " ", err)
	}
	def := "n"
	if qr {
		def = "y"
	}
	ans := strings.ToLower(ask("Show QR code? (y/n)", def))
	return maxDl, exp, strings.HasPrefix(ans, "y"), nil
}

// selfCheck makes one request to the public address. Any HTTP response
// (even 410) means the port is reachable.
func selfCheck(scheme, wan string) error {
	c := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	resp, err := c.Head(scheme + "://" + wan + "/")
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return err
	}
	resp.Body.Close()
	return nil
}

func watchLog(p Paths, offset int64, w io.Writer) {
	fmt.Fprintf(w, "\nwatching %s (Ctrl-C to stop watching; the daemon keeps running)\n", p.Log)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)

	f, err := os.Open(p.Log)
	if err != nil {
		warnf(w, "%v", err)
		return
	}
	defer f.Close()
	f.Seek(offset, io.SeekStart)
	buf := make([]byte, 32<<10)
	drain := func() {
		for {
			n, _ := f.Read(buf)
			if n == 0 {
				return
			}
			w.Write(buf[:n])
		}
	}
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	checks := 0
	for {
		select {
		case <-sig:
			fmt.Fprintln(w)
			return
		case <-tick.C:
			drain()
			if checks++; checks%3 == 0 && !daemonAlive(p) {
				drain()
				fmt.Fprintln(w, "daemon has exited")
				return
			}
		}
	}
}

func cmdList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "output JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	p, err := resolvePaths()
	if err != nil {
		return errorf(stderr, "%v", err)
	}
	var shares []Share
	running := true
	resp, err := ctrlCall(p, ctrlRequest{Op: "list"})
	switch {
	case errors.Is(err, errDaemonDown):
		running = false
		shares, err = offlineShares(p)
		if err != nil {
			return errorf(stderr, "%v", err)
		}
	case err != nil:
		return errorf(stderr, "%v", err)
	default:
		shares = resp.Shares
	}

	if *jsonOut {
		if shares == nil {
			shares = []Share{}
		}
		json.NewEncoder(stdout).Encode(shares)
		return 0
	}
	if !running && len(shares) > 0 {
		fmt.Fprintln(stderr, "daemon is not running: these links are offline until the next `quickshare <file>` starts it")
	}
	if len(shares) == 0 {
		fmt.Fprintln(stderr, "no active shares")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TOKEN\tFILE\tDOWNLOADS\tEXPIRES")
	now := time.Now()
	for _, sh := range shares {
		exp := "never"
		if sh.ExpiresAt != 0 {
			exp = "in " + humanDuration(time.Unix(sh.ExpiresAt, 0).Sub(now))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", sh.Token, sh.Path, countString(sh), exp)
	}
	tw.Flush()
	return 0
}

// offlineShares reads the database directly when no daemon is running.
func offlineShares(p Paths) ([]Share, error) {
	if !fileExists(p.DB) {
		return nil, nil
	}
	st, err := openStore(p.DB)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	all, err := st.List()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var live []Share
	for _, sh := range all {
		if sh.Active(now) && fileExists(sh.Path) {
			live = append(live, sh)
		}
	}
	return live, nil
}

func cmdRevoke(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: quickshare revoke <token>")
		return 2
	}
	p, err := resolvePaths()
	if err != nil {
		return errorf(stderr, "%v", err)
	}
	resp, err := ctrlCall(p, ctrlRequest{Op: "revoke", Token: args[0]})
	if errors.Is(err, errDaemonDown) {
		tok, err := revokeOffline(p, args[0])
		if err != nil {
			return errorf(stderr, "%v", err)
		}
		fmt.Fprintf(stdout, "revoked %s\n", tok)
		return 0
	}
	if err != nil {
		return errorf(stderr, "%v", err)
	}
	fmt.Fprintf(stdout, "revoked %s\n", resp.Share.Token)
	return 0
}

func revokeOffline(p Paths, q string) (string, error) {
	if !fileExists(p.DB) {
		return "", fmt.Errorf("no share matches %q", q)
	}
	st, err := openStore(p.DB)
	if err != nil {
		return "", err
	}
	defer st.Close()
	all, err := st.List()
	if err != nil {
		return "", err
	}
	tok, err := resolveToken(all, q)
	if err != nil {
		return "", err
	}
	_, err = st.Delete(tok)
	return tok, err
}

func cmdStop(stdout, stderr io.Writer) int {
	p, err := resolvePaths()
	if err != nil {
		return errorf(stderr, "%v", err)
	}
	st, err := daemonStatus(p)
	if err != nil {
		if daemonAlive(p) {
			return errorf(stderr, "a daemon holds %s but is not answering; check %s", p.DaemonLock, p.Log)
		}
		clearStale(p)
		fmt.Fprintln(stdout, "daemon is not running")
		return 0
	}
	if _, err := ctrlCall(p, ctrlRequest{Op: "stop"}); err != nil {
		return errorf(stderr, "%v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for daemonAlive(p) {
		if time.Now().After(deadline) {
			return errorf(stderr, "daemon (pid %d) did not exit within 10s", st.PID)
		}
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Fprintf(stdout, "daemon stopped (pid %d)\n", st.PID)
	if st.Active > 0 {
		fmt.Fprintf(stderr, "%d share(s) kept: they come back online next time the daemon starts. Use `quickshare revoke <token>` to cancel them.\n", st.Active)
	}
	return 0
}

func cmdDaemon(args []string) int {
	fs := flag.NewFlagSet("__daemon", flag.ContinueOnError)
	opt := defaultDaemonOptions()
	fs.StringVar(&opt.Bind, "bind", opt.Bind, "")
	fs.StringVar(&opt.TLSCert, "tls-cert", "", "")
	fs.StringVar(&opt.TLSKey, "tls-key", "", "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	p, err := resolvePaths()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	logger := log.New(os.Stderr, "", log.LstdFlags)
	ctx, cancel := signal.NotifyContext(context.Background(), stopSignals...)
	defer cancel()
	err = newDaemon(p, opt, logger).Run(ctx)
	if errors.Is(err, errAlreadyRunning) {
		logger.Printf("%v; exiting", err)
		return 0
	}
	if err != nil {
		logger.Printf("error: %v", err)
		return 1
	}
	return 0
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// formatDuration prints a duration the way a user would type it.
func formatDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour && d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return d.String()
}

func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Round(time.Minute).Minutes()))
	}
	if d < 48*time.Hour {
		m := int(d.Round(time.Minute).Minutes())
		return fmt.Sprintf("%dh%02dm", m/60, m%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

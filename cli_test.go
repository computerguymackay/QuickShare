//go:build !windows

package main

// End-to-end tests that build the real binary and run it, so daemon
// spawning, locking and crash recovery are exercised for real.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var qsBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "qsbin")
	if err != nil {
		panic(err)
	}
	qsBinary = filepath.Join(dir, "quickshare")
	if out, err := exec.Command("go", "build", "-o", qsBinary, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build failed: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type cliEnv struct {
	home, addr string
}

func newCLIEnv(t *testing.T) *cliEnv {
	t.Helper()
	home, err := os.MkdirTemp("", "qs")
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	e := &cliEnv{home: home, addr: addr}
	t.Cleanup(func() {
		e.run(t, "stop")
		os.RemoveAll(home)
	})
	return e
}

func (e *cliEnv) run(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(qsBinary, args...)
	cmd.Env = append(os.Environ(), "QUICKSHARE_HOME="+e.home, "QUICKSHARE_GRACE=1s", "QUICKSHARE_TICK=200ms")
	var o, er bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &er
	err := cmd.Run()
	code = 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return o.String(), er.String(), code
}

func (e *cliEnv) share(t *testing.T, file string, extra ...string) string {
	t.Helper()
	args := append([]string{file, "--wan", e.addr, "--bind", e.addr}, extra...)
	out, errOut, code := e.run(t, args...)
	if code != 0 {
		t.Fatalf("share failed (%d): %s", code, errOut)
	}
	return strings.TrimSpace(out)
}

func (e *cliEnv) list(t *testing.T) []Share {
	t.Helper()
	out, errOut, code := e.run(t, "list", "--json")
	if code != 0 {
		t.Fatalf("list failed: %s", errOut)
	}
	var shares []Share
	if err := json.Unmarshal([]byte(out), &shares); err != nil {
		t.Fatalf("list output %q: %v", out, err)
	}
	return shares
}

func (e *cliEnv) daemonPID(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.home, "daemon.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

func (e *cliEnv) waitDaemonGone(t *testing.T, within time.Duration) {
	t.Helper()
	p := Paths{DaemonLock: filepath.Join(e.home, "daemon.lock")}
	deadline := time.Now().Add(within)
	for daemonAlive(p) {
		if time.Now().After(deadline) {
			t.Fatal("daemon still running")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func download(t *testing.T, link string) (int, string) {
	t.Helper()
	resp, err := http.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(b)
}

func TestCLIShareDownloadAndAutoStop(t *testing.T) {
	e := newCLIEnv(t)
	f := testFile(t, "hello")
	link := e.share(t, f)
	if !strings.HasPrefix(link, "http://"+e.addr+"/") || strings.Contains(link, "\n") {
		t.Fatalf("stdout should be just the link, got %q", link)
	}
	if code, body := download(t, link); code != 200 || body != "hello" {
		t.Fatalf("download: %d %q", code, body)
	}
	if code, _ := download(t, link); code != 410 {
		t.Fatalf("second download: %d, want 410", code)
	}
	e.waitDaemonGone(t, 4*time.Second)
}

func TestCLIQRKeepsLinkOnStdout(t *testing.T) {
	e := newCLIEnv(t)
	png := filepath.Join(t.TempDir(), "qr.png")
	out, errOut, code := e.run(t, testFile(t, "x"), "--wan", e.addr, "--bind", e.addr, "--qr", "--qr-out", png)
	if code != 0 {
		t.Fatal(errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "http://") {
		t.Fatalf("stdout = %q, want only the link", out)
	}
	if !strings.Contains(errOut, "█") {
		t.Fatal("QR code not rendered on stderr")
	}
	checkQRPNG(t, png, lines[0])
}

func TestCLIJSON(t *testing.T) {
	e := newCLIEnv(t)
	out := e.share(t, testFile(t, "x"), "--json", "--max-downloads", "3", "--expires", "1h")
	var v struct {
		URL          string  `json:"url"`
		Token        string  `json:"token"`
		ExpiresAt    *string `json:"expires_at"`
		MaxDownloads int     `json:"max_downloads"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("%q: %v", out, err)
	}
	if v.MaxDownloads != 3 || v.ExpiresAt == nil || !strings.Contains(v.URL, v.Token) {
		t.Fatalf("unexpected JSON: %+v", v)
	}
	exp, err := time.Parse(time.RFC3339, *v.ExpiresAt)
	if err != nil || exp.Sub(time.Now()) < 59*time.Minute {
		t.Fatalf("expires_at %q", *v.ExpiresAt)
	}
}

func TestCLIForeverNeedsFlag(t *testing.T) {
	e := newCLIEnv(t)
	f := testFile(t, "x")
	_, errOut, code := e.run(t, f, "--wan", e.addr, "--bind", e.addr, "--max-downloads", "0", "--expires", "0")
	if code == 0 || !strings.Contains(errOut, "--allow-forever") {
		t.Fatalf("forever share accepted: code %d, %s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(e.home, "daemon.pid")); err == nil {
		t.Fatal("daemon started for a refused share")
	}
	e.share(t, f, "--max-downloads", "0", "--expires", "0", "--allow-forever")
	if s := e.list(t); len(s) != 1 || s[0].MaxDownloads != 0 || s[0].ExpiresAt != 0 {
		t.Fatalf("forever share: %+v", s)
	}
}

func TestCLIParallelInvocationsShareOneDaemon(t *testing.T) {
	e := newCLIEnv(t)
	f := testFile(t, "x")
	const n = 8
	links := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			links[i] = e.share(t, f)
		}(i)
	}
	wg.Wait()
	if got := len(e.list(t)); got != n {
		t.Fatalf("daemon has %d shares, want %d (were two daemons started?)", got, n)
	}
	for _, l := range links {
		if code, _ := download(t, l); code != 200 {
			t.Fatalf("download %s: %d", l, code)
		}
	}
	log, _ := os.ReadFile(filepath.Join(e.home, "quickshare.log"))
	if c := strings.Count(string(log), "daemon started"); c != 1 {
		t.Fatalf("daemon started %d times, want 1", c)
	}
}

func TestCLIStalePIDAndSocket(t *testing.T) {
	e := newCLIEnv(t)
	os.WriteFile(filepath.Join(e.home, "daemon.pid"), []byte("999999\n"), 0o600)
	os.WriteFile(filepath.Join(e.home, "control.sock"), []byte("stale"), 0o600)

	link := e.share(t, testFile(t, "x"))
	if code, _ := download(t, link); code != 200 {
		t.Fatalf("download: %d", code)
	}
	if pid := e.daemonPID(t); pid == 999999 {
		t.Fatal("stale PID file not replaced")
	}
}

func TestCLICrashRestoresShares(t *testing.T) {
	e := newCLIEnv(t)
	f := testFile(t, "data")
	first := e.share(t, f, "--max-downloads", "2")
	expired := e.share(t, f, "--expires", "1s")
	time.Sleep(1100 * time.Millisecond)

	syscall.Kill(e.daemonPID(t), syscall.SIGKILL)
	e.waitDaemonGone(t, 2*time.Second)
	if resp, err := http.Get(first); err == nil {
		resp.Body.Close()
		t.Fatal("daemon still serving after kill")
	}

	second := e.share(t, f) // starts a fresh daemon
	shares := e.list(t)
	if len(shares) != 2 {
		t.Fatalf("after restart: %d shares, want 2: %+v", len(shares), shares)
	}
	for _, l := range []string{first, second} {
		if code, _ := download(t, l); code != 200 {
			t.Fatalf("download %s after restart: %d", l, code)
		}
	}
	if code, _ := download(t, expired); code != 410 {
		t.Fatalf("expired share after restart: %d, want 410", code)
	}
}

func TestCLIStopListRevoke(t *testing.T) {
	e := newCLIEnv(t)
	f := testFile(t, "x")
	link := e.share(t, f)
	tok := strings.Split(strings.TrimPrefix(link, "http://"+e.addr+"/"), "/")[0]

	out, _, code := e.run(t, "stop")
	if code != 0 || !strings.Contains(out, "daemon stopped") {
		t.Fatalf("stop: %d %q", code, out)
	}
	e.waitDaemonGone(t, 2*time.Second)

	// Offline list and revoke work against the database.
	if s := e.list(t); len(s) != 1 || s[0].Token != tok {
		t.Fatalf("offline list: %+v", s)
	}
	if out, errOut, code := e.run(t, "revoke", tok[:10]); code != 0 || !strings.Contains(out, tok) {
		t.Fatalf("revoke: %d %q %q", code, out, errOut)
	}
	if s := e.list(t); len(s) != 0 {
		t.Fatalf("after revoke: %+v", s)
	}
	if out, _, _ := e.run(t, "stop"); !strings.Contains(out, "not running") {
		t.Fatalf("stop when stopped: %q", out)
	}
}

func TestCLIBindMismatchWarning(t *testing.T) {
	e := newCLIEnv(t)
	_, errOut, code := e.run(t, testFile(t, "x"), "--wan", "192.168.1.5:9999", "--bind", e.addr)
	if code != 0 {
		t.Fatal(errOut)
	}
	for _, want := range []string{"private address", "differs from wan port", "plain HTTP"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr missing %q:\n%s", want, errOut)
		}
	}
}

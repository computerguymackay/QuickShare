package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"testing"
	"time"
)

// testPaths returns paths under a short temp dir (Unix socket paths
// have a length limit).
func testPaths(t *testing.T) Paths {
	t.Helper()
	dir, err := os.MkdirTemp("", "qs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("QUICKSHARE_HOME", dir)
	p, err := resolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type runningDaemon struct {
	p    Paths
	addr string
	done chan error
}

func startTestDaemon(t *testing.T, p Paths, grace time.Duration) *runningDaemon {
	t.Helper()
	opt := DaemonOptions{Bind: "127.0.0.1:0", Grace: grace, Tick: 100 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	rd := &runningDaemon{p: p, done: make(chan error, 1)}
	go func() { rd.done <- newDaemon(p, opt, log.New(io.Discard, "", 0)).Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-rd.done:
		case <-time.After(5 * time.Second):
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := daemonStatus(p); err == nil {
			rd.addr = st.Addr
			return rd
		}
		select {
		case err := <-rd.done:
			t.Fatalf("daemon exited early: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatal("daemon did not start")
	return nil
}

func (rd *runningDaemon) add(t *testing.T, path string, max int, expiresAt int64) Share {
	t.Helper()
	resp, err := ctrlCall(rd.p, ctrlRequest{Op: "add-share", Path: path, MaxDownloads: max, ExpiresAt: expiresAt})
	if err != nil {
		t.Fatal(err)
	}
	return *resp.Share
}

func (rd *runningDaemon) get(t *testing.T, tok string) int {
	t.Helper()
	resp, err := http.Get("http://" + rd.addr + "/" + tok)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func (rd *runningDaemon) exitsWithin(d time.Duration) bool {
	select {
	case <-rd.done:
		return true
	case <-time.After(d):
		return false
	}
}

func TestDaemonExitsAfterLastDownload(t *testing.T) {
	p := testPaths(t)
	rd := startTestDaemon(t, p, 500*time.Millisecond)
	sh := rd.add(t, testFile(t, "data"), 1, 0)
	if rd.exitsWithin(800 * time.Millisecond) {
		t.Fatal("daemon exited while a share was active")
	}
	if code := rd.get(t, sh.Token); code != 200 {
		t.Fatalf("download: %d", code)
	}
	if rd.exitsWithin(200 * time.Millisecond) {
		t.Fatal("daemon exited before the grace period")
	}
	if !rd.exitsWithin(2 * time.Second) {
		t.Fatal("daemon did not exit after the download limit was reached")
	}
}

func TestDaemonExitsOnExpiry(t *testing.T) {
	p := testPaths(t)
	rd := startTestDaemon(t, p, 200*time.Millisecond)
	rd.add(t, testFile(t, "data"), 1, time.Now().Add(2*time.Second).Unix())
	if rd.exitsWithin(time.Second) {
		t.Fatal("daemon exited before the share expired")
	}
	if !rd.exitsWithin(3 * time.Second) {
		t.Fatal("daemon did not exit after expiry")
	}
}

func TestOneDaemonManySharesExitsWhenLastEnds(t *testing.T) {
	p := testPaths(t)
	rd := startTestDaemon(t, p, 300*time.Millisecond)
	a := rd.add(t, testFile(t, "a"), 1, 0)
	b := rd.add(t, testFile(t, "b"), 1, 0)

	rd.get(t, a.Token)
	if rd.exitsWithin(time.Second) {
		t.Fatal("daemon exited with one share still active")
	}
	if resp, err := ctrlCall(p, ctrlRequest{Op: "list"}); err != nil || len(resp.Shares) != 1 {
		t.Fatalf("list after first download: %+v %v", resp.Shares, err)
	}
	rd.get(t, b.Token)
	if !rd.exitsWithin(2 * time.Second) {
		t.Fatal("daemon did not exit after the last share ended")
	}
}

func TestNewShareCancelsGrace(t *testing.T) {
	p := testPaths(t)
	rd := startTestDaemon(t, p, 600*time.Millisecond)
	a := rd.add(t, testFile(t, "a"), 1, 0)
	rd.get(t, a.Token) // grace starts
	time.Sleep(300 * time.Millisecond)
	rd.add(t, testFile(t, "b"), 1, 0)
	if rd.exitsWithin(time.Second) {
		t.Fatal("daemon exited even though a new share arrived during the grace period")
	}
}

func TestRevokeEndsShare(t *testing.T) {
	p := testPaths(t)
	rd := startTestDaemon(t, p, 200*time.Millisecond)
	sh := rd.add(t, testFile(t, "a"), 1, 0)
	if _, err := ctrlCall(p, ctrlRequest{Op: "revoke", Token: sh.Token[:8]}); err != nil {
		t.Fatal(err)
	}
	if code := rd.get(t, sh.Token); code != 410 {
		t.Fatalf("revoked share: %d, want 410", code)
	}
	if !rd.exitsWithin(2 * time.Second) {
		t.Fatal("daemon did not exit after revoking the only share")
	}
}

func TestRestartRestoresLiveShares(t *testing.T) {
	p := testPaths(t)
	if err := p.ensure(); err != nil {
		t.Fatal(err)
	}
	f := testFile(t, "data")
	st, err := openStore(p.DB)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	st.Add(Share{Token: "live-share-000000000000", Path: f, MaxDownloads: 1, ExpiresAt: now.Add(time.Hour).Unix()})
	st.Add(Share{Token: "expired-share-000000000", Path: f, MaxDownloads: 1, ExpiresAt: now.Add(-time.Minute).Unix()})
	st.Add(Share{Token: "missing-file-0000000000", Path: "/nonexistent/x", MaxDownloads: 1})
	st.Close()

	rd := startTestDaemon(t, p, time.Second)
	resp, err := ctrlCall(p, ctrlRequest{Op: "list"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Shares) != 1 || resp.Shares[0].Token != "live-share-000000000000" {
		t.Fatalf("restored shares = %+v, want only the live one", resp.Shares)
	}
	if code := rd.get(t, "live-share-000000000000"); code != 200 {
		t.Fatalf("restored share download: %d", code)
	}
	if code := rd.get(t, "expired-share-000000000"); code != 410 {
		t.Fatalf("expired share: %d, want 410", code)
	}
}

func TestSecondDaemonRefuses(t *testing.T) {
	p := testPaths(t)
	startTestDaemon(t, p, time.Minute)
	opt := DaemonOptions{Bind: "127.0.0.1:0", Grace: time.Minute, Tick: time.Second}
	err := newDaemon(p, opt, log.New(io.Discard, "", 0)).Run(context.Background())
	if err != errAlreadyRunning {
		t.Fatalf("second daemon: %v, want errAlreadyRunning", err)
	}
	if _, err := daemonStatus(p); err != nil {
		t.Fatalf("first daemon disturbed: %v", err)
	}
}

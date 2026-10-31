package main

import (
	"bufio"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type serverEnv struct {
	store *Store
	srv   *httptest.Server
	done  chan struct{} // receives once per finished request
}

func newServerEnv(t *testing.T) *serverEnv {
	t.Helper()
	e := &serverEnv{store: testStore(t), done: make(chan struct{}, 100)}
	fs := &FileServer{store: e.store, log: log.New(io.Discard, "", 0), now: time.Now,
		onChange: func() { e.done <- struct{}{} }}
	e.srv = httptest.NewServer(fs)
	t.Cleanup(e.srv.Close)
	return e
}

func (e *serverEnv) share(t *testing.T, path string, max int, expiresAt int64) string {
	t.Helper()
	tok, _ := newToken()
	if err := e.store.Add(Share{Token: tok, Path: path, MaxDownloads: max, ExpiresAt: expiresAt}); err != nil {
		t.Fatal(err)
	}
	return tok
}

func (e *serverEnv) count(t *testing.T, tok string) int {
	t.Helper()
	sh, err := e.store.Get(tok)
	if err != nil {
		t.Fatal(err)
	}
	return sh.Count
}

func (e *serverEnv) do(t *testing.T, method, tok string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+"/"+tok+"/file.txt", nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	<-e.done
	return resp, string(body)
}

const content = "0123456789abcdefghij"

func TestFullGetCounts(t *testing.T) {
	e := newServerEnv(t)
	tok := e.share(t, testFile(t, content), 5, 0)
	resp, body := e.do(t, "GET", tok, nil)
	if resp.StatusCode != 200 || body != content {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	if c := e.count(t, tok); c != 1 {
		t.Fatalf("count = %d, want 1", c)
	}
	for k, v := range map[string]string{"Referrer-Policy": "no-referrer", "Cache-Control": "no-store"} {
		if resp.Header.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, resp.Header.Get(k), v)
		}
	}
	if !strings.Contains(resp.Header.Get("Content-Disposition"), `filename=file.txt`) {
		t.Errorf("Content-Disposition = %q", resp.Header.Get("Content-Disposition"))
	}
}

func TestRangeFromZeroDoesNotCount(t *testing.T) {
	e := newServerEnv(t)
	tok := e.share(t, testFile(t, content), 5, 0)
	resp, body := e.do(t, "GET", tok, map[string]string{"Range": "bytes=0-"})
	if resp.StatusCode != 206 || body != content {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	if c := e.count(t, tok); c != 0 {
		t.Fatalf("count = %d, want 0", c)
	}
}

func TestMidFileRangeDoesNotCount(t *testing.T) {
	e := newServerEnv(t)
	tok := e.share(t, testFile(t, content), 5, 0)
	resp, body := e.do(t, "GET", tok, map[string]string{"Range": "bytes=10-"})
	if resp.StatusCode != 206 || body != content[10:] {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	if c := e.count(t, tok); c != 0 {
		t.Fatalf("count = %d, want 0", c)
	}
}

func TestHeadDoesNotCount(t *testing.T) {
	e := newServerEnv(t)
	tok := e.share(t, testFile(t, content), 1, 0)
	resp, _ := e.do(t, "HEAD", tok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if c := e.count(t, tok); c != 0 {
		t.Fatalf("count = %d, want 0", c)
	}
	// The single allowed download is still available.
	if resp, _ := e.do(t, "GET", tok, nil); resp.StatusCode != 200 {
		t.Fatalf("GET after HEAD: %d", resp.StatusCode)
	}
}

func TestTwoFullDownloadsSameIPCountTwice(t *testing.T) {
	e := newServerEnv(t)
	tok := e.share(t, testFile(t, content), 5, 0)
	e.do(t, "GET", tok, nil)
	e.do(t, "GET", tok, nil)
	if c := e.count(t, tok); c != 2 {
		t.Fatalf("count = %d, want 2", c)
	}
}

func TestExhaustedAndUnknownAndExpiredAreGone(t *testing.T) {
	e := newServerEnv(t)
	f := testFile(t, content)
	tok := e.share(t, f, 1, 0)
	e.do(t, "GET", tok, nil)
	if resp, _ := e.do(t, "GET", tok, nil); resp.StatusCode != 410 {
		t.Fatalf("exhausted: %d, want 410", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", "nosuchtoken", nil); resp.StatusCode != 410 {
		t.Fatalf("unknown: %d, want 410", resp.StatusCode)
	}
	old := e.share(t, f, 1, time.Now().Add(-time.Second).Unix())
	if resp, _ := e.do(t, "GET", old, nil); resp.StatusCode != 410 {
		t.Fatalf("expired: %d, want 410", resp.StatusCode)
	}
	gone := e.share(t, filepath.Join(t.TempDir(), "deleted"), 1, 0)
	if resp, _ := e.do(t, "GET", gone, nil); resp.StatusCode != 410 {
		t.Fatalf("missing file: %d, want 410", resp.StatusCode)
	}
}

func TestInterruptedThenRetriedCountsOnce(t *testing.T) {
	e := newServerEnv(t)
	// Bigger than any socket buffer, so the server sees the disconnect.
	big := filepath.Join(t.TempDir(), "big.bin")
	f, _ := os.Create(big)
	f.Truncate(64 << 20)
	f.Close()
	tok := e.share(t, big, 5, 0)

	conn, err := net.Dial("tcp", strings.TrimPrefix(e.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(conn, "GET /"+tok+"/big.bin HTTP/1.1\r\nHost: x\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("partial download: %v %v", resp, err)
	}
	io.CopyN(io.Discard, resp.Body, 1<<20)
	conn.Close()

	select {
	case <-e.done:
	case <-time.After(10 * time.Second):
		t.Fatal("handler did not finish after disconnect")
	}
	if c := e.count(t, tok); c != 0 {
		t.Fatalf("count after interrupted download = %d, want 0", c)
	}

	resp2, err := http.Get(e.srv.URL + "/" + tok + "/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	n, _ := io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	<-e.done
	if n != 64<<20 {
		t.Fatalf("retry got %d bytes", n)
	}
	if c := e.count(t, tok); c != 1 {
		t.Fatalf("count after retry = %d, want 1", c)
	}
}

func TestPostNotAllowed(t *testing.T) {
	e := newServerEnv(t)
	tok := e.share(t, testFile(t, content), 1, 0)
	if resp, _ := e.do(t, "POST", tok, nil); resp.StatusCode != 405 {
		t.Fatalf("POST: %d, want 405", resp.StatusCode)
	}
	if c := e.count(t, tok); c != 0 {
		t.Fatalf("count = %d", c)
	}
}

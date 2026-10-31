package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	st, err := openStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func testFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestIncrementClampsAtMax(t *testing.T) {
	st := testStore(t)
	st.Add(Share{Token: "tok", Path: "/x", MaxDownloads: 2})
	for i := 0; i < 5; i++ {
		st.Increment("tok")
	}
	sh, _ := st.Get("tok")
	if sh.Count != 2 {
		t.Fatalf("count = %d, want 2", sh.Count)
	}
}

func TestIncrementUnlimited(t *testing.T) {
	st := testStore(t)
	st.Add(Share{Token: "tok", Path: "/x", MaxDownloads: 0, ExpiresAt: time.Now().Add(time.Hour).Unix()})
	for i := 0; i < 5; i++ {
		st.Increment("tok")
	}
	if sh, _ := st.Get("tok"); sh.Count != 5 {
		t.Fatalf("count = %d, want 5", sh.Count)
	}
}

func TestPruneDropsDeadShares(t *testing.T) {
	st := testStore(t)
	f := testFile(t, "x")
	now := time.Now()
	st.Add(Share{Token: "live", Path: f, MaxDownloads: 1, ExpiresAt: now.Add(time.Hour).Unix()})
	st.Add(Share{Token: "expired", Path: f, MaxDownloads: 1, ExpiresAt: now.Add(-time.Second).Unix()})
	st.Add(Share{Token: "used", Path: f, MaxDownloads: 1, Count: 1})
	st.Add(Share{Token: "missing", Path: "/nonexistent/file", MaxDownloads: 1})

	removed, err := st.Prune(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 3 {
		t.Fatalf("removed %d shares, want 3", len(removed))
	}
	all, _ := st.List()
	if len(all) != 1 || all[0].Token != "live" {
		t.Fatalf("remaining = %+v, want only live", all)
	}
	if n, _ := st.CountActive(now); n != 1 {
		t.Fatalf("active = %d, want 1", n)
	}
}

func TestTokenProperties(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		tok, err := newToken()
		if err != nil {
			t.Fatal(err)
		}
		if len(tok) != 24 {
			t.Fatalf("token %q has length %d", tok, len(tok))
		}
		if seen[tok] {
			t.Fatal("duplicate token")
		}
		seen[tok] = true
	}
	if s := shortToken("abcdefghijkl"); s != "abcdefgh…" {
		t.Fatalf("shortToken = %q", s)
	}
}

func TestResolveToken(t *testing.T) {
	shares := []Share{{Token: "abcdef123"}, {Token: "abcdef456"}, {Token: "zzzzzz999"}}
	if tok, err := resolveToken(shares, "zzzzzz"); err != nil || tok != "zzzzzz999" {
		t.Fatalf("prefix: %q %v", tok, err)
	}
	if _, err := resolveToken(shares, "abcdef"); err == nil {
		t.Fatal("ambiguous prefix accepted")
	}
	if _, err := resolveToken(shares, "zz"); err == nil {
		t.Fatal("too-short prefix accepted")
	}
	if tok, _ := resolveToken(shares, "abcdef123"); tok != "abcdef123" {
		t.Fatal("exact match failed")
	}
}

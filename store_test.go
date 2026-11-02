package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
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

func TestResolveShare(t *testing.T) {
	shares := []Share{
		{Number: 3, Token: "AAAAAAbbbbbbccccccdddddd", Path: "/x/a.txt"},
		{Number: 12, Token: "AAAAAAxxxxxxccccccdddddd", Path: "/x/b.txt"},
		{Number: 4, Token: "123456xxxxxxccccccdddddd", Path: "/x/c.txt"},
	}
	for q, want := range map[string]string{
		"3": "/x/a.txt", "#3": "/x/a.txt", " 12 ": "/x/b.txt", "012": "/x/b.txt",
		"AAAAAAb": "/x/a.txt", "AAAAAAxxxxxxccccccdddddd": "/x/b.txt", "AAAAAAx…": "/x/b.txt",
	} {
		sh, err := resolveShare(shares, q)
		if err != nil || sh.Path != want {
			t.Errorf("resolveShare(%q) = %q, %v; want %q", q, sh.Path, err, want)
		}
	}
	// Digits are always a number, never a token prefix; prefixes need 6+
	// characters and must be unique.
	for _, q := range []string{"123456", "5", "#0", "0", "AAAAAA", "AAAAA", "#", ""} {
		if sh, err := resolveShare(shares, q); err == nil {
			t.Errorf("resolveShare(%q) = %+v, want an error", q, sh)
		}
	}
	if _, err := resolveShare(shares, "5"); err == nil || !strings.Contains(err.Error(), "no active share #5") {
		t.Errorf("unknown number: %v", err)
	}
}

// ---- Share numbers ----

func addNumbered(t *testing.T, st *Store, tok string, sh Share) int {
	t.Helper()
	sh.Token = tok
	if sh.Path == "" {
		sh.Path = "/srv/" + tok
	}
	if sh.MaxDownloads == 0 {
		sh.MaxDownloads = 1
	}
	if err := st.Add(sh); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(tok)
	if err != nil {
		t.Fatal(err)
	}
	return got.Number
}

// Numbers only go up while any share is active, even when the highest
// number ends, and start again at 1 once nothing is active.
func TestShareNumbersCountUpUntilNothingIsActive(t *testing.T) {
	st := testStore(t)
	for i, tok := range []string{"a", "b", "c"} {
		if n := addNumbered(t, st, tok, Share{}); n != i+1 {
			t.Fatalf("share %s = #%d, want #%d", tok, n, i+1)
		}
	}
	st.Delete("b")
	if n := addNumbered(t, st, "d", Share{}); n != 4 {
		t.Fatalf("after revoking #2: #%d, want #4", n)
	}
	st.Delete("d") // the highest number ends
	if n := addNumbered(t, st, "e", Share{}); n != 5 {
		t.Fatalf("after the highest share ended: #%d, want #5 (never reuse while shares are active)", n)
	}
	for _, tok := range []string{"a", "c", "e"} {
		st.Delete(tok)
	}
	if n := addNumbered(t, st, "f", Share{}); n != 1 {
		t.Fatalf("with nothing active: #%d, want #1", n)
	}
	if n := addNumbered(t, st, "g", Share{}); n != 2 {
		t.Fatalf("next: #%d, want #2", n)
	}
}

// Expired and used-up shares are not active, so they don't hold the count up.
func TestShareNumbersIgnoreEndedShares(t *testing.T) {
	st := testStore(t)
	addNumbered(t, st, "expired", Share{ExpiresAt: time.Now().Add(-time.Minute).Unix()})
	addNumbered(t, st, "used", Share{MaxDownloads: 1, Count: 1})
	if n := addNumbered(t, st, "new", Share{}); n != 1 {
		t.Fatalf("only ended shares exist: #%d, want #1", n)
	}
}

// The count is kept in state.db, so a restart (stop, crash) neither resets
// it nor repeats numbers.
func TestShareNumbersSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	addNumbered(t, st, "a", Share{})
	addNumbered(t, st, "b", Share{})
	st.Delete("b")
	st.Close()

	st, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if got, _ := st.Get("a"); got.Number != 1 {
		t.Fatalf("after reopen share a = #%d, want #1", got.Number)
	}
	if n := addNumbered(t, st, "c", Share{}); n != 3 {
		t.Fatalf("after reopen: #%d, want #3", n)
	}
}

// A state.db from before share numbers numbers its shares oldest first and
// carries on from there.
func TestOpenStoreNumbersOldShares(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE shares(token TEXT PRIMARY KEY, path TEXT, created INTEGER,
		expires_at INTEGER, max_downloads INTEGER, count INTEGER);
		INSERT INTO shares VALUES ('later', '/x/b', 9, 0, 1, 0), ('first', '/x/a', 5, 0, 1, 0), ('tied', '/x/c', 9, 0, 1, 0);`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	st, err := openStore(path)
	if err != nil {
		t.Fatalf("opening an old database: %v", err)
	}
	defer st.Close()
	for tok, want := range map[string]int{"first": 1, "later": 2, "tied": 3} {
		if got, err := st.Get(tok); err != nil || got.Number != want || got.Path == "" {
			t.Errorf("%s = %+v, %v; want #%d", tok, got, err, want)
		}
	}
	if n := addNumbered(t, st, "new", Share{}); n != 4 {
		t.Fatalf("first new share = #%d, want #4", n)
	}
}

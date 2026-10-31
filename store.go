package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

// Share is one shared file. ExpiresAt == 0 means no expiry;
// MaxDownloads == 0 means unlimited downloads.
type Share struct {
	Token        string `json:"token"`
	Path         string `json:"path"`
	Created      int64  `json:"created"`
	ExpiresAt    int64  `json:"expires_at"`
	MaxDownloads int    `json:"max_downloads"`
	Count        int    `json:"count"`
}

// Active reports whether the share can still be downloaded at time now.
func (s Share) Active(now time.Time) bool {
	if s.ExpiresAt != 0 && now.Unix() >= s.ExpiresAt {
		return false
	}
	if s.MaxDownloads != 0 && s.Count >= s.MaxDownloads {
		return false
	}
	return true
}

var errNotFound = errors.New("share not found")

type Store struct {
	db *sql.DB
}

const schema = `CREATE TABLE IF NOT EXISTS shares(
	token TEXT PRIMARY KEY,
	path TEXT,
	created INTEGER,
	expires_at INTEGER,
	max_downloads INTEGER,
	count INTEGER
)`

func openStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	// One connection serialises all writes, which keeps increments atomic.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init database: %w", err)
	}
	os.Chmod(path, 0o600)
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Add(sh Share) error {
	_, err := s.db.Exec(`INSERT INTO shares(token, path, created, expires_at, max_downloads, count) VALUES(?,?,?,?,?,?)`,
		sh.Token, sh.Path, sh.Created, sh.ExpiresAt, sh.MaxDownloads, sh.Count)
	return err
}

func (s *Store) Get(token string) (Share, error) {
	var sh Share
	err := s.db.QueryRow(`SELECT token, path, created, expires_at, max_downloads, count FROM shares WHERE token = ?`, token).
		Scan(&sh.Token, &sh.Path, &sh.Created, &sh.ExpiresAt, &sh.MaxDownloads, &sh.Count)
	if errors.Is(err, sql.ErrNoRows) {
		return Share{}, errNotFound
	}
	return sh, err
}

func (s *Store) List() ([]Share, error) {
	rows, err := s.db.Query(`SELECT token, path, created, expires_at, max_downloads, count FROM shares ORDER BY created`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Share
	for rows.Next() {
		var sh Share
		if err := rows.Scan(&sh.Token, &sh.Path, &sh.Created, &sh.ExpiresAt, &sh.MaxDownloads, &sh.Count); err != nil {
			return nil, err
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}

func (s *Store) Delete(token string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM shares WHERE token = ?`, token)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Increment atomically adds one completed download, never exceeding
// max_downloads. It returns the share as it is after the update.
func (s *Store) Increment(token string) (Share, error) {
	_, err := s.db.Exec(`UPDATE shares SET count = count + 1
		WHERE token = ? AND (max_downloads = 0 OR count < max_downloads)`, token)
	if err != nil {
		return Share{}, err
	}
	return s.Get(token)
}

// Prune deletes shares that are expired, exhausted, or whose file is gone.
// It returns the removed shares.
func (s *Store) Prune(now time.Time) ([]Share, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	var removed []Share
	for _, sh := range all {
		if sh.Active(now) && fileExists(sh.Path) {
			continue
		}
		if _, err := s.Delete(sh.Token); err != nil {
			return removed, err
		}
		removed = append(removed, sh)
	}
	return removed, nil
}

func (s *Store) CountActive(now time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM shares
		WHERE (expires_at = 0 OR expires_at > ?) AND (max_downloads = 0 OR count < max_downloads)`, now.Unix()).Scan(&n)
	return n, err
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

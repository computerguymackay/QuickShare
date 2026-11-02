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
	// Number is the short number `list` shows and `revoke` accepts. See Add.
	Number       int    `json:"number"`
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
	count INTEGER,
	number INTEGER DEFAULT 0  -- share number for list/revoke; see Add
);
CREATE TABLE IF NOT EXISTS counters(
	name TEXT PRIMARY KEY,
	value INTEGER NOT NULL
)`

const shareColumns = `token, path, created, expires_at, max_downloads, count, number`

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
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("upgrade database: %w", err)
	}
	os.Chmod(path, 0o600)
	return &Store{db: db}, nil
}

// migrate upgrades a state.db from before share numbers: it adds the
// number column and numbers the existing shares, oldest first.
func migrate(db *sql.DB) error {
	var have int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('shares') WHERE name = 'number'`).Scan(&have); err != nil {
		return err
	}
	if have > 0 {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE shares ADD COLUMN number INTEGER DEFAULT 0`); err != nil {
		return err
	}
	if _, err := db.Exec(`UPDATE shares SET number = (SELECT COUNT(*) FROM shares AS o
		WHERE o.created < shares.created
		OR (o.created = shares.created AND o.rowid <= shares.rowid))`); err != nil {
		return err
	}
	_, err := db.Exec(`INSERT OR REPLACE INTO counters(name, value)
		SELECT 'last_number', COALESCE(MAX(number), 0) FROM shares`)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

// Add saves a new share and gives it the next share number. Numbers only
// go up while any share is active, so a number never points at a different
// share than the one `list` showed. When no share is active, numbering
// starts again at 1. The last number handed out is kept in the database,
// so shares kept by `quickshare stop` or a crash keep their numbers and new
// ones don't repeat them.
func (s *Store) Add(sh Share) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var active, last int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM shares WHERE `+activeWhere, time.Now().Unix()).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		err := tx.QueryRow(`SELECT value FROM counters WHERE name = 'last_number'`).Scan(&last)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	sh.Number = last + 1
	if _, err := tx.Exec(`INSERT OR REPLACE INTO counters(name, value) VALUES('last_number', ?)`, sh.Number); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO shares(`+shareColumns+`) VALUES(?,?,?,?,?,?,?)`,
		sh.Token, sh.Path, sh.Created, sh.ExpiresAt, sh.MaxDownloads, sh.Count, sh.Number); err != nil {
		return err
	}
	return tx.Commit()
}

type scanner interface{ Scan(dest ...any) error }

func scanShare(row scanner) (Share, error) {
	var sh Share
	var number sql.NullInt64
	err := row.Scan(&sh.Token, &sh.Path, &sh.Created, &sh.ExpiresAt, &sh.MaxDownloads, &sh.Count, &number)
	sh.Number = int(number.Int64)
	return sh, err
}

func (s *Store) Get(token string) (Share, error) {
	sh, err := scanShare(s.db.QueryRow(`SELECT `+shareColumns+` FROM shares WHERE token = ?`, token))
	if errors.Is(err, sql.ErrNoRows) {
		return Share{}, errNotFound
	}
	return sh, err
}

func (s *Store) List() ([]Share, error) {
	rows, err := s.db.Query(`SELECT ` + shareColumns + ` FROM shares ORDER BY created`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Share
	for rows.Next() {
		sh, err := scanShare(rows)
		if err != nil {
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

// activeWhere matches the shares Active accepts. Its one parameter is
// now.Unix().
const activeWhere = `(expires_at = 0 OR expires_at > ?) AND (max_downloads = 0 OR count < max_downloads)`

func (s *Store) CountActive(now time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM shares WHERE `+activeWhere, now.Unix()).Scan(&n)
	return n, err
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

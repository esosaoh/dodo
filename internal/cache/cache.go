package cache

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/esosaoh/dodo/internal/classify"
)

// scans older than this are pruned (cascades to their link_states) on write
const scanMaxAge = 90 * 24 * time.Hour

type LinkState struct {
	URL          string
	Class        classify.Class
	Status       int
	ETag         string
	LastModified string
	CheckedAt    time.Time
	Fails        int
	Successes    int
}

// ScanSummary is the per-run row a Store keeps so link history can be
// grouped and queried by scan, not just by latest state.
type ScanSummary struct {
	Seed         string
	StartedAt    time.Time
	FinishedAt   time.Time
	PagesCrawled int
	TotalLinks   int
	Broken       int
}

// Store persists every scan's link states in SQLite, so re-scans can diff
// against the last run and history/trend queries can look across many runs.
type Store struct {
	db *sql.DB
}

// path "" means the default under os.UserCacheDir
func Open(path string) (*Store, error) {
	if path == "" {
		dir, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("resolve cache dir: %w", err)
		}
		path = filepath.Join(dir, "dodo", "dodo.db")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open cache db: %w", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS scans (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	seed          TEXT NOT NULL,
	started_at    TEXT NOT NULL,
	finished_at   TEXT NOT NULL,
	pages_crawled INTEGER NOT NULL,
	total_links   INTEGER NOT NULL,
	broken        INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS link_states (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	scan_id       INTEGER NOT NULL REFERENCES scans(id) ON DELETE CASCADE,
	url           TEXT NOT NULL,
	class         TEXT NOT NULL,
	status        INTEGER NOT NULL,
	etag          TEXT NOT NULL DEFAULT '',
	last_modified TEXT NOT NULL DEFAULT '',
	checked_at    TEXT NOT NULL,
	fails         INTEGER NOT NULL DEFAULT 0,
	successes     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_link_states_url_checked ON link_states(url, checked_at DESC);
CREATE INDEX IF NOT EXISTS idx_scans_seed_started ON scans(seed, started_at);
`)
	return err
}

func (s *Store) GetStates(ctx context.Context, urls []string) (map[string]*LinkState, error) {
	out := make(map[string]*LinkState, len(urls))
	stmt, err := s.db.PrepareContext(ctx, `
SELECT class, status, etag, last_modified, checked_at, fails, successes
FROM link_states WHERE url = ? ORDER BY checked_at DESC LIMIT 1`)
	if err != nil {
		return out, nil // unreadable cache is not fatal
	}
	defer stmt.Close()

	for _, u := range urls {
		var class, etag, lastMod, checkedAt string
		var status, fails, successes int
		err := stmt.QueryRowContext(ctx, u).Scan(&class, &status, &etag, &lastMod, &checkedAt, &fails, &successes)
		if err != nil {
			continue // no prior record for this URL
		}
		ts, err := time.Parse(time.RFC3339Nano, checkedAt)
		if err != nil {
			continue
		}
		out[u] = &LinkState{
			URL: u, Class: classify.Class(class), Status: status,
			ETag: etag, LastModified: lastMod, CheckedAt: ts,
			Fails: fails, Successes: successes,
		}
	}
	return out, nil
}

func (s *Store) PutStates(ctx context.Context, scan ScanSummary, states []*LinkState) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`INSERT INTO scans (seed, started_at, finished_at, pages_crawled, total_links, broken) VALUES (?, ?, ?, ?, ?, ?)`,
		scan.Seed, scan.StartedAt.UTC().Format(time.RFC3339Nano), scan.FinishedAt.UTC().Format(time.RFC3339Nano),
		scan.PagesCrawled, scan.TotalLinks, scan.Broken)
	if err != nil {
		return err
	}
	scanID, err := res.LastInsertId()
	if err != nil {
		return err
	}

	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO link_states (scan_id, url, class, status, etag, last_modified, checked_at, fails, successes)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, st := range states {
		_, err := stmt.ExecContext(ctx, scanID, st.URL, string(st.Class), st.Status, st.ETag, st.LastModified,
			st.CheckedAt.UTC().Format(time.RFC3339Nano), st.Fails, st.Successes)
		if err != nil {
			return err
		}
	}

	cutoff := time.Now().Add(-scanMaxAge).UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `DELETE FROM scans WHERE finished_at < ?`, cutoff); err != nil {
		return err
	}

	return tx.Commit()
}

// History returns every recorded state for a URL, oldest first.
func (s *Store) History(ctx context.Context, url string) ([]*LinkState, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT class, status, etag, last_modified, checked_at, fails, successes
FROM link_states WHERE url = ? ORDER BY checked_at ASC`, url)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*LinkState
	for rows.Next() {
		var class, etag, lastMod, checkedAt string
		var status, fails, successes int
		if err := rows.Scan(&class, &status, &etag, &lastMod, &checkedAt, &fails, &successes); err != nil {
			return nil, err
		}
		ts, err := time.Parse(time.RFC3339Nano, checkedAt)
		if err != nil {
			return nil, err
		}
		out = append(out, &LinkState{
			URL: url, Class: classify.Class(class), Status: status,
			ETag: etag, LastModified: lastMod, CheckedAt: ts,
			Fails: fails, Successes: successes,
		})
	}
	return out, rows.Err()
}

// Trend returns every recorded scan of a seed, oldest first.
func (s *Store) Trend(ctx context.Context, seed string) ([]ScanSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT seed, started_at, finished_at, pages_crawled, total_links, broken
FROM scans WHERE seed = ? ORDER BY started_at ASC`, seed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ScanSummary
	for rows.Next() {
		var sc ScanSummary
		var started, finished string
		if err := rows.Scan(&sc.Seed, &started, &finished, &sc.PagesCrawled, &sc.TotalLinks, &sc.Broken); err != nil {
			return nil, err
		}
		if sc.StartedAt, err = time.Parse(time.RFC3339Nano, started); err != nil {
			return nil, err
		}
		if sc.FinishedAt, err = time.Parse(time.RFC3339Nano, finished); err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

func (s *Store) Close() error {
	return s.db.Close()
}

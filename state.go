package main

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Preserve the on-disk contract of the Python worker so either worker can use
// the same notification and cooldown records across a handover or rollback.
const stateSchema = `
CREATE TABLE IF NOT EXISTS notified (
    event TEXT NOT NULL, screen TEXT NOT NULL, sku TEXT NOT NULL,
    PRIMARY KEY (event, screen, sku)
);
CREATE TABLE IF NOT EXISTS pause (
    service TEXT NOT NULL PRIMARY KEY, until_time INTEGER NOT NULL
);`

type stateStore struct{ db *sql.DB }
type listingKey struct{ screen, sku string }

func keyOf(l listing) listingKey { return listingKey{l.screenID, l.skuID} }

func openState(ctx context.Context, path string, readOnly bool) (*stateStore, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: abs}
	q := u.Query()
	q.Set("_pragma", "busy_timeout(5000)")
	if readOnly {
		q.Set("mode", "ro")
	}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if readOnly {
		err = db.PingContext(ctx)
	} else {
		_, err = db.ExecContext(ctx, stateSchema)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return &stateStore{db}, nil
}

func (s *stateStore) close() error { return s.db.Close() }

func (s *stateStore) pending(ctx context.Context, ev event, observed []listing) ([]listing, error) {
	var candidates []listing
	unavailableSeen := false
	for _, row := range observed {
		switch row.state {
		case unavailable:
			unavailableSeen = true
		case available:
			candidates = append(candidates, row)
		}
	}
	// Missing or unknown observations must not reset the previous notification.
	if !unavailableSeen && len(candidates) == 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if unavailableSeen {
		// Read only already-notified rows, then clear those now conclusively out
		// of stock. The common empty/sold-out poll issues no write or fsync.
		rows, err := tx.QueryContext(ctx, `SELECT screen, sku FROM notified WHERE event=?`, ev.key())
		if err != nil {
			return nil, err
		}
		sent := make(map[listingKey]bool)
		for rows.Next() {
			var k listingKey
			if err := rows.Scan(&k.screen, &k.sku); err != nil {
				rows.Close()
				return nil, err
			}
			sent[k] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		for _, row := range observed {
			k := keyOf(row)
			if row.state == unavailable && sent[k] {
				if _, err := tx.ExecContext(ctx, `DELETE FROM notified WHERE event=? AND screen=? AND sku=?`, ev.key(), k.screen, k.sku); err != nil {
					return nil, err
				}
				delete(sent, k)
			}
		}
		n := 0
		for _, row := range candidates {
			if !sent[keyOf(row)] {
				candidates[n] = row
				n++
			}
		}
		candidates = candidates[:n]
	} else {
		n := 0
		for _, row := range candidates {
			var sent int
			err := tx.QueryRowContext(ctx, `SELECT 1 FROM notified WHERE event=? AND screen=? AND sku=?`, ev.key(), row.screenID, row.skuID).Scan(&sent)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				candidates[n] = row
				n++
			case err != nil:
				return nil, err
			}
		}
		candidates = candidates[:n]
	}
	return candidates, tx.Commit()
}

func (s *stateStore) markSent(ctx context.Context, ev event, rows []listing) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, row := range rows {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO notified (event, screen, sku) VALUES (?, ?, ?)`, ev.key(), row.screenID, row.skuID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *stateStore) pausedUntil(ctx context.Context, service string) (time.Time, error) {
	var seconds int64
	err := s.db.QueryRowContext(ctx, `SELECT until_time FROM pause WHERE service=?`, service).Scan(&seconds)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(seconds, 0), nil
}

func (s *stateStore) pause(ctx context.Context, service string, until time.Time) error {
	seconds := until.Unix()
	if until.Nanosecond() != 0 {
		seconds++
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO pause (service, until_time) VALUES (?, ?)
        ON CONFLICT(service) DO UPDATE SET until_time=MAX(pause.until_time,excluded.until_time)`, service, seconds)
	return err
}

func (s *stateStore) check(ctx context.Context, now time.Time) (notified, cooldowns int, err error) {
	var integrity string
	if err = s.db.QueryRowContext(ctx, `PRAGMA quick_check(1)`).Scan(&integrity); err != nil {
		return
	}
	if integrity != "ok" {
		return 0, 0, errors.New("SQLite integrity check failed")
	}
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notified`).Scan(&notified); err != nil {
		return
	}
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pause WHERE until_time>?`, now.Unix()).Scan(&cooldowns)
	return
}

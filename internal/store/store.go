// Package store keeps WhatsApp group messages in SQLite for the daily feed.
package store

import (
	"database/sql"
	"strconv"
	"time"

	_ "modernc.org/sqlite"
)

// Message is one stored WhatsApp message.
type Message struct {
	ID, Chat, Sender string
	TS, StoredAt     time.Time
	Kind             string // "text" | "caption" | "media" | "poll"
	Text             string // rendered body, marker included
	ReplySender      string
	ReplySnippet     string
	Edited, Sent     bool
}

// Store is safe for concurrent use (single connection).
type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS messages(
	id TEXT PRIMARY KEY,
	chat TEXT NOT NULL,
	sender TEXT NOT NULL,
	ts INTEGER NOT NULL,
	stored_at INTEGER NOT NULL,
	kind TEXT NOT NULL,
	text TEXT NOT NULL,
	reply_sender TEXT NOT NULL DEFAULT '',
	reply_snippet TEXT NOT NULL DEFAULT '',
	edited INTEGER NOT NULL DEFAULT 0,
	sent INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS messages_stored_at ON messages(stored_at);
CREATE TABLE IF NOT EXISTS state(key TEXT PRIMARY KEY, value TEXT);`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Insert stores m unless a row with the same ID exists.
func (s *Store) Insert(m Message) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO messages
		(id,chat,sender,ts,stored_at,kind,text,reply_sender,reply_snippet,edited,sent)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		m.ID, m.Chat, m.Sender, m.TS.Unix(), m.StoredAt.Unix(), m.Kind, m.Text,
		m.ReplySender, m.ReplySnippet, b2i(m.Edited), b2i(m.Sent))
	return err
}

// ApplyEdit upserts: an existing row gets the new text and kind, edited=1 and
// stored_at restamped (ts and sent kept); a missing row is inserted with ts =
// m.TS (the edit time) and edited=1.
func (s *Store) ApplyEdit(m Message) error {
	_, err := s.db.Exec(`INSERT INTO messages
		(id,chat,sender,ts,stored_at,kind,text,reply_sender,reply_snippet,edited,sent)
		VALUES (?,?,?,?,?,?,?,?,?,1,?)
		ON CONFLICT(id) DO UPDATE SET
			text=excluded.text, kind=excluded.kind, edited=1, stored_at=excluded.stored_at`,
		m.ID, m.Chat, m.Sender, m.TS.Unix(), m.StoredAt.Unix(), m.Kind, m.Text,
		m.ReplySender, m.ReplySnippet, b2i(m.Sent))
	return err
}

func (s *Store) Delete(id string) error {
	_, err := s.db.Exec(`DELETE FROM messages WHERE id=?`, id)
	return err
}

// Purge deletes rows whose ts and stored_at are both older than now-retention.
func (s *Store) Purge(now time.Time, retention time.Duration) (int, error) {
	cut := now.Add(-retention).Unix()
	res, err := s.db.Exec(`DELETE FROM messages WHERE ts < ? AND stored_at < ?`, cut, cut)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// Pending returns messages with last_sent_at < stored_at <= until (all up to
// until when last_sent_at is unset), ordered by chat, ts, id.
func (s *Store) Pending(until time.Time) ([]Message, error) {
	last, ok, err := s.LastSentAt()
	if err != nil {
		return nil, err
	}
	lo := int64(-1 << 62)
	if ok {
		lo = last.Unix()
	}
	rows, err := s.db.Query(`SELECT id,chat,sender,ts,stored_at,kind,text,reply_sender,reply_snippet,edited,sent
		FROM messages WHERE stored_at > ? AND stored_at <= ? ORDER BY chat, ts, id`, lo, until.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var ts, st int64
		var ed, sent int
		if err := rows.Scan(&m.ID, &m.Chat, &m.Sender, &ts, &st, &m.Kind, &m.Text,
			&m.ReplySender, &m.ReplySnippet, &ed, &sent); err != nil {
			return nil, err
		}
		m.TS, m.StoredAt = time.Unix(ts, 0).UTC(), time.Unix(st, 0).UTC()
		m.Edited, m.Sent = ed != 0, sent != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) LastSentAt() (time.Time, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM state WHERE key='last_sent_at'`).Scan(&v)
	if err == sql.ErrNoRows {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}, false, err
	}
	return time.Unix(n, 0).UTC(), true, nil
}

// CommitSent marks ids sent and records builtAt as last_sent_at, atomically.
func (s *Store) CommitSent(builtAt time.Time, ids []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.Exec(`UPDATE messages SET sent=1 WHERE id=?`, id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO state(key,value) VALUES('last_sent_at',?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, strconv.FormatInt(builtAt.Unix(), 10)); err != nil {
		return err
	}
	return tx.Commit()
}

package store

import (
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 25, 18, 0, 0, 0, time.UTC)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func msg(id, text string, ts time.Time) Message {
	return Message{ID: id, Chat: "g", Sender: "s", TS: ts, StoredAt: ts, Kind: "text", Text: text}
}

func pending(t *testing.T, s *Store, until time.Time) []Message {
	t.Helper()
	ms, err := s.Pending(until)
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

func TestInsertIgnoresDuplicate(t *testing.T) {
	s := open(t)
	s.Insert(msg("A", "one", t0))
	s.Insert(msg("A", "two", t0))
	ms := pending(t, s, t0)
	if len(ms) != 1 || ms[0].Text != "one" {
		t.Fatalf("got %+v", ms)
	}
}

func TestEditUpdatesAndRestamps(t *testing.T) {
	s := open(t)
	s.Insert(msg("A", "old", t0))
	if err := s.CommitSent(t0.Add(time.Hour), []string{"A"}); err != nil {
		t.Fatal(err)
	}
	e := msg("A", "new", t0.Add(2*time.Hour))
	if err := s.ApplyEdit(e); err != nil {
		t.Fatal(err)
	}
	ms := pending(t, s, t0.Add(3*time.Hour))
	if len(ms) != 1 {
		t.Fatalf("got %+v", ms)
	}
	m := ms[0]
	if m.Text != "new" || !m.Edited || !m.Sent || !m.TS.Equal(t0) || !m.StoredAt.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("got %+v", m)
	}
}

func TestEditBeforeOriginal(t *testing.T) {
	s := open(t)
	s.ApplyEdit(msg("A", "edited", t0.Add(time.Hour)))
	s.Insert(msg("A", "original", t0))
	ms := pending(t, s, t0.Add(2*time.Hour))
	if len(ms) != 1 || ms[0].Text != "edited" || !ms[0].Edited {
		t.Fatalf("got %+v", ms)
	}
}

func TestEditOfPurgedInsertsRow(t *testing.T) {
	s := open(t)
	e := msg("B", "x", t0) // TS = edit time
	e.StoredAt = t0.Add(time.Hour)
	if err := s.ApplyEdit(e); err != nil {
		t.Fatal(err)
	}
	ms := pending(t, s, t0.Add(time.Hour))
	if len(ms) != 1 || !ms[0].TS.Equal(t0) || !ms[0].StoredAt.Equal(t0.Add(time.Hour)) || !ms[0].Edited {
		t.Fatalf("got %+v", ms)
	}
}

func TestDelete(t *testing.T) {
	s := open(t)
	s.Insert(msg("A", "x", t0))
	if err := s.Delete("A"); err != nil {
		t.Fatal(err)
	}
	if ms := pending(t, s, t0); len(ms) != 0 {
		t.Fatalf("got %+v", ms)
	}
}

func TestPurgeKeepsRecentEdits(t *testing.T) {
	s := open(t)
	m := msg("A", "x", t0)
	m.StoredAt = t0.Add(47 * time.Hour)
	s.Insert(m)
	if n, err := s.Purge(t0.Add(49*time.Hour), 48*time.Hour); err != nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if n, err := s.Purge(t0.Add(96*time.Hour), 48*time.Hour); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestPendingWindow(t *testing.T) {
	s := open(t)
	if _, ok, err := s.LastSentAt(); ok || err != nil {
		t.Fatal(ok, err)
	}
	s.Insert(msg("A", "a", t0))
	s.Insert(msg("B", "b", t0.Add(time.Hour)))
	s.Insert(msg("C", "c", t0.Add(2*time.Hour)))
	if err := s.CommitSent(t0.Add(time.Hour), []string{"A", "B"}); err != nil {
		t.Fatal(err)
	}
	ms := pending(t, s, t0.Add(2*time.Hour))
	if len(ms) != 1 || ms[0].ID != "C" {
		t.Fatalf("got %+v", ms)
	}
	got, ok, err := s.LastSentAt()
	if err != nil || !ok || !got.Equal(t0.Add(time.Hour)) {
		t.Fatal(got, ok, err)
	}
}

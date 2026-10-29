package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"feedbridge/internal/config"
	"feedbridge/internal/store"
	"feedbridge/internal/webhook"
)

const (
	g1 = "111@g.us"
	g2 = "222@g.us"
)

type fakeWA struct {
	st         *store.Store
	connectErr error
	synced     bool
	syncErr    error
	msgs       []store.Message
	names      map[string]error
	calls      []string
}

func (f *fakeWA) Connect(ctx context.Context) error {
	f.calls = append(f.calls, "connect")
	return f.connectErr
}
func (f *fakeWA) WaitSynced(ctx context.Context, d time.Duration) SyncResult {
	f.calls = append(f.calls, "wait")
	for _, m := range f.msgs {
		if err := f.st.Insert(m); err != nil {
			panic(err)
		}
	}
	return SyncResult{Synced: f.synced, New: len(f.msgs), Err: f.syncErr}
}
func (f *fakeWA) GroupName(ctx context.Context, jid string) (string, error) {
	f.calls = append(f.calls, "name")
	if err := f.names[jid]; err != nil {
		return "", err
	}
	return "Name " + jid[:3], nil
}
func (f *fakeWA) Disconnect() { f.calls = append(f.calls, "disconnect") }

var now = time.Date(2026, 10, 26, 6, 35, 0, 123, time.UTC)

func setup(t *testing.T) (Deps, *fakeWA, *[]webhook.Payload) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	at := now.Add(-time.Hour)
	wa := &fakeWA{st: st, synced: true, msgs: []store.Message{
		{ID: "a", Chat: g1, Sender: "Ann", TS: at, StoredAt: at, Kind: "text", Text: "hello one"},
		{ID: "b", Chat: g2, Sender: "Bob", TS: at, StoredAt: at, Kind: "text", Text: "hello two"},
	}}
	var sent []webhook.Payload
	d := Deps{
		Cfg: config.Config{Groups: []string{g1, g2}, Retention: 48 * time.Hour, SyncTimeout: 5 * time.Minute,
			MaxMessageChars: 500, MaxFeedBytes: 180000, Secret: "s3cret"},
		WA: wa, Store: st, Now: func() time.Time { return now }, Version: "1.0.0",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Send: func(ctx context.Context, p webhook.Payload) error {
			wa.calls = append(wa.calls, "send")
			sent = append(sent, p)
			return nil
		},
	}
	return d, wa, &sent
}

func TestRunHappyPath(t *testing.T) {
	d, wa, sent := setup(t)
	if err := Run(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 1 {
		t.Fatalf("sends = %d", len(*sent))
	}
	txt := (*sent)[0].Text
	for _, w := range []string{"hello one", "hello two", "whatsapp: synced"} {
		if !strings.Contains(txt, w) {
			t.Errorf("missing %q in:\n%s", w, txt)
		}
	}
	builtAt := now.Truncate(time.Second)
	last, ok, err := d.Store.LastSentAt()
	if err != nil || !ok || !last.Equal(builtAt) {
		t.Fatalf("last = %v %v %v", last, ok, err)
	}
	p, _ := d.Store.Pending(now.Add(time.Hour))
	if len(p) != 0 {
		t.Errorf("pending after run = %d", len(p))
	}
	order := strings.Join(wa.calls, ",")
	if !strings.Contains(order, "disconnect,send") || !strings.HasPrefix(order, "connect,wait") {
		t.Errorf("call order = %s", order)
	}
}

func TestRunSendFails(t *testing.T) {
	d, _, sent := setup(t)
	good := d.Send
	d.Send = func(ctx context.Context, p webhook.Payload) error {
		*sent = append(*sent, p)
		return webhook.ErrNotOK
	}
	if err := Run(context.Background(), d); !errors.Is(err, webhook.ErrNotOK) {
		t.Fatalf("err = %v", err)
	}
	if _, ok, _ := d.Store.LastSentAt(); ok {
		t.Fatal("last_sent_at moved on failure")
	}
	d.WA = &fakeWA{st: d.Store, synced: true}
	d.Send = good
	d.Now = func() time.Time { return now.Add(10 * time.Minute) }
	n := len(*sent)
	if err := Run(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	txt := (*sent)[n].Text
	if !strings.Contains(txt, "hello one") || !strings.Contains(txt, "hello two") {
		t.Errorf("retry feed lacks messages:\n%s", txt)
	}
	d.Now = func() time.Time { return now.Add(20 * time.Minute) }
	if err := Run(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	txt = (*sent)[n+1].Text
	if strings.Contains(txt, "hello one") || strings.Contains(txt, "hello two") {
		t.Errorf("third feed repeats messages:\n%s", txt)
	}
}

func TestRunLoggedOut(t *testing.T) {
	d, wa, sent := setup(t)
	wa.connectErr = ErrLoggedOut
	if err := Run(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 1 || !strings.Contains((*sent)[0].Text, "NOT SYNCED — LOGGED OUT") {
		t.Fatalf("sent = %+v", *sent)
	}
	for _, c := range wa.calls {
		if c == "wait" || c == "disconnect" || c == "name" {
			t.Errorf("unexpected call %s", c)
		}
	}
}

func TestRunPartial(t *testing.T) {
	d, wa, sent := setup(t)
	wa.synced = false
	if err := Run(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains((*sent)[0].Text, "PARTIAL — sync timed out after 5m0s") {
		t.Errorf("text:\n%s", (*sent)[0].Text)
	}
}

func TestRunMissingGroup(t *testing.T) {
	d, wa, sent := setup(t)
	wa.names = map[string]error{g2: ErrNotMember}
	if err := Run(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains((*sent)[0].Text, "groups: 1/2 found (missing: "+g2+")") {
		t.Errorf("text:\n%s", (*sent)[0].Text)
	}
}

func TestRunPurgesBeforeBuild(t *testing.T) {
	d, _, sent := setup(t)
	old := now.Add(-100 * time.Hour)
	if err := d.Store.Insert(store.Message{ID: "old", Chat: g1, Sender: "Ann", TS: old, StoredAt: old, Kind: "text", Text: "ancient words"}); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if strings.Contains((*sent)[0].Text, "ancient words") {
		t.Error("old message in feed")
	}
	p, _ := d.Store.Pending(now)
	for _, m := range p {
		if m.ID == "old" {
			t.Error("old row survived")
		}
	}
}

func TestRunPayload(t *testing.T) {
	d, _, sent := setup(t)
	if err := Run(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	p := (*sent)[0]
	if p.ID != "2026-10-26T06:35:00Z" || p.Date != "26 Oct 2026" || p.Secret != "s3cret" {
		t.Errorf("payload = %q %q", p.ID, p.Date)
	}
}

func TestRunFatalDuringSync(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{ErrLoggedOut, "NOT SYNCED — LOGGED OUT"},
		{ErrBanned, "NOT SYNCED — BANNED"},
	} {
		d, wa, sent := setup(t)
		wa.synced, wa.syncErr = false, tc.err
		if err := Run(context.Background(), d); err != nil {
			t.Fatal(err)
		}
		txt := (*sent)[0].Text
		if !strings.Contains(txt, tc.want) || strings.Contains(txt, "PARTIAL") {
			t.Errorf("%v: text:\n%s", tc.err, txt)
		}
		if !strings.Contains(txt, "hello one") {
			t.Errorf("%v: stored messages missing:\n%s", tc.err, txt)
		}
		if got := strings.Join(wa.calls, ","); got != "connect,wait,disconnect,send" {
			t.Errorf("%v: calls = %s", tc.err, got)
		}
	}
}

func TestRunLogsSendFailureReason(t *testing.T) {
	d, _, _ := setup(t)
	var buf bytes.Buffer
	d.Log = slog.New(slog.NewTextHandler(&buf, nil))
	d.Send = func(ctx context.Context, p webhook.Payload) error {
		return fmt.Errorf("%w (status 500) after 4 attempts", webhook.ErrNotOK)
	}
	if err := Run(context.Background(), d); err == nil {
		t.Fatal("want error")
	}
	var line string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, "send failed") {
			line = l
		}
	}
	if !strings.Contains(line, "status 500") || !strings.Contains(line, "ms=") {
		t.Errorf("send failure log = %q", line)
	}
}

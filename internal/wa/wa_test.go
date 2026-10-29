package wa

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"feedbridge/internal/config"
	"feedbridge/internal/run"
	fbstore "feedbridge/internal/store"
)

func TestOpenContainerWithModernc(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.db")
	c, err := OpenContainer(context.Background(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	dev, err := c.GetFirstDevice(context.Background())
	if err != nil || dev == nil {
		t.Fatalf("first device: %v %v", dev, err)
	}
	if dev.ID != nil {
		t.Fatalf("fresh store should be unlinked")
	}
}

func newTestClient(t *testing.T) (*Client, *fbstore.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := fbstore.Open(filepath.Join(dir, "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Config{
		Groups:      []string{"120363000000000001@g.us"},
		SessionDB:   filepath.Join(dir, "session.db"),
		StorePath:   filepath.Join(dir, "messages.db"),
		Retention:   48 * time.Hour,
		SyncTimeout: 50 * time.Millisecond,
	}
	c, err := New(context.Background(), cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, st
}

func groupMsg(id, chat, text string) *events.Message {
	j, _ := types.ParseJID(chat)
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: j, IsGroup: true},
			ID:            id, PushName: "Alice", Timestamp: time.Now(),
		},
		Message: &waE2E.Message{Conversation: proto.String(text)},
	}
}

func TestConnectUnlinkedIsLoggedOut(t *testing.T) {
	c, _ := newTestClient(t)
	if err := c.Connect(context.Background()); !errors.Is(err, run.ErrLoggedOut) {
		t.Fatalf("got %v, want ErrLoggedOut", err)
	}
}

func TestHandlerCountsAndSync(t *testing.T) {
	c, st := newTestClient(t)
	c.handle(groupMsg("A1", "120363000000000001@g.us", "hello"))
	c.handle(groupMsg("B1", "120363000000000999@g.us", "other group")) // dropped
	c.handle(&events.OfflineSyncCompleted{})

	start := time.Now()
	res := c.WaitSynced(context.Background(), time.Hour)
	if time.Since(start) > time.Second {
		t.Fatalf("WaitSynced did not return immediately after OfflineSyncCompleted")
	}
	if !res.Synced || res.New != 1 || res.StoreErrors != 0 || res.Err != nil {
		t.Fatalf("got %+v", res)
	}
	msgs, err := st.Pending(time.Now().Add(time.Minute))
	if err != nil || len(msgs) != 1 || msgs[0].ID != "A1" {
		t.Fatalf("pending: %v %+v", err, msgs)
	}
}

func TestWaitSyncedTimesOut(t *testing.T) {
	c, _ := newTestClient(t)
	res := c.WaitSynced(context.Background(), 20*time.Millisecond)
	if res.Synced || res.Err != nil {
		t.Fatalf("want not synced, no error, got %+v", res)
	}
}

func TestWaitSyncedReturnsFatalErr(t *testing.T) {
	for _, tc := range []struct {
		evt  any
		want error
	}{
		{&events.LoggedOut{Reason: events.ConnectFailureLoggedOut}, run.ErrLoggedOut},
		{&events.TemporaryBan{}, run.ErrBanned},
	} {
		c, _ := newTestClient(t)
		c.handle(tc.evt)
		start := time.Now()
		res := c.WaitSynced(context.Background(), time.Hour)
		if time.Since(start) > time.Second {
			t.Fatalf("%T: WaitSynced did not return on fatal", tc.evt)
		}
		if res.Synced || !errors.Is(res.Err, tc.want) {
			t.Errorf("%T: got %+v, want Err %v", tc.evt, res, tc.want)
		}
	}
}

func TestFatalEvents(t *testing.T) {
	for _, tc := range []struct {
		evt  any
		want error
	}{
		{&events.LoggedOut{Reason: events.ConnectFailureLoggedOut}, run.ErrLoggedOut},
		{&events.LoggedOut{Reason: events.ConnectFailureUnknownLogout}, run.ErrBanned},
		{&events.TemporaryBan{}, run.ErrBanned},
		{&events.ConnectFailure{Reason: events.ConnectFailureTempBanned}, run.ErrBanned},
	} {
		c, _ := newTestClient(t)
		c.handle(tc.evt)
		<-c.fatal
		if !errors.Is(c.fatalErr, tc.want) {
			t.Errorf("%T: got %v, want %v", tc.evt, c.fatalErr, tc.want)
		}
	}
}

func TestGroupErrMapsToNotMember(t *testing.T) {
	for _, err := range []error{whatsmeow.ErrNotInGroup, whatsmeow.ErrGroupNotFound,
		whatsmeow.ErrIQForbidden, whatsmeow.ErrIQNotFound, fmt.Errorf("x: %w", whatsmeow.ErrNotInGroup)} {
		if !errors.Is(groupErr(err), run.ErrNotMember) {
			t.Errorf("%v not mapped", err)
		}
	}
	other := errors.New("timeout")
	if groupErr(other) != other {
		t.Errorf("unrelated error mapped")
	}
}

func TestLoggerRedactsStrings(t *testing.T) {
	var buf bytes.Buffer
	l := NewLogger(slog.New(slog.NewTextHandler(&buf, nil))).Sub("Client")
	l.Infof("info %s", "secret text")
	l.Debugf("debug %s", "secret text")
	l.Warnf("decrypt %s failed %d: %v", "Alice's text", 7, errors.New("bad mac"))
	out := buf.String()
	if strings.Contains(out, "secret") || strings.Contains(out, "Alice") || strings.Contains(out, "info") {
		t.Fatalf("leaked: %s", out)
	}
	if !strings.Contains(out, "failed 7: bad mac") || !strings.Contains(out, "level=WARN") || !strings.Contains(out, "module=Client") {
		t.Fatalf("missing warn: %s", out)
	}
}

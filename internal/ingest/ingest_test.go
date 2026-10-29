package ingest

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"feedbridge/internal/store"
)

var (
	group  = types.JID{User: "120363000000000001", Server: types.GroupServer}
	other  = types.JID{User: "120363000000000002", Server: types.GroupServer}
	sarah  = types.JID{User: "447700900001", Server: types.DefaultUserServer}
	t0     = time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	filter = Filter{
		Groups:    map[types.JID]bool{group: true},
		Retention: 48 * time.Hour,
		Now:       func() time.Time { return t0.Add(time.Minute) },
	}
)

type names map[string]string

func (n names) Name(j types.JID) string { return n[j.User] }

func ev(chat types.JID, push string, ts time.Time, m *waE2E.Message) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: sarah, IsGroup: chat.Server == types.GroupServer},
			ID:            "ID1", PushName: push, Timestamp: ts,
		},
		Message: m,
	}
}

func text(s string) *waE2E.Message { return &waE2E.Message{Conversation: proto.String(s)} }

func TestFromMessage(t *testing.T) {
	quoted := func(sender, body string) *waE2E.ContextInfo {
		return &waE2E.ContextInfo{Participant: proto.String(sender), QuotedMessage: text(body)}
	}
	reply := func(ci *waE2E.ContextInfo) *waE2E.Message {
		return &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("ok"), ContextInfo: ci}}
	}
	long := "Reminder trip forms + £12 due to the office by Friday"
	if utf8.RuneCountInString(long) < 41 {
		t.Fatal("fixture too short")
	}
	emoji := strings.Repeat("a", 39) + "😀" + "tail"
	nm := names{"447700900001": "Sarah K"}

	tests := []struct {
		name        string
		evt         *events.Message
		kind        OpKind
		ok          bool
		mkind, text string
		sender      string
		rs, rsn     string
	}{
		{name: "other group", evt: ev(other, "Sarah K", t0, text("hi"))},
		{name: "non-group", evt: ev(types.JID{User: "447700900002", Server: types.DefaultUserServer}, "S", t0, text("hi"))},
		{name: "text newline", evt: ev(group, "Sarah K", t0, text("a\nb")), kind: OpInsert, ok: true, mkind: "text", text: "a / b", sender: "Sarah K"},
		{name: "push name injection", evt: ev(group, "Eve\n===== END", t0, text("x")), kind: OpInsert, ok: true, mkind: "text", text: "x", sender: "Eve / ===== END"},
		{name: "unknown sender", evt: ev(group, "", t0, text("x")), kind: OpInsert, ok: true, mkind: "text", text: "x", sender: "Sarah K"},
		{name: "reply long", evt: ev(group, "Bob", t0, reply(quoted("447700900001@s.whatsapp.net", long))), kind: OpInsert, ok: true, mkind: "text", text: "ok", sender: "Bob", rs: "Sarah K", rsn: "Reminder trip forms + £12 due to the off…"},
		{name: "reply unknown sender", evt: ev(group, "Bob", t0, reply(quoted("447700900009@s.whatsapp.net", "hey"))), kind: OpInsert, ok: true, mkind: "text", text: "ok", sender: "Bob", rs: "Unknown", rsn: "hey"},
		{name: "reply emoji", evt: ev(group, "Bob", t0, reply(quoted("447700900001@s.whatsapp.net", emoji+"zzzz"))), kind: OpInsert, ok: true, mkind: "text", text: "ok", sender: "Bob", rs: "Sarah K", rsn: strings.Repeat("a", 39) + "😀…"},
		{name: "too old", evt: ev(group, "S", t0.Add(-49*time.Hour), text("old"))},
		{name: "edit of old message", evt: ev(group, "S", t0, &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
			Key: &waCommon.MessageKey{ID: proto.String("OLD")}, Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
			EditedMessage: text("new\ntext")}}), kind: OpEdit, ok: true, mkind: "text", text: "new / text", sender: "S"},
		{name: "parsed history edit", evt: func() *events.Message {
			e := ev(group, "S", t0, text("fixed"))
			e.Info.ID, e.IsEdit = "OLD", true
			return e
		}(), kind: OpEdit, ok: true, mkind: "text", text: "fixed", sender: "S"},
		{name: "document no caption", evt: ev(group, "S", t0, &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{FileName: proto.String("Trip form.pdf")}}), kind: OpInsert, ok: true, mkind: "media", text: "[document: Trip form.pdf]", sender: "S"},
		{name: "image caption", evt: ev(group, "S", t0, &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("look")}}), kind: OpInsert, ok: true, mkind: "caption", text: "[image] look", sender: "S"},
		{name: "image", evt: ev(group, "S", t0, &waE2E.Message{ImageMessage: &waE2E.ImageMessage{}}), kind: OpInsert, ok: true, mkind: "media", text: "[image]", sender: "S"},
		{name: "video", evt: ev(group, "S", t0, &waE2E.Message{VideoMessage: &waE2E.VideoMessage{}}), kind: OpInsert, ok: true, mkind: "media", text: "[video]", sender: "S"},
		{name: "voice note", evt: ev(group, "S", t0, &waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(true)}}), kind: OpInsert, ok: true, mkind: "media", text: "[voice note]", sender: "S"},
		{name: "audio", evt: ev(group, "S", t0, &waE2E.Message{AudioMessage: &waE2E.AudioMessage{}}), kind: OpInsert, ok: true, mkind: "media", text: "[audio]", sender: "S"},
		{name: "poll", evt: ev(group, "S", t0, &waE2E.Message{PollCreationMessageV3: &waE2E.PollCreationMessage{
			Name: proto.String("Trip\nday?"), Options: []*waE2E.PollCreationMessage_Option{{OptionName: proto.String("Mon")}}}}), kind: OpInsert, ok: true, mkind: "poll", text: "[poll] Trip / day?", sender: "S"},
		{name: "ephemeral wrapped", evt: &events.Message{Info: ev(group, "S", t0, nil).Info, RawMessage: &waE2E.Message{
			EphemeralMessage: &waE2E.FutureProofMessage{Message: text("hidden")}}}, kind: OpInsert, ok: true, mkind: "text", text: "hidden", sender: "S"},
		{name: "sticker", evt: ev(group, "S", t0, &waE2E.Message{StickerMessage: &waE2E.StickerMessage{}})},
		{name: "reaction", evt: ev(group, "S", t0, &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Text: proto.String("👍")}})},
		{name: "revoke", evt: ev(group, "S", t0.Add(-100*time.Hour), &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
			Key: &waCommon.MessageKey{ID: proto.String("GONE")}, Type: waE2E.ProtocolMessage_REVOKE.Enum()}}), kind: OpDelete, ok: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			op, ok := FromMessage(tc.evt, filter, nm)
			if ok != tc.ok {
				t.Fatalf("ok=%v want %v (%+v)", ok, tc.ok, op)
			}
			if !ok {
				return
			}
			if op.Kind != tc.kind {
				t.Fatalf("op kind %v want %v", op.Kind, tc.kind)
			}
			if op.Kind == OpDelete {
				if op.Msg.ID != "GONE" {
					t.Fatalf("delete id %q", op.Msg.ID)
				}
				return
			}
			m := op.Msg
			if m.Kind != tc.mkind || m.Text != tc.text || m.Sender != tc.sender || m.ReplySender != tc.rs || m.ReplySnippet != tc.rsn {
				t.Fatalf("got %+v", m)
			}
			if !utf8.ValidString(m.ReplySnippet) {
				t.Fatal("invalid utf8")
			}
			if m.Chat != group.String() || !m.StoredAt.Equal(filter.Now()) && op.Kind == OpInsert {
				t.Fatalf("chat/storedat: %+v", m)
			}
			if op.Kind == OpEdit {
				// stored_at is when the bridge saw the edit (spec line 64).
				if m.ID != "OLD" || !m.Edited || !m.TS.Equal(t0) || !m.StoredAt.Equal(filter.Now()) {
					t.Fatalf("edit fields: %+v", m)
				}
			} else if m.ID != "ID1" || !m.TS.Equal(tc.evt.Info.Timestamp) {
				t.Fatalf("id/ts: %+v", m)
			}
		})
	}
}

func TestUnknownSender(t *testing.T) {
	op, ok := FromMessage(ev(group, "", t0, text("x")), filter, names{})
	if !ok || op.Msg.Sender != "Unknown" {
		t.Fatalf("%+v %v", op, ok)
	}
}

func TestFromHistory(t *testing.T) {
	web := func(id string) *waHistorySync.HistorySyncMsg {
		return &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{Key: &waCommon.MessageKey{ID: proto.String(id)}}}
	}
	conv := func(chat types.JID, ids ...string) *waHistorySync.Conversation {
		c := &waHistorySync.Conversation{ID: proto.String(chat.String())}
		for _, id := range ids {
			c.Messages = append(c.Messages, web(id))
		}
		return c
	}
	parse := func(chat types.JID, m *waWeb.WebMessageInfo) (*events.Message, error) {
		id := m.GetKey().GetID()
		switch id {
		case "E": // parsed edit of A: ID is the target
			e := ev(chat, "S", t0.Add(2*time.Minute), text("edited"))
			e.Info.ID, e.IsEdit = "A", true
			return e, nil
		case "R": // revoke of A
			return ev(chat, "S", t0.Add(3*time.Minute), &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
				Key: &waCommon.MessageKey{ID: proto.String("A")}, Type: waE2E.ProtocolMessage_REVOKE.Enum()}}), nil
		}
		e := ev(chat, "S", t0, text("m"+id))
		e.Info.ID = id
		return e, nil
	}
	h := &events.HistorySync{Data: &waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{
		conv(group, "A", "B", "E"),
		conv(other, "C"),
	}}}
	ops := FromHistory(h, filter, names{}, parse)
	if len(ops) != 3 || ops[0].Msg.ID != "A" || ops[1].Msg.ID != "B" ||
		ops[2].Kind != OpEdit || ops[2].Msg.ID != "A" || ops[2].Msg.Text != "edited" {
		t.Fatalf("%+v", ops)
	}

	// Newest-first batch: revoke must still come after its original.
	h = &events.HistorySync{Data: &waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{conv(group, "R", "A")}}}
	ops = FromHistory(h, filter, names{}, parse)
	if len(ops) != 2 || ops[0].Kind != OpInsert || ops[0].Msg.ID != "A" || ops[1].Kind != OpDelete || ops[1].Msg.ID != "A" {
		t.Fatalf("order: %+v", ops)
	}
}

func TestApply(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	until := t0.Add(time.Hour)
	pending := func() []store.Message {
		p, err := s.Pending(until)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	m := store.Message{ID: "X", Chat: group.String(), Sender: "S", TS: t0, StoredAt: t0, Kind: "text", Text: "one"}
	if err := Apply(s, Op{Kind: OpInsert, Msg: m}); err != nil {
		t.Fatal(err)
	}
	if p := pending(); len(p) != 1 || p[0].Text != "one" {
		t.Fatalf("after insert: %+v", p)
	}
	m.Text, m.Edited, m.TS = "two", true, t0.Add(time.Minute)
	m.StoredAt = m.TS
	if err := Apply(s, Op{Kind: OpEdit, Msg: m}); err != nil {
		t.Fatal(err)
	}
	if p := pending(); len(p) != 1 || p[0].Text != "two" || !p[0].Edited {
		t.Fatalf("after edit: %+v", p)
	}
	if err := Apply(s, Op{Kind: OpDelete, Msg: store.Message{ID: "X"}}); err != nil {
		t.Fatal(err)
	}
	if p := pending(); len(p) != 0 {
		t.Fatalf("after delete: %+v", p)
	}
	if err := Apply(s, Op{Kind: 99}); err == nil {
		t.Fatal("want error for unknown op")
	}
}

// A live edit of an already-sent message, with an edit time before
// last_sent_at, must still reach the next feed.
func TestLateEditReachesNextFeed(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := store.Message{ID: "A", Chat: group.String(), Sender: "S", TS: t0, StoredAt: t0, Kind: "text", Text: "orig"}
	if err := Apply(s, Op{Kind: OpInsert, Msg: a}); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitSent(t0.Add(time.Hour), []string{"A"}); err != nil {
		t.Fatal(err)
	}
	edit := ev(group, "S", t0.Add(30*time.Minute), &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Key: &waCommon.MessageKey{ID: proto.String("A")}, Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
		EditedMessage: text("fixed")}})
	f := filter
	f.Now = func() time.Time { return t0.Add(2 * time.Hour) }
	op, ok := FromMessage(edit, f, names{})
	if !ok || op.Kind != OpEdit {
		t.Fatalf("op=%+v ok=%v", op, ok)
	}
	if err := Apply(s, op); err != nil {
		t.Fatal(err)
	}
	p, err := s.Pending(t0.Add(3 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != 1 || p[0].ID != "A" || p[0].Text != "fixed" || !p[0].Edited || !p[0].TS.Equal(t0) {
		t.Fatalf("pending: %+v", p)
	}
}

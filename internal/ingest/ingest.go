// Package ingest turns whatsmeow events into store operations. It is pure:
// it logs nothing and touches the store only in Apply.
package ingest

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"feedbridge/internal/sanitize"
	"feedbridge/internal/store"
)

type OpKind int

const (
	OpInsert OpKind = iota + 1
	OpEdit
	OpDelete
)

// Op is one store operation. OpDelete uses Msg.ID only.
type Op struct {
	Kind OpKind
	Msg  store.Message
}

// Names resolves a JID to a display name, "" when unknown.
type Names interface {
	Name(jid types.JID) string
}

type Filter struct {
	Groups    map[types.JID]bool
	Retention time.Duration
	Now       func() time.Time
}

const snippetRunes = 40

// FromMessage classifies one event; ok is false when it must be dropped.
func FromMessage(evt *events.Message, f Filter, n Names) (Op, bool) {
	if evt == nil || !evt.Info.IsGroup || !f.Groups[evt.Info.Chat] {
		return Op{}, false
	}
	if evt.Message == nil && evt.RawMessage != nil {
		evt.UnwrapRaw()
	}
	msg := evt.Message
	if msg == nil {
		return Op{}, false
	}
	now := f.Now()
	ts := evt.Info.Timestamp

	if pm := msg.GetProtocolMessage(); pm != nil {
		id := pm.GetKey().GetID()
		if id == "" {
			return Op{}, false
		}
		switch pm.GetType() {
		case waE2E.ProtocolMessage_REVOKE:
			return Op{Kind: OpDelete, Msg: store.Message{ID: id}}, true
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			if !fresh(ts, now, f.Retention) {
				return Op{}, false
			}
			kind, body, ok := render(pm.GetEditedMessage())
			if !ok {
				return Op{}, false
			}
			m := build(evt, n, kind, body, pm.GetEditedMessage())
			m.ID, m.Edited, m.TS, m.StoredAt = id, true, ts, now // stored_at: when the bridge saw the edit
			return Op{Kind: OpEdit, Msg: m}, true
		}
		return Op{}, false
	}

	if !fresh(ts, now, f.Retention) {
		return Op{}, false
	}
	kind, body, ok := render(msg)
	if !ok {
		return Op{}, false
	}
	m := build(evt, n, kind, body, msg)
	if evt.IsEdit {
		// ParseWebMessage rewrites MESSAGE_EDIT: Info.ID is the target's ID.
		m.Edited, m.TS, m.StoredAt = true, ts, now
		return Op{Kind: OpEdit, Msg: m}, true
	}
	m.StoredAt = now
	return Op{Kind: OpInsert, Msg: m}, true
}

func fresh(ts, now time.Time, retention time.Duration) bool {
	return ts.After(now.Add(-retention))
}

func build(evt *events.Message, n Names, kind, body string, msg *waE2E.Message) store.Message {
	m := store.Message{
		ID:     evt.Info.ID,
		Chat:   evt.Info.Chat.String(),
		Sender: sanitize.Line(senderName(evt.Info.PushName, evt.Info.Sender, n)),
		TS:     evt.Info.Timestamp,
		Kind:   kind,
		Text:   body,
	}
	if ci := contextInfo(msg); ci != nil && ci.GetQuotedMessage() != nil {
		m.ReplySender = sanitize.Line(senderName("", parseJID(ci.GetParticipant()), n))
		if _, qbody, ok := render(ci.GetQuotedMessage()); ok {
			m.ReplySnippet = snippet(qbody)
		}
	}
	return m
}

func senderName(push string, jid types.JID, n Names) string {
	if s := sanitize.Line(push); s != "" {
		return s
	}
	if n != nil && !jid.IsEmpty() {
		if s := sanitize.Line(n.Name(jid)); s != "" {
			return s
		}
	}
	return "Unknown"
}

func parseJID(s string) types.JID {
	if s == "" {
		return types.EmptyJID
	}
	j, err := types.ParseJID(s)
	if err != nil {
		return types.EmptyJID
	}
	return j
}

func snippet(s string) string {
	s = sanitize.Line(s)
	r := []rune(s)
	if len(r) <= snippetRunes {
		return s
	}
	return string(r[:snippetRunes]) + "…"
}

func contextInfo(m *waE2E.Message) *waE2E.ContextInfo {
	switch {
	case m.GetExtendedTextMessage() != nil:
		return m.GetExtendedTextMessage().GetContextInfo()
	case m.GetImageMessage() != nil:
		return m.GetImageMessage().GetContextInfo()
	case m.GetVideoMessage() != nil:
		return m.GetVideoMessage().GetContextInfo()
	case m.GetAudioMessage() != nil:
		return m.GetAudioMessage().GetContextInfo()
	case m.GetDocumentMessage() != nil:
		return m.GetDocumentMessage().GetContextInfo()
	}
	return nil
}

// render returns the store kind and sanitised body for a message content.
func render(m *waE2E.Message) (kind, body string, ok bool) {
	if m == nil {
		return "", "", false
	}
	// Unwrap wrappers that whatsmeow's UnwrapRaw would also remove.
	for i := 0; i < 4; i++ {
		switch {
		case m.GetEphemeralMessage().GetMessage() != nil:
			m = m.GetEphemeralMessage().GetMessage()
		case m.GetViewOnceMessage().GetMessage() != nil:
			m = m.GetViewOnceMessage().GetMessage()
		case m.GetViewOnceMessageV2().GetMessage() != nil:
			m = m.GetViewOnceMessageV2().GetMessage()
		case m.GetDocumentWithCaptionMessage().GetMessage() != nil:
			m = m.GetDocumentWithCaptionMessage().GetMessage()
		}
	}
	marked := func(marker, caption string) (string, string, bool) {
		if c := sanitize.Line(caption); c != "" {
			return "caption", marker + " " + c, true
		}
		return "media", marker, true
	}
	switch {
	case m.Conversation != nil:
		b := sanitize.Line(m.GetConversation())
		return "text", b, b != ""
	case m.GetExtendedTextMessage() != nil:
		b := sanitize.Line(m.GetExtendedTextMessage().GetText())
		return "text", b, b != ""
	case m.GetImageMessage() != nil:
		return marked("[image]", m.GetImageMessage().GetCaption())
	case m.GetVideoMessage() != nil:
		return marked("[video]", m.GetVideoMessage().GetCaption())
	case m.GetAudioMessage() != nil:
		if m.GetAudioMessage().GetPTT() {
			return "media", "[voice note]", true
		}
		return "media", "[audio]", true
	case m.GetDocumentMessage() != nil:
		d := m.GetDocumentMessage()
		return marked(fmt.Sprintf("[document: %s]", sanitize.Line(d.GetFileName())), d.GetCaption())
	}
	for _, p := range []*waE2E.PollCreationMessage{
		m.GetPollCreationMessage(), m.GetPollCreationMessageV2(), m.GetPollCreationMessageV3(),
		m.GetPollCreationMessageV5(), m.GetPollCreationMessageV6(),
		m.GetPollCreationMessageV4().GetMessage().GetPollCreationMessage(),
	} {
		if p != nil {
			return "poll", strings.TrimSpace("[poll] " + sanitize.Line(p.GetName())), true
		}
	}
	return "", "", false
}

// FromHistory runs every message of every watched conversation through parse
// and FromMessage.
func FromHistory(evt *events.HistorySync, f Filter, n Names,
	parse func(chat types.JID, m *waWeb.WebMessageInfo) (*events.Message, error)) []Op {
	var ops []Op
	for _, c := range evt.Data.GetConversations() {
		chat, err := types.ParseJID(c.GetID())
		if err != nil || !f.Groups[chat] {
			continue
		}
		var evs []*events.Message
		for _, hm := range c.GetMessages() {
			me, err := parse(chat, hm.GetMessage())
			if err != nil || me == nil {
				continue
			}
			evs = append(evs, me)
		}
		// Oldest first, so an original precedes its edits and revokes.
		sort.SliceStable(evs, func(i, j int) bool { return evs[i].Info.Timestamp.Before(evs[j].Info.Timestamp) })
		for _, me := range evs {
			if op, ok := FromMessage(me, f, n); ok {
				ops = append(ops, op)
			}
		}
	}
	return ops
}

// Apply executes an op against the store.
func Apply(s *store.Store, op Op) error {
	switch op.Kind {
	case OpInsert:
		return s.Insert(op.Msg)
	case OpEdit:
		return s.ApplyEdit(op.Msg)
	case OpDelete:
		return s.Delete(op.Msg.ID)
	}
	return fmt.Errorf("ingest: unknown op kind %d", op.Kind)
}

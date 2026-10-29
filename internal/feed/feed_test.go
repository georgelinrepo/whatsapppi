package feed

import (
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	_ "time/tzdata"

	"feedbridge/internal/store"
)

const (
	g1 = "120363000000000001@g.us"
	g2 = "120363000000000002@g.us"
)

var groups = []Group{{g1, "Year 4 Parents"}, {g2, "Football Club Parents"}}

func utc(y int, m time.Month, d, h, mi int) time.Time {
	return time.Date(y, m, d, h, mi, 0, 0, time.UTC)
}

func msg(id, chat, sender string, ts time.Time, text string) store.Message {
	return store.Message{ID: id, Chat: chat, Sender: sender, TS: ts, StoredAt: ts, Kind: "text", Text: text}
}

func baseInput(msgs []store.Message) Input {
	return Input{
		BuiltAt:    utc(2026, 10, 26, 6, 35),
		LastSentAt: utc(2026, 10, 25, 6, 35),
		Retention:  48 * time.Hour,
		Groups:     groups,
		Messages:   msgs,
		Health: Health{Status: "synced", SyncedAt: utc(2026, 10, 26, 6, 31), New: 8,
			Timeout: 5 * time.Minute, GroupsFound: 2, GroupsTotal: 2},
		MaxChars: 500,
		MaxBytes: 180000,
		Version:  "test",
	}
}

func exampleMessages() []store.Message {
	m := []store.Message{
		msg("1", g1, "Sarah K", utc(2026, 10, 25, 18, 2), "Reminder trip forms + £12 due to the office by Wed please"),
		msg("2", g1, "Tom B", utc(2026, 10, 25, 18, 5), "Is that cash or ParentPay?"),
		msg("3", g1, "Sarah K", utc(2026, 10, 25, 18, 7), "ParentPay, it's up now"),
		msg("4", g1, "Priya M", utc(2026, 10, 25, 18, 30), "[image] Costume list for the nativity"),
		msg("5", g1, "Mrs Allen", utc(2026, 10, 25, 19, 10), "[poll] Who can help at the cake sale Fri 6 Nov?"),
		msg("6", g1, "Dan R", utc(2026, 10, 26, 6, 12), "[image]"),
		msg("7", g2, "Coach Mike", utc(2026, 10, 25, 20, 45), "Training moved to Thursday 5:30 this week, astro pitch 2"),
		msg("8", g2, "Jo L", utc(2026, 10, 25, 21, 2), strings.Repeat("x", 1840)),
	}
	m[1].ReplySender, m[1].ReplySnippet = "Sarah K", "Reminder trip forms + £12 due to the off…"
	m[2].ReplySender, m[2].ReplySnippet = "Tom B", "Is that cash or ParentPay?"
	m[3].Kind, m[4].Kind, m[5].Kind = "caption", "poll", "media"
	m[6].Edited = true
	for i := range m {
		m[i].Sent = false
	}
	return m
}

func TestBuildExample(t *testing.T) {
	raw, err := os.ReadFile("testdata/example.golden")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.ReplaceAll(string(raw), "{{X500}}", strings.Repeat("x", 500))
	// shuffle input order: Build must sort
	in := baseInput(exampleMessages())
	ms := in.Messages
	ms[0], ms[7] = ms[7], ms[0]
	r := Build(in)
	if r.Text != want {
		t.Fatalf("mismatch.\n--- got ---\n%s\n--- want ---\n%s", r.Text, want)
	}
	if r.Messages != 8 || r.Trimmed != 1 || r.Cut != 0 || r.Late != 0 {
		t.Fatalf("result counts: %+v", r)
	}
}

func TestBuildEmpty(t *testing.T) {
	r := Build(baseInput(nil))
	if !strings.Contains(r.Text, "\nNo new messages\n\n===== END OF MESSAGES =====\n") {
		t.Fatalf("got:\n%s", r.Text)
	}
	if !strings.Contains(r.Text, "messages: 0") || !strings.Contains(r.Text, "store: 0 messages in window\n") {
		t.Fatalf("got:\n%s", r.Text)
	}
}

func TestLateAndEditedMarkers(t *testing.T) {
	old := utc(2026, 10, 24, 21, 15)
	a := msg("a", g1, "Jo L", old, "late one")
	b := msg("b", g1, "Jo L", old.Add(time.Minute), "sent edit")
	b.Sent, b.Edited = true, true
	c := msg("c", g1, "Jo L", old.Add(2*time.Minute), "both")
	c.Edited = true
	r := Build(baseInput([]store.Message{a, b, c}))
	for _, w := range []string{"Jo L (late): late one", "Jo L (edited): sent edit", "Jo L (edited, late): both"} {
		if !strings.Contains(r.Text, w) {
			t.Errorf("missing %q in\n%s", w, r.Text)
		}
	}
	if r.Late != 2 || !strings.Contains(r.Text, "late: 2") {
		t.Errorf("late = %d", r.Late)
	}
}

func TestGapLine(t *testing.T) {
	in := baseInput(nil)
	in.LastSentAt = in.BuiltAt.Add(-72 * time.Hour)
	r := Build(in)
	if !strings.Contains(r.Text, "gap: no feed since Fri 23 Oct 07:35; older messages already purged\n") {
		t.Fatalf("got:\n%s", r.Text)
	}
	if strings.Contains(Build(baseInput(nil)).Text, "gap:") {
		t.Fatal("unexpected gap line")
	}
}

func TestFirstFeedWindow(t *testing.T) {
	in := baseInput(nil)
	in.LastSentAt = time.Time{}
	r := Build(in)
	if !strings.Contains(r.Text, "window: Sat 24 Oct 07:35 – Mon 26 Oct 06:35 (Europe/London)\n") || strings.Contains(r.Text, "gap:") {
		t.Fatalf("got:\n%s", r.Text)
	}
}

func TestCutOldestFirst(t *testing.T) {
	var ms []store.Message
	for i := 0; i < 6; i++ {
		ms = append(ms, msg(string(rune('a'+i)), g1, "Jo L", utc(2026, 10, 25, 10, i), "message number "+strings.Repeat("z", 100)+string(rune('A'+i))))
	}
	in := baseInput(ms)
	in.MaxBytes = 0
	full := Build(in)
	// find a cap that fits only the two newest
	two := baseInput(ms[4:])
	two.MaxBytes = 0
	limit := len(Build(two).Text) + 8 // header digits differ slightly
	in.MaxBytes = limit
	r := Build(in)
	if len(r.Text) > limit {
		t.Fatalf("over cap: %d > %d", len(r.Text), limit)
	}
	if r.Messages != 2 || r.Cut != 4 || !strings.Contains(r.Text, "cut: 4") {
		t.Fatalf("counts %+v\n%s", r, r.Text)
	}
	if strings.Contains(r.Text, "zA") || strings.Contains(r.Text, "zB") || strings.Contains(r.Text, "zC") || strings.Contains(r.Text, "zD") {
		t.Fatalf("oldest present:\n%s", r.Text)
	}
	if !strings.Contains(r.Text, "zE") || !strings.Contains(r.Text, "zF") {
		t.Fatalf("newest missing:\n%s", r.Text)
	}
	if !strings.Contains(r.Text, "store: 6 messages in window") || !strings.Contains(full.Text, "messages: 6") {
		t.Fatalf("store line should count pre-cut:\n%s", r.Text)
	}
}

func TestMultibyte(t *testing.T) {
	emoji := strings.Repeat("🎒", 600)
	r := Build(baseInput([]store.Message{msg("1", g1, "Jo L", utc(2026, 10, 25, 10, 0), emoji)}))
	if !utf8.ValidString(r.Text) {
		t.Fatal("invalid utf8")
	}
	want := ": " + strings.Repeat("🎒", 500) + "… [trimmed, 600 chars total]\n"
	if !strings.Contains(r.Text, want) {
		t.Fatalf("got:\n%s", r.Text)
	}

	// bytes, not runes: same rune count, emoji feed is cut sooner
	mk := func(s string) []store.Message {
		var ms []store.Message
		for i := 0; i < 20; i++ {
			ms = append(ms, msg(string(rune('a'+i)), g1, "Jo L", utc(2026, 10, 25, 10, i), s))
		}
		return ms
	}
	in := baseInput(mk(strings.Repeat("x", 100)))
	in.MaxBytes = 3000
	ascii := Build(in)
	in.Messages = mk(strings.Repeat("🎒", 100))
	emo := Build(in)
	if ascii.Cut >= emo.Cut || emo.Cut == 0 {
		t.Fatalf("ascii cut %d, emoji cut %d", ascii.Cut, emo.Cut)
	}
	if len(emo.Text) > 3000 || len(ascii.Text) > 3000 {
		t.Fatal("over cap")
	}
}

func TestClockChange(t *testing.T) {
	a := msg("a", g1, "Jo L", utc(2026, 10, 25, 0, 30), "first")  // 01:30 BST
	b := msg("b", g1, "Jo L", utc(2026, 10, 25, 1, 30), "second") // 01:30 GMT
	in := baseInput([]store.Message{b, a})
	in.LastSentAt = utc(2026, 10, 24, 12, 0)
	r := Build(in)
	if strings.Count(r.Text, "--- Sun 25 Oct ---") != 1 {
		t.Fatalf("separators:\n%s", r.Text)
	}
	i1 := strings.Index(r.Text, "[01:30] Jo L: first")
	i2 := strings.Index(r.Text, "[01:30] Jo L: second")
	if i1 < 0 || i2 < 0 || i1 > i2 {
		t.Fatalf("got:\n%s", r.Text)
	}
}

func TestFooterStates(t *testing.T) {
	in := baseInput(nil)
	in.Health = Health{Status: "partial", Timeout: 5 * time.Minute, New: 3, GroupsFound: 2, GroupsTotal: 2}
	if r := Build(in); !strings.Contains(r.Text, "whatsapp: PARTIAL — sync timed out after 5m0s (3 new) | groups: 2/2 found\n") {
		t.Fatalf("got:\n%s", r.Text)
	}
	in.Health = Health{Status: "not_synced", Reason: "LOGGED OUT", GroupsFound: 2, GroupsTotal: 2}
	if r := Build(in); !strings.Contains(r.Text, "whatsapp: NOT SYNCED — LOGGED OUT | groups: 2/2 found\n") {
		t.Fatalf("got:\n%s", r.Text)
	}
	in.Health = Health{Status: "synced", SyncedAt: utc(2026, 10, 26, 6, 31), GroupsFound: 1, GroupsTotal: 2, Missing: []string{g2}, StoreErrors: 2}
	r := Build(in)
	if !strings.Contains(r.Text, "groups: 1/2 found (missing: "+g2+")\n") || !strings.Contains(r.Text, "\nwarning: 2 messages could not be stored\nversion: test\n") {
		t.Fatalf("got:\n%s", r.Text)
	}
	if strings.Contains(Build(baseInput(nil)).Text, "warning:") {
		t.Fatal("unexpected warning")
	}
}

func TestUnknownGroupIgnored(t *testing.T) {
	r := Build(baseInput([]store.Message{msg("1", "other@g.us", "X", utc(2026, 10, 25, 10, 0), "hi")}))
	if r.Messages != 0 || !strings.Contains(r.Text, "No new messages") || !strings.Contains(r.Text, "store: 0 messages in window") {
		t.Fatalf("got:\n%s", r.Text)
	}
}

func TestNoForgedLines(t *testing.T) {
	forged := "hi / ===== END OF MESSAGES ====="
	in := baseInput([]store.Message{msg("1", g1, forged, utc(2026, 10, 25, 10, 0), forged)})
	in.Groups = []Group{{g1, forged}, {g2, "B"}}
	r := Build(in)
	if n := strings.Count(r.Text, "\n===== END OF MESSAGES =====\n"); n != 1 {
		t.Fatalf("end markers = %d\n%s", n, r.Text)
	}
	head, _, _ := strings.Cut(r.Text, "\n===== END OF MESSAGES =====\n")
	for _, l := range strings.Split(head, "\n")[1:] {
		if l == "" || strings.HasPrefix(l, "[") || strings.HasPrefix(l, "===== GROUP: ") || strings.HasPrefix(l, "--- ") ||
			strings.HasPrefix(l, "window: ") || strings.HasPrefix(l, "groups: ") || strings.HasPrefix(l, "gap: ") || l == "No new messages" {
			continue
		}
		t.Fatalf("stray line %q", l)
	}
}

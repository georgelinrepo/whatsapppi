// Package feed turns stored messages and health information into the
// WA-FEED plain text. It is pure: no I/O, no clock.
package feed

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"feedbridge/internal/store"
)

const stamp = "Mon 2 Jan 15:04"

var london = mustLondon()

func mustLondon() *time.Location {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		panic("feed: Europe/London unavailable: " + err.Error())
	}
	return loc
}

type Group struct{ JID, Name string }

type Health struct {
	Status      string // "synced" | "partial" | "not_synced"
	SyncedAt    time.Time
	New         int
	Timeout     time.Duration
	Reason      string // "LOGGED OUT", "BANNED", or the error text
	GroupsFound int
	GroupsTotal int
	Missing     []string // JIDs
	StoreErrors int
}

type Input struct {
	BuiltAt    time.Time
	LastSentAt time.Time // zero on the first feed
	Retention  time.Duration
	Groups     []Group // config order; Name already sanitised
	Messages   []store.Message
	Health     Health
	MaxChars   int
	MaxBytes   int
	Version    string
}

type Result struct {
	Text                         string
	Messages, Late, Trimmed, Cut int
}

// Date formats t as "26 Oct 2026" in London time.
func Date(t time.Time) string { return t.In(london).Format("2 Jan 2006") }

// Build renders the feed. If the text exceeds MaxBytes, the oldest messages
// (by TS, across all groups) are dropped until it fits.
func Build(in Input) Result {
	idx := make(map[string]int, len(in.Groups))
	for i, g := range in.Groups {
		if _, ok := idx[g.JID]; !ok {
			idx[g.JID] = i
		}
	}
	var msgs []store.Message
	for _, m := range in.Messages {
		if _, ok := idx[m.Chat]; ok {
			msgs = append(msgs, m)
		}
	}
	sort.SliceStable(msgs, func(i, j int) bool {
		if !msgs[i].TS.Equal(msgs[j].TS) {
			return msgs[i].TS.Before(msgs[j].TS)
		}
		return msgs[i].ID < msgs[j].ID
	})

	r := render(in, idx, msgs, 0)
	for in.MaxBytes > 0 && len(r.Text) > in.MaxBytes && r.Cut < len(msgs) {
		r = render(in, idx, msgs, r.Cut+1)
	}
	return r
}

func render(in Input, idx map[string]int, all []store.Message, cut int) Result {
	start := in.LastSentAt
	if start.IsZero() {
		start = in.BuiltAt.Add(-in.Retention)
	}
	shown := all[cut:]
	byGroup := make([][]store.Message, len(in.Groups))
	for _, m := range shown {
		byGroup[idx[m.Chat]] = append(byGroup[idx[m.Chat]], m)
	}

	res := Result{Messages: len(shown), Cut: cut}
	var body strings.Builder
	for gi, g := range in.Groups {
		ms := byGroup[gi]
		if len(ms) == 0 {
			continue
		}
		fmt.Fprintf(&body, "===== GROUP: %s =====\n", g.Name)
		lastDay := ""
		for _, m := range ms {
			lt := m.TS.In(london)
			if day := lt.Format("Mon 2 Jan"); day != lastDay {
				fmt.Fprintf(&body, "--- %s ---\n", day)
				lastDay = day
			}
			late := m.TS.Before(start) && !m.Sent
			if late {
				res.Late++
			}
			text, trimmed := trim(m.Text, in.MaxChars)
			if trimmed {
				res.Trimmed++
			}
			body.WriteString("[" + lt.Format("15:04") + "] " + m.Sender)
			switch {
			case m.Edited && late:
				body.WriteString(" (edited, late)")
			case m.Edited:
				body.WriteString(" (edited)")
			case late:
				body.WriteString(" (late)")
			}
			if m.ReplySender != "" || m.ReplySnippet != "" {
				body.WriteString(" ↪ " + m.ReplySender + " \"" + m.ReplySnippet + "\"")
			}
			body.WriteString(": " + text + "\n")
		}
		body.WriteString("\n")
	}
	if len(shown) == 0 {
		body.WriteString("No new messages\n\n")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "WA-FEED %s\n", Date(in.BuiltAt))
	fmt.Fprintf(&b, "window: %s – %s (Europe/London)\n", start.In(london).Format(stamp), in.BuiltAt.In(london).Format(stamp))
	fmt.Fprintf(&b, "groups: %d · messages: %d · late: %d · trimmed: %d · cut: %d\n",
		len(in.Groups), res.Messages, res.Late, res.Trimmed, res.Cut)
	if !in.LastSentAt.IsZero() && in.LastSentAt.Before(in.BuiltAt.Add(-in.Retention)) {
		fmt.Fprintf(&b, "gap: no feed since %s; older messages already purged\n", in.LastSentAt.In(london).Format(stamp))
	}
	b.WriteString("\n")
	b.WriteString(body.String())
	b.WriteString("===== END OF MESSAGES =====\n--- feedbridge health ---\n")

	h := in.Health
	switch h.Status {
	case "synced":
		fmt.Fprintf(&b, "whatsapp: synced %s (%d new)", h.SyncedAt.In(london).Format("15:04"), h.New)
	case "partial":
		fmt.Fprintf(&b, "whatsapp: PARTIAL — sync timed out after %s (%d new)", h.Timeout, h.New)
	default:
		fmt.Fprintf(&b, "whatsapp: NOT SYNCED — %s", h.Reason)
	}
	fmt.Fprintf(&b, " | groups: %d/%d found", h.GroupsFound, h.GroupsTotal)
	if len(h.Missing) > 0 {
		fmt.Fprintf(&b, " (missing: %s)", strings.Join(h.Missing, ", "))
	}
	b.WriteString("\n")
	if len(all) == 0 {
		b.WriteString("store: 0 messages in window\n")
	} else {
		fmt.Fprintf(&b, "store: %d messages in window, oldest %s\n", len(all), all[0].TS.In(london).Format(stamp))
	}
	if h.StoreErrors > 0 {
		fmt.Fprintf(&b, "warning: %d messages could not be stored\n", h.StoreErrors)
	}
	fmt.Fprintf(&b, "version: %s\n", in.Version)
	res.Text = b.String()
	return res
}

// trim cuts s to max runes and appends the trimmed note.
func trim(s string, max int) (string, bool) {
	n := utf8.RuneCountInString(s)
	if max <= 0 || n <= max {
		return s, false
	}
	i := 0
	for c := 0; c < max; c++ {
		_, w := utf8.DecodeRuneInString(s[i:])
		i += w
	}
	return fmt.Sprintf("%s… [trimmed, %d chars total]", s[:i], n), true
}

// Package run orchestrates one daily feed run against interfaces.
package run

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"time"

	"feedbridge/internal/config"
	"feedbridge/internal/feed"
	"feedbridge/internal/sanitize"
	"feedbridge/internal/store"
	"feedbridge/internal/webhook"
)

type SyncResult struct {
	Synced      bool
	New         int
	StoreErrors int
	Err         error // ErrLoggedOut, ErrBanned or another fatal error during sync
}

type WhatsApp interface {
	Connect(ctx context.Context) error // returns ErrLoggedOut, ErrBanned, or a connection error
	WaitSynced(ctx context.Context, timeout time.Duration) SyncResult
	GroupName(ctx context.Context, jid string) (string, error) // ErrNotMember if not in group
	JoinedGroups(ctx context.Context) ([]GroupInfo, error)
	Disconnect()
}

// GroupInfo is one group the linked number belongs to.
type GroupInfo struct{ JID, Name string }

var (
	ErrLoggedOut = errors.New("logged out")
	ErrBanned    = errors.New("banned")
	ErrNotMember = errors.New("not a member of group")
)

type Deps struct {
	Cfg     config.Config
	WA      WhatsApp
	Store   *store.Store
	Send    func(ctx context.Context, p webhook.Payload) error
	Now     func() time.Time
	Version string
	Log     *slog.Logger
}

// Run performs one run; it returns nil only when the feed got "ok".
func Run(ctx context.Context, d Deps) error {
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	cfg := d.Cfg
	h := feed.Health{Timeout: cfg.SyncTimeout}

	start := time.Now()
	connectErr := d.WA.Connect(ctx)
	connected := connectErr == nil
	available := connected // WhatsApp usable for group names
	if connected {
		log.Info("connect", "ok", true, "ms", time.Since(start).Milliseconds())
		t := time.Now()
		res := d.WA.WaitSynced(ctx, cfg.SyncTimeout)
		h.New, h.StoreErrors = res.New, res.StoreErrors
		if res.Err != nil {
			available = false
			h.Status, h.Reason = "not_synced", reason(res.Err)
			log.Warn("sync", "status", h.Status, "reason", h.Reason, "new", res.New,
				"store_errors", res.StoreErrors, "ms", time.Since(t).Milliseconds())
		} else {
			h.SyncedAt = d.Now()
			h.Status = "partial"
			if res.Synced {
				h.Status = "synced"
			}
			log.Info("sync", "status", h.Status, "new", res.New, "store_errors", res.StoreErrors, "ms", time.Since(t).Milliseconds())
		}
	} else {
		h.Status, h.Reason = "not_synced", reason(connectErr)
		log.Warn("connect", "ok", false, "reason", h.Reason)
	}

	var groups []feed.Group
	if cfg.AllGroups {
		groups = joinedGroups(ctx, d.WA, available, cfg.ExcludeGroups, &h, log)
	} else {
		groups = listedGroups(ctx, d.WA, available, cfg.Groups, &h, log)
	}

	if connected {
		d.WA.Disconnect()
	}

	builtAt := d.Now().Truncate(time.Second)
	if n, err := d.Store.Purge(builtAt, cfg.Retention); err != nil {
		log.Error("purge failed", "err", err)
	} else {
		log.Info("purge", "removed", n)
	}
	pending, err := d.Store.Pending(builtAt)
	if err != nil {
		log.Error("pending failed", "err", err)
		return err
	}
	if cfg.AllGroups {
		groups = addUnlisted(groups, pending, cfg.ExcludeGroups)
	}
	last, _, err := d.Store.LastSentAt()
	if err != nil {
		log.Error("last_sent_at failed", "err", err)
		return err
	}

	res := feed.Build(feed.Input{
		BuiltAt: builtAt, LastSentAt: last, Retention: cfg.Retention, Groups: groups,
		Messages: pending, Health: h, MaxChars: cfg.MaxMessageChars, MaxBytes: cfg.MaxFeedBytes,
		Version: d.Version,
	})
	log.Info("build", "pending", len(pending), "messages", res.Messages, "late", res.Late,
		"trimmed", res.Trimmed, "cut", res.Cut, "bytes", len(res.Text))

	p := webhook.Payload{
		Secret: cfg.Secret,
		ID:     builtAt.UTC().Format(time.RFC3339),
		Date:   feed.Date(builtAt),
		Text:   res.Text,
	}
	t := time.Now()
	if err := d.Send(ctx, p); err != nil {
		log.Error("send failed", "err", err, "ms", time.Since(t).Milliseconds()) // err is URL-free
		return err
	}
	log.Info("send", "ok", true, "ms", time.Since(t).Milliseconds())

	ids := make([]string, len(pending))
	for i, m := range pending {
		ids[i] = m.ID
	}
	if err := d.Store.CommitSent(builtAt, ids); err != nil {
		log.Error("commit failed", "err", err)
		return err
	}
	log.Info("commit", "ids", len(ids))
	return nil
}

// listedGroups names each configured group and counts the ones the number
// is still in.
func listedGroups(ctx context.Context, wa WhatsApp, available bool, jids []string, h *feed.Health, log *slog.Logger) []feed.Group {
	groups := make([]feed.Group, 0, len(jids))
	h.GroupsTotal = len(jids)
	for _, jid := range jids {
		g := feed.Group{JID: jid, Name: jid}
		found := true
		if available {
			name, err := wa.GroupName(ctx, jid)
			switch {
			case errors.Is(err, ErrNotMember):
				found = false
			case err == nil:
				if n := sanitize.Line(name); n != "" {
					g.Name = n
				}
			}
		}
		if found {
			h.GroupsFound++
		} else {
			h.Missing = append(h.Missing, jid)
		}
		groups = append(groups, g)
	}
	log.Info("groups", "found", h.GroupsFound, "total", h.GroupsTotal)
	return groups
}

// joinedGroups lists every group the number is in, minus the excluded ones,
// sorted by name. When WhatsApp is unavailable the list is empty and the
// footer says the groups were not checked.
func joinedGroups(ctx context.Context, wa WhatsApp, available bool, exclude []string, h *feed.Health, log *slog.Logger) []feed.Group {
	h.AllGroups = true
	if !available {
		h.GroupsUnchecked = true
		return nil
	}
	joined, err := wa.JoinedGroups(ctx)
	if err != nil {
		h.GroupsUnchecked = true
		log.Warn("groups", "err", err)
		return nil
	}
	skip := make(map[string]bool, len(exclude))
	for _, jid := range exclude {
		skip[jid] = true
	}
	var groups []feed.Group
	for _, j := range joined {
		if skip[j.JID] {
			h.Excluded++
			continue
		}
		g := feed.Group{JID: j.JID, Name: sanitize.Line(j.Name)}
		if g.Name == "" {
			g.Name = j.JID
		}
		groups = append(groups, g)
	}
	sort.SliceStable(groups, func(i, k int) bool {
		if groups[i].Name != groups[k].Name {
			return groups[i].Name < groups[k].Name
		}
		return groups[i].JID < groups[k].JID
	})
	h.GroupsFound = len(groups)
	log.Info("groups", "joined", h.GroupsFound, "excluded", h.Excluded)
	return groups
}

// addUnlisted appends, headed by JID, any group with pending messages that
// joinedGroups did not return: one the number has since left, or every
// group when WhatsApp was unavailable. Excluded groups stay out.
func addUnlisted(groups []feed.Group, pending []store.Message, exclude []string) []feed.Group {
	seen := make(map[string]bool, len(groups)+len(exclude))
	for _, g := range groups {
		seen[g.JID] = true
	}
	for _, jid := range exclude {
		seen[jid] = true
	}
	var extra []feed.Group
	for _, m := range pending {
		if !seen[m.Chat] {
			seen[m.Chat] = true
			extra = append(extra, feed.Group{JID: m.Chat, Name: m.Chat})
		}
	}
	sort.Slice(extra, func(i, k int) bool { return extra[i].JID < extra[k].JID })
	return append(groups, extra...)
}

// reason is the health-line text for an unavailable WhatsApp.
func reason(err error) string {
	switch {
	case errors.Is(err, ErrLoggedOut):
		return "LOGGED OUT"
	case errors.Is(err, ErrBanned):
		return "BANNED"
	}
	return sanitize.Line(err.Error())
}

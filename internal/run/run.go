// Package run orchestrates one daily feed run against interfaces.
package run

import (
	"context"
	"errors"
	"log/slog"
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
}

type WhatsApp interface {
	Connect(ctx context.Context) error // returns ErrLoggedOut, ErrBanned, or a connection error
	WaitSynced(ctx context.Context, timeout time.Duration) SyncResult
	GroupName(ctx context.Context, jid string) (string, error) // ErrNotMember if not in group
	Disconnect()
}

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
	if connected {
		log.Info("connect", "ok", true, "ms", time.Since(start).Milliseconds())
		t := time.Now()
		res := d.WA.WaitSynced(ctx, cfg.SyncTimeout)
		h.SyncedAt = d.Now()
		h.New, h.StoreErrors = res.New, res.StoreErrors
		h.Status = "partial"
		if res.Synced {
			h.Status = "synced"
		}
		log.Info("sync", "status", h.Status, "new", res.New, "store_errors", res.StoreErrors, "ms", time.Since(t).Milliseconds())
	} else {
		h.Status = "not_synced"
		switch {
		case errors.Is(connectErr, ErrLoggedOut):
			h.Reason = "LOGGED OUT"
		case errors.Is(connectErr, ErrBanned):
			h.Reason = "BANNED"
		default:
			h.Reason = sanitize.Line(connectErr.Error())
		}
		log.Warn("connect", "ok", false, "reason", h.Reason)
	}

	groups := make([]feed.Group, 0, len(cfg.Groups))
	h.GroupsTotal = len(cfg.Groups)
	for _, jid := range cfg.Groups {
		g := feed.Group{JID: jid, Name: jid}
		found := true
		if connected {
			name, err := d.WA.GroupName(ctx, jid)
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
		log.Error("send failed", "ms", time.Since(t).Milliseconds())
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

// Command feedbridge collects two WhatsApp groups' messages once a day and
// posts them as a plain-text feed to an Apps Script webhook.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"
	_ "time/tzdata"

	"feedbridge/internal/config"
	"feedbridge/internal/lock"
	"feedbridge/internal/run"
	"feedbridge/internal/sanitize"
	"feedbridge/internal/store"
	"feedbridge/internal/wa"
	"feedbridge/internal/webhook"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
	exitBusy  = 75 // EX_TEMPFAIL
)

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

func realMain(args []string, stdout, stderr io.Writer) int {
	// Databases, WAL files and the lock file must be private (mode 600),
	// whoever starts us and with whatever umask.
	syscall.Umask(0o077)
	fs := flag.NewFlagSet("feedbridge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "/etc/feedbridge/config.toml", "config file")
	doRun := fs.Bool("run", false, "collect messages and send the daily feed")
	doLink := fs.Bool("link", false, "link as a companion device by QR code")
	doList := fs.Bool("list-groups", false, "print each joined group's JID and name")
	doVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *doVersion {
		fmt.Fprintln(stdout, version)
		return exitOK
	}
	modes := 0
	for _, b := range []bool{*doRun, *doLink, *doList} {
		if b {
			modes++
		}
	}
	if modes != 1 || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: feedbridge [--config path] --run | --link | --list-groups | --version")
		return exitUsage
	}

	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	if *doRun && len(cfg.Groups) == 0 {
		fmt.Fprintln(stderr, "whatsapp.groups: no groups configured; run --list-groups and add their JIDs")
		return exitUsage
	}

	release, err := lock.Acquire(cfg.LockPath())
	if errors.Is(err, lock.ErrBusy) {
		fmt.Fprintln(stderr, "run in progress")
		return exitBusy
	} else if err != nil {
		fmt.Fprintln(stderr, err)
		return exitFail
	}
	defer release()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Every mode opens the message store, so anything WhatsApp delivers
	// while connected is filtered and kept rather than lost.
	st, err := store.Open(cfg.StorePath)
	if err != nil {
		log.Error("open store", "err", err)
		return exitFail
	}
	defer st.Close()

	switch {
	case *doRun:
		return runFeed(ctx, cfg, st, log)
	case *doLink:
		return link(ctx, cfg, st, stdout, log)
	default:
		return listGroups(ctx, cfg, st, stdout, stderr, log)
	}
}

func runFeed(ctx context.Context, cfg config.Config, st *store.Store, log *slog.Logger) int {
	var w run.WhatsApp
	c, err := wa.New(ctx, cfg, st)
	if err != nil {
		// Still send the feed; the reason goes in the health footer.
		log.Error("whatsapp client", "err", err)
		w = failedWA{err: err}
	} else {
		defer c.Close()
		w = c
	}
	sender := &webhook.Sender{URL: cfg.WebhookURL}
	err = run.Run(ctx, run.Deps{
		Cfg: cfg, WA: w, Store: st, Send: sender.Send,
		Now: time.Now, Version: version, Log: log,
	})
	if err != nil {
		return exitFail
	}
	return exitOK
}

func link(ctx context.Context, cfg config.Config, st *store.Store, stdout io.Writer, log *slog.Logger) int {
	c, err := wa.New(ctx, cfg, st)
	if err != nil {
		log.Error("whatsapp client", "err", err)
		return exitFail
	}
	defer c.Close()
	if err := c.Link(ctx, stdout); err != nil {
		log.Error("link", "err", err)
		return exitFail
	}
	fmt.Fprintln(stdout, "Done. Now run --list-groups.")
	return exitOK
}

func listGroups(ctx context.Context, cfg config.Config, st *store.Store, stdout, stderr io.Writer, log *slog.Logger) int {
	c, err := wa.New(ctx, cfg, st)
	if err != nil {
		log.Error("whatsapp client", "err", err)
		return exitFail
	}
	defer c.Close()
	if err := c.Connect(ctx); err != nil {
		if errors.Is(err, run.ErrLoggedOut) {
			fmt.Fprintln(stderr, "not linked: run --link first")
		} else {
			log.Error("connect", "err", err)
		}
		return exitFail
	}
	defer c.Disconnect()
	groups, err := c.JoinedGroups(ctx)
	if err != nil {
		log.Error("list groups", "err", err)
		return exitFail
	}
	for i := range groups {
		groups[i].Name = sanitize.Line(groups[i].Name)
	}
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].Name != groups[j].Name {
			return groups[i].Name < groups[j].Name
		}
		return groups[i].JID < groups[j].JID
	})
	for _, g := range groups {
		fmt.Fprintf(stdout, "%s\t%s\n", g.JID, g.Name)
	}
	return exitOK
}

// failedWA stands in when the session store cannot be opened, so the run
// still sends a feed with the reason in its health footer.
type failedWA struct{ err error }

func (f failedWA) Connect(context.Context) error { return f.err }
func (f failedWA) WaitSynced(context.Context, time.Duration) run.SyncResult {
	return run.SyncResult{}
}
func (f failedWA) GroupName(context.Context, string) (string, error) { return "", f.err }
func (f failedWA) Disconnect()                                       {}

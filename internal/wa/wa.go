// Package wa wraps whatsmeow, backed by pure-Go SQLite (modernc.org/sqlite).
// It is the only package that talks to WhatsApp, and it only ever reads:
// it never calls a send, read-receipt, presence or typing API.
package wa

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
	"modernc.org/sqlite"

	"feedbridge/internal/config"
	"feedbridge/internal/ingest"
	"feedbridge/internal/run"
	fbstore "feedbridge/internal/store"
)

var registerOnce sync.Once

// OpenContainer opens (creating if needed) the whatsmeow session store at path.
// modernc's driver is registered under "sqlite3", the name whatsmeow expects.
func OpenContainer(ctx context.Context, path string) (*sqlstore.Container, error) {
	registerOnce.Do(func() {
		sql.Register("sqlite3", &sqlite.Driver{})
	})
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", path)
	return sqlstore.New(ctx, "sqlite3", dsn, waLog.Noop)
}

// GroupInfo is one group the linked number belongs to.
type GroupInfo struct{ JID, Name string }

// Client adapts whatsmeow to run.WhatsApp and feeds every event through ingest.
type Client struct {
	cli         *whatsmeow.Client
	container   *sqlstore.Container
	st          *fbstore.Store
	filter      ingest.Filter
	names       ingest.Names
	log         *slog.Logger
	syncTimeout time.Duration

	applied   atomic.Int64 // inserts and edits applied
	storeErrs atomic.Int64 // Apply failures

	synced     chan struct{} // closed on OfflineSyncCompleted
	syncedOnce sync.Once
	connected  chan struct{} // closed on the first Connected
	connOnce   sync.Once
	fatal      chan struct{} // closed on logout, ban or another unrecoverable failure
	fatalOnce  sync.Once
	fatalErr   error // set before fatal is closed

	mu    sync.Mutex
	start time.Time // when Connect began; the sync budget runs from here

	// History syncs are downloaded and applied on whatsmeow's own goroutine,
	// which can outlive Disconnect; Close waits for them (up to closeWait).
	onHistory   func(*events.HistorySync) // default c.handleHistory; replaced in tests
	closeWait   time.Duration
	histMu      sync.Mutex
	histClosed  bool
	histWG      sync.WaitGroup
	histPending atomic.Int64
}

var _ run.WhatsApp = (*Client)(nil)

// New opens cfg.SessionDB and prepares (but does not connect) a client.
// Events for cfg.Groups are written to st.
func New(ctx context.Context, cfg config.Config, st *fbstore.Store) (*Client, error) {
	container, err := OpenContainer(ctx, cfg.SessionDB)
	if err != nil {
		return nil, fmt.Errorf("session store: %w", err)
	}
	dev, err := container.GetFirstDevice(ctx)
	if err != nil {
		container.Close()
		return nil, fmt.Errorf("session store: %w", err)
	}
	groups := make(map[types.JID]bool, len(cfg.Groups))
	for _, g := range cfg.Groups {
		j, err := types.ParseJID(g)
		if err != nil {
			container.Close()
			return nil, fmt.Errorf("whatsapp.groups: %q: %w", g, err)
		}
		groups[j] = true
	}

	store.DeviceProps.RequireFullSync = proto.Bool(false)
	log := slog.Default()
	cli := whatsmeow.NewClient(dev, NewLogger(log.With("component", "whatsmeow")))
	cli.EnableAutoReconnect = true
	cli.InitialAutoReconnect = true

	c := &Client{
		cli:         cli,
		container:   container,
		st:          st,
		filter:      ingest.Filter{Groups: groups, Retention: cfg.Retention, Now: time.Now},
		names:       contactNames{contacts: dev.Contacts},
		log:         log,
		syncTimeout: cfg.SyncTimeout,
		synced:      make(chan struct{}),
		connected:   make(chan struct{}),
		fatal:       make(chan struct{}),
		closeWait:   30 * time.Second,
	}
	c.onHistory = c.handleHistory
	cli.AddEventHandler(c.handle)
	return c, nil
}

// Close releases the session database. Call after Disconnect, and before
// closing the message store: it first waits up to closeWait for any history
// sync still being applied, and drops any that arrive later.
func (c *Client) Close() error {
	c.histMu.Lock()
	c.histClosed = true
	c.histMu.Unlock()
	done := make(chan struct{})
	go func() { c.histWG.Wait(); close(done) }()
	t := time.NewTimer(c.closeWait)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		c.log.Warn("history sync still running at close", "pending", c.histPending.Load(),
			"wait", c.closeWait)
	}
	return c.container.Close()
}

func (c *Client) handle(evt any) {
	switch e := evt.(type) {
	case *events.Message:
		if op, ok := ingest.FromMessage(e, c.filter, c.names); ok {
			c.apply(op)
		}
	case *events.HistorySync:
		c.histMu.Lock()
		if c.histClosed {
			c.histMu.Unlock()
			c.log.Warn("history sync after close dropped", "conversations", len(e.Data.GetConversations()))
			return
		}
		c.histWG.Add(1)
		c.histPending.Add(1)
		c.histMu.Unlock()
		defer func() { c.histPending.Add(-1); c.histWG.Done() }()
		c.onHistory(e)
	case *events.OfflineSyncCompleted:
		c.log.Info("offline sync completed", "count", e.Count)
		c.syncedOnce.Do(func() { close(c.synced) })
	case *events.Connected:
		c.connOnce.Do(func() { close(c.connected) })
	case *events.PairSuccess:
		c.log.Info("paired")
	case *events.Disconnected:
		c.log.Warn("disconnected; reconnecting")
	case *events.LoggedOut:
		if e.Reason == events.ConnectFailureUnknownLogout { // 406, "BANNED" in WhatsApp Web
			c.setFatal(run.ErrBanned)
		} else {
			c.setFatal(run.ErrLoggedOut)
		}
	case *events.TemporaryBan:
		c.setFatal(run.ErrBanned)
	case *events.ConnectFailure:
		switch e.Reason {
		case events.ConnectFailureTempBanned, events.ConnectFailureUnknownLogout:
			c.setFatal(run.ErrBanned)
		default:
			if e.Reason.IsLoggedOut() {
				c.setFatal(run.ErrLoggedOut)
			} else {
				c.setFatal(fmt.Errorf("connect failure %d", int(e.Reason)))
			}
		}
	case *events.ClientOutdated:
		c.setFatal(errors.New("client outdated"))
	case *events.StreamReplaced:
		c.setFatal(errors.New("session replaced by another connection"))
	}
}

func (c *Client) handleHistory(e *events.HistorySync) {
	ops := ingest.FromHistory(e, c.filter, c.names, c.cli.ParseWebMessage)
	for _, op := range ops {
		c.apply(op)
	}
	c.log.Info("history sync", "conversations", len(e.Data.GetConversations()), "ops", len(ops))
}

func (c *Client) apply(op ingest.Op) {
	if c.st == nil {
		return
	}
	if err := ingest.Apply(c.st, op); err != nil {
		c.storeErrs.Add(1)
		c.log.Warn("store error", "op", int(op.Kind), "chat", op.Msg.Chat, "err", err)
		return
	}
	if op.Kind == ingest.OpInsert || op.Kind == ingest.OpEdit {
		c.applied.Add(1)
	}
}

func (c *Client) setFatal(err error) {
	c.fatalOnce.Do(func() {
		c.fatalErr = err
		close(c.fatal)
		c.log.Warn("whatsapp unavailable", "reason", err.Error())
	})
}

// Connect connects and waits until logged in, retrying with backoff until
// the sync timeout. It returns run.ErrLoggedOut or run.ErrBanned when the
// session is gone. On any error the client is left disconnected.
func (c *Client) Connect(ctx context.Context) error {
	if c.cli.Store.ID == nil {
		return run.ErrLoggedOut
	}
	c.mu.Lock()
	c.start = time.Now()
	c.mu.Unlock()
	if err := c.cli.ConnectContext(ctx); err != nil {
		c.Disconnect()
		return err
	}
	t := time.NewTimer(c.syncTimeout)
	defer t.Stop()
	select {
	case <-c.connected:
		return nil
	case <-c.fatal:
		c.Disconnect()
		return c.fatalErr
	case <-t.C:
		c.Disconnect()
		return errors.New("connect timed out")
	case <-ctx.Done():
		c.Disconnect()
		return ctx.Err()
	}
}

// WaitSynced waits for WhatsApp to finish delivering queued messages. The
// timeout counts from the start of Connect, so connecting and syncing share
// one feed.sync_timeout budget. Err is set (run.ErrLoggedOut, run.ErrBanned
// or another fatal error) when the session became unusable while waiting.
func (c *Client) WaitSynced(ctx context.Context, timeout time.Duration) run.SyncResult {
	c.mu.Lock()
	start := c.start
	c.mu.Unlock()
	if start.IsZero() {
		start = time.Now()
	}
	t := time.NewTimer(time.Until(start.Add(timeout)))
	defer t.Stop()
	synced := false
	var err error
	select {
	case <-c.synced:
		synced = true
	case <-c.fatal:
		err = c.fatalErr
	case <-t.C:
	case <-ctx.Done():
	}
	return run.SyncResult{
		Synced:      synced,
		New:         int(c.applied.Load()),
		StoreErrors: int(c.storeErrs.Load()),
		Err:         err,
	}
}

// GroupName returns the group's current subject, or run.ErrNotMember.
func (c *Client) GroupName(ctx context.Context, jid string) (string, error) {
	j, err := types.ParseJID(jid)
	if err != nil {
		return "", err
	}
	info, err := c.cli.GetGroupInfo(ctx, j)
	if err != nil {
		return "", groupErr(err)
	}
	return info.Name, nil
}

func groupErr(err error) error {
	if errors.Is(err, whatsmeow.ErrNotInGroup) || errors.Is(err, whatsmeow.ErrGroupNotFound) ||
		errors.Is(err, whatsmeow.ErrIQForbidden) || errors.Is(err, whatsmeow.ErrIQNotFound) {
		return run.ErrNotMember
	}
	return err
}

// Disconnect stops auto-reconnect, then closes the connection.
func (c *Client) Disconnect() {
	c.cli.EnableAutoReconnect = false
	c.cli.Disconnect()
}

// Link pairs a new companion device: it prints QR codes to out until one is
// scanned, then stays connected for the sync timeout so the phone's history
// sync is stored, and disconnects.
func (c *Client) Link(ctx context.Context, out io.Writer) error {
	if c.cli.Store.ID != nil {
		return errors.New("already linked; remove the session database to link again")
	}
	qr, err := c.cli.GetQRChannel(ctx)
	if err != nil {
		return err
	}
	if err := c.cli.ConnectContext(ctx); err != nil {
		return err
	}
	defer c.Disconnect()
	paired := false
	for item := range qr {
		switch item.Event {
		case "code":
			qrterminal.GenerateHalfBlock(item.Code, qrterminal.L, out)
			fmt.Fprintln(out, "Scan in WhatsApp > Linked devices > Link a device.")
		case whatsmeow.QRChannelSuccess.Event:
			paired = true
		default:
			if item.Error != nil {
				return fmt.Errorf("link: %w", item.Error)
			}
			return fmt.Errorf("link: %s", item.Event)
		}
		if paired {
			break
		}
	}
	if !paired {
		return errors.New("link: QR channel closed before pairing")
	}
	fmt.Fprintf(out, "Linked. Receiving history for up to %s...\n", c.syncTimeout)
	t := time.NewTimer(c.syncTimeout)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	case <-c.fatal:
		return c.fatalErr
	}
	return nil
}

// JoinedGroups lists the groups the linked number is in. Call after Connect.
func (c *Client) JoinedGroups(ctx context.Context) ([]GroupInfo, error) {
	gs, err := c.cli.GetJoinedGroups(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]GroupInfo, 0, len(gs))
	for _, g := range gs {
		out = append(out, GroupInfo{JID: g.JID.String(), Name: g.Name})
	}
	return out, nil
}

// contactNames resolves sender names from whatsmeow's own contact store.
type contactNames struct{ contacts store.ContactStore }

func (n contactNames) Name(jid types.JID) string {
	if n.contacts == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ci, err := n.contacts.GetContact(ctx, jid)
	if err != nil {
		return ""
	}
	for _, s := range []string{ci.PushName, ci.FullName, ci.FirstName} {
		if s != "" {
			return s
		}
	}
	return ""
}

// NewLogger adapts whatsmeow's logger onto slog: Debug and Info are dropped,
// Warn and Error are kept. Only numbers, booleans, durations and errors are
// formatted into the message; any other argument (which could be message
// text or a name) is replaced by "<redacted>".
func NewLogger(l *slog.Logger) waLog.Logger { return slogLogger{l: l} }

type slogLogger struct{ l *slog.Logger }

func (s slogLogger) Warnf(msg string, args ...any)  { s.l.Warn(safeFormat(msg, args)) }
func (s slogLogger) Errorf(msg string, args ...any) { s.l.Error(safeFormat(msg, args)) }
func (s slogLogger) Infof(string, ...any)           {}
func (s slogLogger) Debugf(string, ...any)          {}
func (s slogLogger) Sub(module string) waLog.Logger { return slogLogger{l: s.l.With("module", module)} }

func safeFormat(msg string, args []any) string {
	safe := make([]any, len(args))
	for i, a := range args {
		switch a.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64,
			float32, float64, bool, time.Duration, error:
			safe[i] = a
		default:
			safe[i] = "<redacted>"
		}
	}
	return fmt.Sprintf(msg, safe...)
}

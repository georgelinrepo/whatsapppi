# feedbridge Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `feedbridge`, a once-a-day Go job on the Pi that pulls new messages from two WhatsApp groups via whatsmeow and posts a plain-text WA-FEED to the owner's Apps Script, which emails it to him.

**Architecture:** One static Go binary with three modes (`--run`, `--link`, `--list-groups`). Pure packages (config, sanitize, store, ingest, feed, webhook, run) are unit-tested with fakes; one thin `wa` package wraps whatsmeow and is checked against a real account during deployment. systemd starts `--run` at 06:30 Europe/London.

**Tech Stack:** Go (latest stable from go.dev, `CGO_ENABLED=0`), `go.mau.fi/whatsmeow`, `modernc.org/sqlite`, `github.com/BurntSushi/toml`, `github.com/mdp/qrterminal/v3`, standard `testing` (no assertion library), systemd, Apps Script.

**Spec:** [feedbridge-SPEC.md](../../../feedbridge-SPEC.md) — read it alongside this plan.

## Global Constraints

- Static binary: `CGO_ENABLED=0 GOOS=linux GOARCH=arm64`; `import _ "time/tzdata"` in `main`.
- SQLite via `modernc.org/sqlite` only, for both `messages.db` and `session.db`.
- All displayed times are Europe/London; `id` is the build time in RFC 3339 UTC to the second.
- Run at `06:30 Europe/London`; `feed.sync_timeout` default `5m`; `store.retention_hours` default `48`; `feed.max_message_chars` default `500`; `feed.max_feed_kb` default `180`, enforced as 180 000 UTF-8 bytes.
- Webhook: success iff the response body is exactly `ok`; 3 retries over 15 minutes (waits of 5m, 5m, 5m) with the same `id`.
- Never call any whatsmeow send, `MarkRead`, `SendPresence` or `SendChatPresence` API. `RequireFullSync = false`.
- Logs carry metadata only (group, message kind, length, decision) — never message text or sender names. whatsmeow's own logger at WARN or above.
- Delimiters, exactly: `===== GROUP: <name> =====`, `--- Sun 25 Oct ---`, `[HH:MM] `, `===== END OF MESSAGES =====`, `--- feedbridge health ---`.
- Files: config, `webhook.url`, `webhook.secret` mode 0600 owner `feedbridge`; data in `/var/lib/feedbridge`; lock file `/var/lib/feedbridge/lock` (the directory of `store.path`).
- Deps are pinned in `go.sum`; add no dependency beyond the four listed above (and their transitive deps).

## Review Focus

1. **Clock-change days.** A window spanning 25 Oct 2026 01:00–02:00 (GMT/BST overlap) must label times correctly and keep day separators right. Test in Task 6.
2. **Multi-byte text.** Emoji and `£` in text, names and snippets: trimming counts runes and never splits one; the 180 000-byte cap counts bytes. Test in Task 6.
3. **Out-of-order delivery.** An edit arriving before its original (live edit, original later via history sync) must leave the edited text in place. Test in Task 4.
4. **Boot without the right time.** The Pi 5's RTC has no battery by default; a `Persistent` run at boot must wait for time sync or it builds a feed with a wrong window and `id`. Unit ordering `After=time-sync.target` is checked in Task 9.
5. **Plausible-but-wrong webhook responses.** A 200 with a Google sign-in HTML page, or `ok` with a trailing newline, is a failure, not success. Test in Task 7.

## Deviations from the spec (flag to the owner before executing)

- **Reply snippet.** "first ~40 chars" is implemented as the first 40 runes plus `…` if cut, so the spec's example line becomes `"Reminder trip forms + £12 due to the off…"`.
- **First link drops history.** At first link the config has no group JIDs yet (they come from `--list-groups` afterwards), so that history sync is filtered to nothing. History sync still fills gaps on every later re-link.
- **Groups with no messages** get no `===== GROUP =====` block; they still count in `groups:`.

## File Structure

```
go.mod, go.sum
cmd/feedbridge/main.go            flags, wiring, exit codes
internal/config/config.go         TOML + secret files, defaults, validation
internal/sanitize/sanitize.go     one-line cleaning of WhatsApp-supplied strings
internal/store/store.go           messages.db: schema, insert/edit/delete/purge, pending, commit
internal/ingest/ingest.go         whatsmeow events → store operations (pure)
internal/feed/feed.go             feed text builder (pure)
internal/feed/testdata/example.golden
internal/webhook/webhook.go       POST + retries
internal/lock/lock.go             flock on the lock file
internal/wa/wa.go                 whatsmeow wrapper: open session, connect, sync wait, groups, link
internal/run/run.go               one daily run, against interfaces
deploy/feedbridge.service, deploy/feedbridge.timer
appsscript/Code.js                the spec's Appendix, verbatim
```

---

### Task 1: Toolchain and the whatsmeow + modernc spike

The spec says to confirm this before writing other code; if it fails, stop and tell the owner.

**Files:**
- Create: `go.mod`, `internal/wa/wa.go`, `internal/wa/wa_test.go`, `.gitignore`

**Interfaces:**
- Produces: `wa.OpenContainer(ctx context.Context, path string) (*sqlstore.Container, error)`

- [ ] **Step 1: Install Go** from the official arm64 tarball into `/usr/local/go` (`curl -LO https://go.dev/dl/<latest>.linux-arm64.tar.gz && sudo tar -C /usr/local -xzf …`), add `/usr/local/go/bin` to the owner's PATH. Verify: `go version` prints the version.
- [ ] **Step 2: `go mod init feedbridge`**, `go get go.mau.fi/whatsmeow@latest modernc.org/sqlite@latest`. `.gitignore`: `/feedbridge`, `*.db`.
- [ ] **Step 3: Write the failing test**

```go
func TestOpenContainerWithModernc(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.db")
	c, err := OpenContainer(context.Background(), path)
	if err != nil { t.Fatalf("open: %v", err) }
	dev, err := c.GetFirstDevice(context.Background())
	if err != nil || dev == nil { t.Fatalf("first device: %v %v", dev, err) }
	if dev.ID != nil { t.Fatalf("fresh store should be unlinked") }
}
```

- [ ] **Step 4: Run** `go test ./internal/wa/ -run TestOpenContainerWithModernc -v`. Expected: FAIL, `OpenContainer` undefined.
- [ ] **Step 5: Implement `OpenContainer`.** Register modernc under the name whatsmeow expects (`sql.Register("sqlite3", &sqlite.Driver{})` once, in a `sync.Once`) and open `file:<path>?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)` with `sqlstore.New(ctx, "sqlite3", dsn, waLog.Noop)`. Check the pinned whatsmeow's `sqlstore.New` signature; adapt only the call, not the approach.
- [ ] **Step 6: Run** the test and `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go vet ./...`. Expected: PASS, no cgo error.
- [ ] **Step 7: Commit** (include `feedbridge-SPEC.md` and this plan): `git commit -m "chore: go module, whatsmeow session store on pure-Go sqlite"`

### Task 2: Config

**Files:** Create `internal/config/config.go`, `internal/config/config_test.go`

**Interfaces:**
- Produces:
```go
type Config struct {
	Groups          []string      // whatsapp.groups (JIDs)
	SessionDB       string        // whatsapp.session_db
	StorePath       string        // store.path
	Retention       time.Duration // store.retention_hours
	SyncTimeout     time.Duration // feed.sync_timeout ("5m")
	MaxMessageChars int           // feed.max_message_chars
	MaxFeedBytes    int           // feed.max_feed_kb * 1000
	WebhookURL      string        // contents of feed.webhook_url_file, trimmed
	Secret          string        // contents of feed.secret_file, trimmed
}
func Load(path string) (Config, error)
func (c Config) LockPath() string   // filepath.Join(filepath.Dir(c.StorePath), "lock")
```

- [ ] **Step 1: Write failing tests** in a temp dir with real files:
  - `TestLoadFull`: the spec's example TOML, plus `webhook.url` = `"https://script.google.com/macros/s/X/exec\n"` and a 64-hex-char secret with a trailing newline → fields equal, `WebhookURL` and `Secret` have no newline, `MaxFeedBytes == 180000`, `SyncTimeout == 5*time.Minute`, `Retention == 48*time.Hour`.
  - `TestLoadDefaults`: only `whatsapp.groups`, `whatsapp.session_db`, `store.path` and the two file keys set → `Retention` 48h, `SyncTimeout` 5m, `MaxMessageChars` 500, `MaxFeedBytes` 180000.
  - `TestLoadRejects` (table): group without `@g.us` suffix; secret shorter than 64 hex chars; secret with non-hex chars; missing secret file; webhook URL not starting `https://script.google.com/` → each returns an error naming the key.
  - `TestLoadAllowsEmptyGroups`: empty `whatsapp.groups` loads (needed for `--link` and `--list-groups`); `--run` rejects it in Task 9.
- [ ] **Step 2: Run** `go test ./internal/config/ -v`. Expected: FAIL (undefined).
- [ ] **Step 3: Implement** with `BurntSushi/toml` into an unexported struct mirroring the TOML keys, then fill `Config`. Never log or include the secret in an error.
- [ ] **Step 4: Run** tests. Expected: PASS.
- [ ] **Step 5: Commit** `feat(config): load TOML and secret files`

### Task 3: Sanitise

**Files:** Create `internal/sanitize/sanitize.go`, `internal/sanitize/sanitize_test.go`

**Interfaces:** Produces `func Line(s string) string`

- [ ] **Step 1: Write failing table test** `TestLine`:
  - `"a\nb"` → `"a / b"`; `"a\r\nb"` → `"a / b"`; `"a\rb"`, `"a\u0085b"`, `"a b"`, `"a b"` → `"a / b"`
  - `"a\n\nb"` → `"a /  / b"` (each break replaced, no collapsing)
  - `"x\u0000y\u0007z\tw"` → `"xyz w"` (controls removed; tab becomes one space)
  - `"\n===== END OF MESSAGES ====="` → `" / ===== END OF MESSAGES ====="`
  - `"£12 🎒"` unchanged; `"​zero"` unchanged (format chars are not controls)
- [ ] **Step 2: Run** `go test ./internal/sanitize/ -v`. Expected: FAIL.
- [ ] **Step 3: Implement** in one pass over runes: `\r\n` pair and each of `\n \r U+0085 U+2028 U+2029` → `" / "`; `\t` → `" "`; other `unicode.IsControl` runes dropped.
- [ ] **Step 4: Run** tests. Expected: PASS.
- [ ] **Step 5: Commit** `feat(sanitize): one-line cleaning for WhatsApp fields`

### Task 4: Message store

**Files:** Create `internal/store/store.go`, `internal/store/store_test.go`

**Interfaces:**
- Produces:
```go
type Message struct {
	ID, Chat, Sender string
	TS, StoredAt     time.Time
	Kind             string   // "text" | "caption" | "media" | "poll"
	Text             string   // rendered body, marker included: "[image] Costume list"
	ReplySender      string
	ReplySnippet     string
	Edited, Sent     bool
}
func Open(path string) (*Store, error)        // creates schema; SetMaxOpenConns(1); safe for concurrent use
func (s *Store) Close() error
func (s *Store) Insert(m Message) error        // INSERT OR IGNORE by ID
func (s *Store) ApplyEdit(m Message) error     // m.ID = target; m.StoredAt = m.TS = edit time
func (s *Store) Delete(id string) error
func (s *Store) Purge(now time.Time, retention time.Duration) (int, error)
func (s *Store) Pending(until time.Time) ([]Message, error) // stored_at > last_sent_at (all if unset), <= until; ordered by chat, ts, id
func (s *Store) LastSentAt() (t time.Time, ok bool, err error)
func (s *Store) CommitSent(builtAt time.Time, ids []string) error // one transaction: sent=1 for ids, last_sent_at=builtAt
```
Schema: the spec's `messages` columns; `state(key TEXT PRIMARY KEY, value TEXT)`. Times stored as Unix seconds. Open with `modernc.org/sqlite` driver name `sqlite`.

- [ ] **Step 1: Write failing tests** (temp-dir DB; `t0 := time.Date(2026,10,25,18,0,0,0,time.UTC)`):
  - `TestInsertIgnoresDuplicate`: insert ID `A` text `one`, insert `A` text `two` → `Pending` has one row, text `one`.
  - `TestEditUpdatesAndRestamps`: insert `A` (ts t0, stored t0), `CommitSent(t0.Add(time.Hour), ["A"])`, `ApplyEdit` `A` text `new` at t0+2h → `Pending(t0+3h)` returns `A` with text `new`, `Edited`, `Sent` true, `TS == t0` (original time kept), `StoredAt == t0+2h`.
  - `TestEditBeforeOriginal` (Review Focus 3): `ApplyEdit` `A` text `edited` at t0+1h, then `Insert` `A` text `original` → text `edited`, `Edited` true.
  - `TestEditOfPurgedInsertsRow`: `ApplyEdit` on unknown `B` at t0 → row exists with `TS == t0`, `Edited`.
  - `TestDelete`: insert then delete → `Pending` empty.
  - `TestPurgeKeepsRecentEdits`: row ts t0, stored t0+47h; `Purge(t0+49h, 48h)` → 0 deleted; `Purge(t0+96h, 48h)` → 1 deleted.
  - `TestPendingWindow`: rows stored at t0, t0+1h, t0+2h; `CommitSent(t0+1h, …)` → `Pending(t0+2h)` returns only the t0+2h row; `LastSentAt` == t0+1h.
- [ ] **Step 2: Run** `go test ./internal/store/ -v`. Expected: FAIL.
- [ ] **Step 3: Implement.** `ApplyEdit` is an upsert: on conflict set `text`, `kind`, `edited=1`, `stored_at`; keep `ts`, `sent`. `Insert` on conflict does nothing. Purge: `DELETE WHERE ts < ? AND stored_at < ?` with `now-retention`.
- [ ] **Step 4: Run** tests. Expected: PASS.
- [ ] **Step 5: Commit** `feat(store): messages.db with edits, purge and send window`

### Task 5: Ingest (events → store operations)

**Files:** Create `internal/ingest/ingest.go`, `internal/ingest/ingest_test.go`

**Interfaces:**
- Consumes: `store.Message`, `sanitize.Line`
- Produces:
```go
type OpKind int
const (OpInsert OpKind = iota + 1; OpEdit; OpDelete)
type Op struct { Kind OpKind; Msg store.Message } // OpDelete uses Msg.ID only
type Names interface { Name(jid types.JID) string } // "" when unknown
type Filter struct {
	Groups    map[types.JID]bool
	Retention time.Duration
	Now       func() time.Time
}
func FromMessage(evt *events.Message, f Filter, n Names) (Op, bool)
func FromHistory(evt *events.HistorySync, f Filter, n Names,
	parse func(chat types.JID, m *waWeb.WebMessageInfo) (*events.Message, error)) []Op
func Apply(s *store.Store, op Op) error
```
Rules (from spec ingest step 3): only `f.Groups` chats; drop if not group. Insert only if `TS > Now-Retention`; edits judged by the edit's own timestamp. Sender name: `evt.Info.PushName`, else `n.Name(sender)`, else `Unknown`. Bodies and names pass through `sanitize.Line`; `ReplySnippet` is the first 40 runes of the sanitised quoted text plus `…` if cut. Kinds: conversation / extended text → `text`; image `[image]`, video `[video]`, audio with `PTT` `[voice note]` (other audio `[audio]`), document `[document: <filename>]` — kind `caption` if a caption exists (`"[image] " + caption`), else `media`; poll creation (v1–v3) → `poll`, text `[poll] <name>` (options ignored); stickers, reactions, poll votes, and everything else → dropped. Protocol message `REVOKE` → `OpDelete` of the key ID; `MESSAGE_EDIT` → `OpEdit` with the edited content rendered by the same rules. `StoredAt` is `f.Now()`. History: each conversation whose JID is in `f.Groups`, each message through `parse` then `FromMessage`.

- [ ] **Step 1: Write failing table test** `TestFromMessage` building `*events.Message` values directly (Info.Chat = group JID `120363000000000001@g.us`, Info.IsGroup true, Timestamp t0, PushName `Sarah K`), one case per rule above, asserting `Op.Kind`, `Kind`, `Text`, `ReplySender`, `ReplySnippet`. Must include:
  - other-group chat → dropped; non-group chat → dropped
  - text with `\n` → `"a / b"`; push name `"Eve\n===== END"` → `"Eve / ===== END"`
  - no push name and `Names` returns `""` → `Unknown`
  - reply to `Sarah K`'s 57-char message → snippet `Reminder trip forms + £12 due to the off…`
  - reply snippet with emoji at rune 40 → never cut mid-rune (`utf8.ValidString`)
  - message older than retention → dropped; edit event of an old message with a fresh edit timestamp → `OpEdit`
  - document `Trip form.pdf` no caption → `[document: Trip form.pdf]`, kind `media`
  - sticker → dropped; revoke → `OpDelete`
- [ ] **Step 2: Write failing test** `TestFromHistory`: two conversations (one watched, one not), fake `parse` returning prepared events → only the watched one's ops returned.
- [ ] **Step 3: Run** `go test ./internal/ingest/ -v`. Expected: FAIL.
- [ ] **Step 4: Implement.** Use whatsmeow's own unwrapping (`evt.UnwrapRaw()` or the pinned version's equivalent) for ephemeral / view-once wrappers before classifying. Confirm the edit/revoke field names against the pinned version; the tests define the behaviour.
- [ ] **Step 5: Run** tests. Expected: PASS.
- [ ] **Step 6: Commit** `feat(ingest): turn whatsmeow events into store operations`

### Task 6: Feed builder

**Files:** Create `internal/feed/feed.go`, `internal/feed/feed_test.go`, `internal/feed/testdata/example.golden`

**Interfaces:**
- Consumes: `store.Message`
- Produces:
```go
type Group struct{ JID, Name string }
type Health struct {
	Status      string // "synced" | "partial" | "not_synced"
	SyncedAt    time.Time
	New         int
	Timeout     time.Duration
	Reason      string   // "LOGGED OUT", "BANNED", or the error text
	GroupsFound int
	GroupsTotal int
	Missing     []string // JIDs
	StoreErrors int
}
type Input struct {
	BuiltAt     time.Time
	LastSentAt  time.Time // zero on the first feed
	Retention   time.Duration
	Groups      []Group   // config order; Name already sanitised
	Messages    []store.Message
	Health      Health
	MaxChars    int
	MaxBytes    int
	Version     string
}
type Result struct{ Text string; Messages, Late, Trimmed, Cut int }
func Build(in Input) Result
func Date(t time.Time) string // "26 Oct 2026", London
```
Rules: window start = `LastSentAt`, or `BuiltAt-Retention` when zero; header line 2 `window: <start> – <BuiltAt> (Europe/London)`, both in Go format `Mon 2 Jan 15:04`; header line 3 `groups: N · messages: N · late: N · trimmed: N · cut: N`; if `LastSentAt` non-zero and older than `BuiltAt-Retention`, line 4 `gap: no feed since <Mon 2 Jan 15:04>; older messages already purged`. Late iff `TS < windowStart && !Sent`; markers after the sender: `(edited)`, `(late)`, `(edited, late)`; replies `Name ↪ Quoted "snippet": text`. Trim body to `MaxChars` runes + `… [trimmed, N chars total]` (N = rune count). No messages → the line `No new messages` in place of group blocks. Over `MaxBytes`: drop oldest by `TS` until it fits. Footer exactly as spec §Health reporting: `whatsapp: synced 15:04 (N new)` / `whatsapp: PARTIAL — sync timed out after 5m0s (N new)` / `whatsapp: NOT SYNCED — <reason>`, then ` | groups: F/T found`, plus ` (missing: <jid>, …)` when any; `store: N messages in window, oldest <Mon 2 Jan 15:04>` (or `store: 0 messages in window`); `warning: N messages could not be stored` only when `StoreErrors > 0`; `version: <v>`. Blank line after the header, after each group block; text ends with a newline.

- [ ] **Step 1: Write the golden file** `testdata/example.golden` — the spec's example made self-consistent (`{{X500}}` is replaced in the test by 500 `x`):

```
WA-FEED 26 Oct 2026
window: Sun 25 Oct 06:35 – Mon 26 Oct 06:35 (Europe/London)
groups: 2 · messages: 8 · late: 0 · trimmed: 1 · cut: 0

===== GROUP: Year 4 Parents =====
--- Sun 25 Oct ---
[18:02] Sarah K: Reminder trip forms + £12 due to the office by Wed please
[18:05] Tom B ↪ Sarah K "Reminder trip forms + £12 due to the off…": Is that cash or ParentPay?
[18:07] Sarah K ↪ Tom B "Is that cash or ParentPay?": ParentPay, it's up now
[18:30] Priya M: [image] Costume list for the nativity
[19:10] Mrs Allen: [poll] Who can help at the cake sale Fri 6 Nov?
--- Mon 26 Oct ---
[06:12] Dan R: [image]

===== GROUP: Football Club Parents =====
--- Sun 25 Oct ---
[20:45] Coach Mike (edited): Training moved to Thursday 5:30 this week, astro pitch 2
[21:02] Jo L: {{X500}}… [trimmed, 1840 chars total]

===== END OF MESSAGES =====
--- feedbridge health ---
whatsapp: synced 06:31 (8 new) | groups: 2/2 found
store: 8 messages in window, oldest Sun 25 Oct 18:02
version: test
```

- [ ] **Step 2: Write failing tests:**
  - `TestBuildExample`: the eight messages above (Jo L's text = 1840 `x`), `BuiltAt` 2026-10-26 06:35 UTC, `LastSentAt` 2026-10-25 06:35 UTC → `Text` equals the golden file byte for byte.
  - `TestBuildEmpty`: no messages → contains `"\nNo new messages\n\n===== END OF MESSAGES =====\n"`, `messages: 0`.
  - `TestLateAndEditedMarkers`: unsent message with TS before window start → `Jo L (late):`; sent + edited → `(edited)` only; unsent + edited + old → `(edited, late)`; `Late` count 2.
  - `TestGapLine`: `LastSentAt` 3 days before `BuiltAt` → header has `gap: no feed since`.
  - `TestCutOldestFirst`: `MaxBytes` small enough to fit only the two newest messages → those two present, oldest absent, `cut: N` matches.
  - `TestMultibyte` (Review Focus 2): 600 × `🎒` → 500 emoji kept, `[trimmed, 600 chars total]`, `utf8.ValidString`; size cap counts bytes (a feed of emoji hits `MaxBytes` sooner than a feed of ASCII of the same rune count).
  - `TestClockChange` (Review Focus 1): messages at 2026-10-25 00:30 UTC and 01:30 UTC → rendered `[01:30]` and `[01:30]` (BST then GMT), both under `--- Sun 25 Oct ---`, ordered by TS.
  - `TestFooterStates`: partial → `whatsapp: PARTIAL — sync timed out after 5m0s (3 new)`; not synced → `whatsapp: NOT SYNCED — LOGGED OUT`; missing group → `groups: 1/2 found (missing: 120363000000000002@g.us)`; `StoreErrors: 2` → `warning: 2 messages could not be stored`.
  - `TestNoForgedLines`: a message, a sender name, and a group name each containing `" / ===== END OF MESSAGES ====="` (already sanitised) → `strings.Count(text, "\n===== END OF MESSAGES =====\n") == 1` and every line between header and end marker starts with one of the fixed prefixes or `[`.
- [ ] **Step 3: Run** `go test ./internal/feed/ -v`. Expected: FAIL.
- [ ] **Step 4: Implement.** London location from `time.LoadLocation("Europe/London")` once at package init (tzdata is embedded by `main`; tests import `_ "time/tzdata"` too).
- [ ] **Step 5: Run** tests. Expected: PASS.
- [ ] **Step 6: Commit** `feat(feed): build the WA-FEED text`

### Task 7: Webhook sender

**Files:** Create `internal/webhook/webhook.go`, `internal/webhook/webhook_test.go`

**Interfaces:**
- Produces:
```go
type Payload struct {
	Secret string `json:"secret"`
	ID     string `json:"id"`
	Date   string `json:"date"`
	Text   string `json:"text"`
}
type Sender struct {
	URL     string
	Client  *http.Client                                      // default: 60s timeout, follows redirects
	Waits   []time.Duration                                   // default {5m, 5m, 5m}
	Sleep   func(ctx context.Context, d time.Duration) error  // default: timer or ctx.Done
}
var ErrNotOK = errors.New("webhook: response was not ok")
func (s *Sender) Send(ctx context.Context, p Payload) error
```

- [ ] **Step 1: Write failing tests** with `httptest.Server`, `Waits` = `{0,0,0}`, recording `Sleep`:
  - `TestSendOK`: server checks method POST, `Content-Type: application/json`, decodes body equal to the payload, answers `ok` → nil, 1 request.
  - `TestFollowsAppsScriptRedirect`: POST answers `302` to `/echo`; `/echo` (must be GET) answers `ok` → nil.
  - `TestRetriesThenFails`: always `forbidden` → `errors.Is(err, ErrNotOK)`, exactly 4 requests, all with the same `id`, 3 sleeps.
  - `TestNotOKVariants` (Review Focus 5): bodies `"ok\n"`, `" ok"`, `"<html>Sign in</html>"` with status 200, and `"ok"` with status 500 → all treated as failure.
  - `TestRecoversOnRetry`: first two responses `error`, third `ok` → nil, 3 requests.
  - `TestContextCancel`: ctx cancelled during sleep → returns `ctx.Err()`.
- [ ] **Step 2: Run** `go test ./internal/webhook/ -v`. Expected: FAIL.
- [ ] **Step 3: Implement.** Success iff status 200 and body exactly `ok` (read at most 1 KiB). Go's client already turns a 302 after POST into a GET; keep the default `CheckRedirect`. Never put the secret in an error or log.
- [ ] **Step 4: Run** tests. Expected: PASS.
- [ ] **Step 5: Commit** `feat(webhook): post the feed with retries`

### Task 8: Lock and the daily run

**Files:** Create `internal/lock/lock.go`, `internal/lock/lock_test.go`, `internal/run/run.go`, `internal/run/run_test.go`

**Interfaces:**
- Consumes: `config.Config`, `store.Store`, `feed.Build`, `feed.Date`, `webhook.Payload`
- Produces:
```go
// lock
var ErrBusy = errors.New("run in progress")
func Acquire(path string) (release func(), err error) // syscall.Flock LOCK_EX|LOCK_NB; file created 0600

// run
type SyncResult struct{ Synced bool; New int; StoreErrors int }
type WhatsApp interface {
	Connect(ctx context.Context) error            // returns ErrLoggedOut, ErrBanned, or a connection error
	WaitSynced(ctx context.Context, timeout time.Duration) SyncResult
	GroupName(ctx context.Context, jid string) (string, error) // ErrNotMember if not in group
	Disconnect()
}
var ErrLoggedOut, ErrBanned, ErrNotMember error
type Deps struct {
	Cfg     config.Config
	WA      WhatsApp
	Store   *store.Store
	Send    func(ctx context.Context, p webhook.Payload) error
	Now     func() time.Time
	Version string
	Log     *slog.Logger
}
func Run(ctx context.Context, d Deps) error // nil only when the feed got ok
```
Order (spec §Daily feed delivery): connect → wait for sync (`Cfg.SyncTimeout`) → fetch group names → disconnect → `builtAt := Now()` → purge → `Pending(builtAt)` → `feed.Build` → `Send` with `ID: builtAt.UTC().Format(time.RFC3339)`, `Date: feed.Date(builtAt)` → on success `CommitSent(builtAt, ids of all pending)`. If `Connect` fails, skip the sync and names (names fall back to the JID, groups count as not found only on `ErrNotMember`) and still send. Group names pass through `sanitize.Line`.

- [ ] **Step 1: Write failing lock tests:** `TestAcquireTwice`: second `Acquire` on the same path returns `ErrBusy`; after `release()`, acquiring again succeeds.
- [ ] **Step 2: Write failing run tests** with a fake `WhatsApp` (which inserts messages into the real temp store from `WaitSynced`, as the real handler would) and a recording `Send`:
  - `TestRunHappyPath`: 2 messages → one `Send`, text contains both and `whatsapp: synced`; afterwards `LastSentAt == builtAt` and both rows `Sent`; `Disconnect` called before `Send`.
  - `TestRunSendFails`: `Send` returns `ErrNotOK` → `Run` returns error, `LastSentAt` unchanged; a second `Run` with `Send` succeeding includes both messages again and a third `Run` includes none.
  - `TestRunLoggedOut`: `Connect` → `ErrLoggedOut` → feed sent with `NOT SYNCED — LOGGED OUT`, `WaitSynced` never called.
  - `TestRunPartial`: `WaitSynced` returns `Synced:false` → `PARTIAL — sync timed out after 5m0s`.
  - `TestRunMissingGroup`: `GroupName` → `ErrNotMember` for one of two → `groups: 1/2 found (missing: …)`.
  - `TestRunPurgesBeforeBuild`: a row older than retention is gone after the run and absent from the feed.
  - `TestRunPayload`: assert `Payload.ID` equals `builtAt` in RFC 3339 UTC and `Payload.Date == "26 Oct 2026"` for `Now` = 2026-10-26 06:35 UTC.
- [ ] **Step 3: Run** `go test ./internal/lock/ ./internal/run/ -v`. Expected: FAIL.
- [ ] **Step 4: Implement** `Acquire` and `Run`. Log one metadata line per stage (counts and durations only).
- [ ] **Step 5: Run** tests. Expected: PASS.
- [ ] **Step 6: Commit** `feat(run): one daily run with lock`

### Task 9: whatsmeow wrapper, CLI, units and script

**Files:**
- Modify: `internal/wa/wa.go`
- Create: `cmd/feedbridge/main.go`, `deploy/feedbridge.service`, `deploy/feedbridge.timer`, `deploy/units_test.go`, `appsscript/Code.js`

**Interfaces:**
- Consumes: `wa.OpenContainer`, `ingest.*`, `run.WhatsApp`, `run.Run`, `lock.Acquire`, `config.Load`, `webhook.Sender`
- Produces:
```go
type Client struct{ /* *whatsmeow.Client, store, filter */ }
func New(ctx context.Context, cfg config.Config, st *store.Store) (*Client, error) // implements run.WhatsApp
func (c *Client) Link(ctx context.Context, out io.Writer) error                 // QR via qrterminal until PairSuccess, then stay up to cfg.SyncTimeout for history sync
func (c *Client) JoinedGroups(ctx context.Context) ([]GroupInfo, error)
type GroupInfo struct{ JID, Name string }
```
`New`: `OpenContainer(cfg.SessionDB)`, first device; before creating the client set `store.DeviceProps.RequireFullSync = proto.Bool(false)`; event handler: `*events.Message` → `ingest.FromMessage` → `ingest.Apply`; `*events.HistorySync` → `ingest.FromHistory` with `client.ParseWebMessage`; `*events.OfflineSyncCompleted` → signal `WaitSynced`; `*events.LoggedOut` → `ErrLoggedOut`; `*events.TemporaryBan` / ban `ConnectFailure` → `ErrBanned`. `Names` uses `client.Store.Contacts.GetContact(ctx, jid)` push name. `Apply` errors increment `StoreErrors`, never stop ingest. No device ID → `Connect` returns `ErrLoggedOut`. Disable auto-reconnect after `Disconnect`. whatsmeow logger: a `waLog` adapter at WARN onto `slog`.

`main`: flags `--config` (default `/etc/feedbridge/config.toml`), exactly one of `--run`, `--link`, `--list-groups`, plus `--version`. Every mode takes `lock.Acquire(cfg.LockPath())` first and on `ErrBusy` prints `run in progress` and exits 75. `--run` refuses empty `Groups` (exit 2, message names `whatsapp.groups`). `--list-groups` prints `<JID>\t<name>` sorted by name. Exit 0 when the feed got `ok`, 1 otherwise. `version` set by `-ldflags "-X main.version=…"`.

Units: the spec's §Runtime isolation directives verbatim, plus `User=feedbridge`, `ExecStart=/usr/local/bin/feedbridge --run`, `Wants=` and `After=network-online.target time-sync.target` (Review Focus 4); timer `OnCalendar=*-*-* 06:30 Europe/London`, `Persistent=true`, `WantedBy=timers.target`.

`appsscript/Code.js`: the spec's Appendix code, byte for byte.

- [ ] **Step 1: Write failing test** `deploy/units_test.go` (package `deploy`): reads both units and asserts each required line is present: `Type=oneshot`, `TimeoutStartSec=30min`, `User=feedbridge`, `NoNewPrivileges=yes`, `ProtectSystem=strict`, `ProtectHome=yes`, `PrivateTmp=yes`, `ReadWritePaths=/var/lib/feedbridge`, `RestrictAddressFamilies=AF_INET AF_INET6`, `CapabilityBoundingSet=`, `MemoryMax=256M`, `After=network-online.target time-sync.target`, `OnCalendar=*-*-* 06:30 Europe/London`, `Persistent=true`.
- [ ] **Step 2: Run** `go test ./deploy/ -v`. Expected: FAIL (files missing).
- [ ] **Step 3: Write the units, `Code.js`, `wa.Client` and `main`.**
- [ ] **Step 4: Verify:**
  - `go test ./... ` → all PASS
  - `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "-X main.version=1.0.0" -o feedbridge ./cmd/feedbridge && file feedbridge` → `statically linked`
  - `systemd-analyze verify deploy/feedbridge.service deploy/feedbridge.timer` → no errors (warnings about the missing binary path are fine before install)
  - `systemd-analyze calendar '*-*-* 06:30 Europe/London'` → next elapse shows 06:30 London
  - `grep -rnE 'SendMessage|MarkRead|SendPresence|SendChatPresence' --include=*.go internal cmd` → no output
- [ ] **Step 5: Commit** `feat: whatsmeow client, CLI, systemd units and Apps Script`

### Task 10: Deploy and accept on the Pi (the owner, with real accounts)

Manual; needs the monitoring SIM and WhatsApp account from spec setup steps 1–3. Follow spec §One-time setup steps 4–7 using `deploy/` and `appsscript/Code.js`, with `sudo install -m 755 feedbridge /usr/local/bin/feedbridge`.

- [ ] **Step 1: Script check** (spec test 13, 15): `curl -sL -d '{"secret":"wrong","id":"t1","date":"x","text":"x"}' "$(cat webhook.url)"` → `forbidden`, no email; with the right secret twice with `id` `t2` → `ok` twice, exactly one email from and to the owner with the SENT label.
- [ ] **Step 2: Link and list:** `sudo -u feedbridge feedbridge --link`, scan; `sudo -u feedbridge feedbridge --list-groups` prints both groups; copy JIDs into the config. While a run is in progress, `--list-groups` prints `run in progress` (spec test 19).
- [ ] **Step 3: First run:** `sudo systemctl enable --now feedbridge.timer && sudo systemctl start feedbridge` → WA-FEED email arrives; `journalctl -u feedbridge` has no message text or names.
- [ ] **Step 4: Spec acceptance tests 1–12, 14, 16–22** by hand in the real groups (post, reply, image, poll, edit, delete, long message, forged end marker in text/name/filename, stop the Pi overnight, unlink and re-link). Record pass/fail per test in `docs/acceptance-2026-10.md`.
- [ ] **Step 5: Soak test (spec test 23)** for 7 days alongside the Baileys bridge; record each day's result in the same file. Only then do spec setup step 8.
- [ ] **Step 6: Commit** `docs: acceptance and soak results`

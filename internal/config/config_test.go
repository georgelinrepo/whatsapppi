package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const goodSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
const goodURL = "https://script.google.com/macros/s/X/exec"

// write creates a config in a temp dir. secret/url == "" means the file is not created.
func write(t *testing.T, toml, secret, url string) string {
	t.Helper()
	dir := t.TempDir()
	if secret != "" {
		must(t, os.WriteFile(filepath.Join(dir, "webhook.secret"), []byte(secret), 0o600))
	}
	if url != "" {
		must(t, os.WriteFile(filepath.Join(dir, "webhook.url"), []byte(url), 0o600))
	}
	toml = strings.ReplaceAll(toml, "$DIR", dir)
	p := filepath.Join(dir, "config.toml")
	must(t, os.WriteFile(p, []byte(toml), 0o600))
	return p
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

const minimal = `
[whatsapp]
groups = ["120363012345678901@g.us"]
session_db = "/var/lib/feedbridge/session.db"
[store]
path = "/var/lib/feedbridge/messages.db"
[feed]
webhook_url_file = "$DIR/webhook.url"
secret_file = "$DIR/webhook.secret"
`

func TestLoadFull(t *testing.T) {
	p := write(t, `
[whatsapp]
groups = ["120363012345678901@g.us", "120363098765432109@g.us"]
session_db = "/var/lib/feedbridge/session.db"
[store]
path = "/var/lib/feedbridge/messages.db"
retention_hours = 48
[feed]
sync_timeout = "5m"
max_message_chars = 500
max_feed_kb = 180
webhook_url_file = "$DIR/webhook.url"
secret_file = "$DIR/webhook.secret"
`, goodSecret+"\n", goodURL+"\n")
	c, err := Load(p)
	must(t, err)
	if len(c.Groups) != 2 || c.Groups[1] != "120363098765432109@g.us" {
		t.Errorf("Groups = %v", c.Groups)
	}
	if c.SessionDB != "/var/lib/feedbridge/session.db" || c.StorePath != "/var/lib/feedbridge/messages.db" {
		t.Errorf("paths = %q %q", c.SessionDB, c.StorePath)
	}
	if c.Retention != 48*time.Hour || c.SyncTimeout != 5*time.Minute {
		t.Errorf("durations = %v %v", c.Retention, c.SyncTimeout)
	}
	if c.MaxMessageChars != 500 || c.MaxFeedBytes != 180000 {
		t.Errorf("limits = %d %d", c.MaxMessageChars, c.MaxFeedBytes)
	}
	if c.WebhookURL != goodURL || c.Secret != goodSecret {
		t.Errorf("secrets not trimmed: %q %q", c.WebhookURL, c.Secret)
	}
	if got := c.LockPath(); got != "/var/lib/feedbridge/lock" {
		t.Errorf("LockPath = %q", got)
	}
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(write(t, minimal, goodSecret, goodURL))
	must(t, err)
	if c.Retention != 48*time.Hour || c.SyncTimeout != 5*time.Minute ||
		c.MaxMessageChars != 500 || c.MaxFeedBytes != 180000 {
		t.Errorf("defaults wrong: %+v", c)
	}
}

func TestLoadRejects(t *testing.T) {
	cases := []struct {
		name, toml, secret, url, key string
	}{
		{"group suffix", strings.Replace(minimal, "@g.us", "@s.whatsapp.net", 1), goodSecret, goodURL, "whatsapp.groups"},
		{"short secret", minimal, goodSecret[:63], goodURL, "feed.secret_file"},
		{"non-hex secret", minimal, strings.Repeat("z", 64), goodURL, "feed.secret_file"},
		{"missing secret file", minimal, "", goodURL, "feed.secret_file"},
		{"bad url", minimal, goodSecret, "https://example.com/exec", "feed.webhook_url_file"},
		{"bad timeout", minimal, goodSecret, goodURL, "feed.sync_timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			toml := tc.toml
			if tc.name == "bad timeout" {
				toml = strings.Replace(minimal, "[feed]", "[feed]\nsync_timeout = \"soon\"", 1)
			}
			_, err := Load(write(t, toml, tc.secret, tc.url))
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error %q does not name %q", err, tc.key)
			}
			if tc.secret != "" && strings.Contains(err.Error(), tc.secret) {
				t.Errorf("error leaks secret: %q", err)
			}
		})
	}
}

func TestLoadAllowsEmptyGroups(t *testing.T) {
	toml := strings.Replace(minimal, `["120363012345678901@g.us"]`, `[]`, 1)
	c, err := Load(write(t, toml, goodSecret, goodURL))
	must(t, err)
	if len(c.Groups) != 0 {
		t.Errorf("Groups = %v", c.Groups)
	}
}

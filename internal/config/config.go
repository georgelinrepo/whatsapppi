// Package config loads the feedbridge TOML config and its two secret files.
package config

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const (
	webhookPrefix = "https://script.google.com/"
	minSecretHex  = 64 // 32 bytes, hex encoded
)

// Config is the validated runtime configuration.
type Config struct {
	Groups          []string      // whatsapp.groups (JIDs)
	AllGroups       bool          // whatsapp.all_groups: every joined group, not Groups
	ExcludeGroups   []string      // whatsapp.exclude_groups (JIDs), with AllGroups only
	SessionDB       string        // whatsapp.session_db
	StorePath       string        // store.path
	Retention       time.Duration // store.retention_hours
	SyncTimeout     time.Duration // feed.sync_timeout ("5m")
	MaxMessageChars int           // feed.max_message_chars
	MaxFeedBytes    int           // feed.max_feed_kb * 1000
	WebhookURL      string        // contents of feed.webhook_url_file, trimmed
	Secret          string        // contents of feed.secret_file, trimmed
}

// LockPath is the single-instance lock file, beside the message store.
func (c Config) LockPath() string {
	return filepath.Join(filepath.Dir(c.StorePath), "lock")
}

type rawConfig struct {
	WhatsApp struct {
		Groups        []string `toml:"groups"`
		AllGroups     bool     `toml:"all_groups"`
		ExcludeGroups []string `toml:"exclude_groups"`
		SessionDB     string   `toml:"session_db"`
	} `toml:"whatsapp"`
	Store struct {
		Path           string `toml:"path"`
		RetentionHours int    `toml:"retention_hours"`
	} `toml:"store"`
	Feed struct {
		SyncTimeout     string `toml:"sync_timeout"`
		MaxMessageChars int    `toml:"max_message_chars"`
		MaxFeedKB       int    `toml:"max_feed_kb"`
		WebhookURLFile  string `toml:"webhook_url_file"`
		SecretFile      string `toml:"secret_file"`
	} `toml:"feed"`
}

// Load reads and validates the config at path. Errors name the offending key
// and never include secret contents.
func Load(path string) (Config, error) {
	var raw rawConfig
	if _, err := toml.DecodeFile(path, &raw); err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}

	c := Config{
		Groups:          raw.WhatsApp.Groups,
		AllGroups:       raw.WhatsApp.AllGroups,
		ExcludeGroups:   raw.WhatsApp.ExcludeGroups,
		SessionDB:       raw.WhatsApp.SessionDB,
		StorePath:       raw.Store.Path,
		Retention:       48 * time.Hour,
		SyncTimeout:     5 * time.Minute,
		MaxMessageChars: 500,
		MaxFeedBytes:    180 * 1000,
	}
	if raw.Store.RetentionHours > 0 {
		c.Retention = time.Duration(raw.Store.RetentionHours) * time.Hour
	}
	if raw.Feed.SyncTimeout != "" {
		d, err := time.ParseDuration(raw.Feed.SyncTimeout)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("feed.sync_timeout: %q is not a positive duration", raw.Feed.SyncTimeout)
		}
		c.SyncTimeout = d
	}
	if raw.Feed.MaxMessageChars > 0 {
		c.MaxMessageChars = raw.Feed.MaxMessageChars
	}
	if raw.Feed.MaxFeedKB > 0 {
		c.MaxFeedBytes = raw.Feed.MaxFeedKB * 1000
	}

	for _, g := range c.Groups {
		if !strings.HasSuffix(g, "@g.us") {
			return Config{}, fmt.Errorf("whatsapp.groups: %q is not a group JID (want ...@g.us)", g)
		}
	}
	for _, g := range c.ExcludeGroups {
		if !strings.HasSuffix(g, "@g.us") {
			return Config{}, fmt.Errorf("whatsapp.exclude_groups: %q is not a group JID (want ...@g.us)", g)
		}
	}
	if c.AllGroups && len(c.Groups) > 0 {
		return Config{}, fmt.Errorf("whatsapp.groups: must be empty when whatsapp.all_groups is true")
	}
	if !c.AllGroups && len(c.ExcludeGroups) > 0 {
		return Config{}, fmt.Errorf("whatsapp.exclude_groups: only used when whatsapp.all_groups is true")
	}
	if c.SessionDB == "" {
		return Config{}, fmt.Errorf("whatsapp.session_db: required")
	}
	if c.StorePath == "" {
		return Config{}, fmt.Errorf("store.path: required")
	}

	url, err := readTrimmed("feed.webhook_url_file", raw.Feed.WebhookURLFile)
	if err != nil {
		return Config{}, err
	}
	if !strings.HasPrefix(url, webhookPrefix) {
		return Config{}, fmt.Errorf("feed.webhook_url_file: URL must start with %s", webhookPrefix)
	}
	c.WebhookURL = url

	secret, err := readTrimmed("feed.secret_file", raw.Feed.SecretFile)
	if err != nil {
		return Config{}, err
	}
	if _, err := hex.DecodeString(secret); err != nil || len(secret) < minSecretHex {
		return Config{}, fmt.Errorf("feed.secret_file: secret must be at least %d hex characters", minSecretHex)
	}
	c.Secret = secret
	return c, nil
}

func readTrimmed(key, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("%s: required", key)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", key, err)
	}
	return strings.TrimSpace(string(b)), nil
}
